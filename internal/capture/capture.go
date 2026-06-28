// Package capture records gateway traffic to encrypted blob storage and a
// Postgres index. The pipeline is non-blocking: Enqueue returns immediately;
// records are dropped (with a counter) when the buffer is full.
package capture

import (
	"context"
	"log/slog"
	"math/rand/v2"
	"sync"
	"sync/atomic"
	"time"

	"github.com/rromenskyi/ipsupport-airllm/internal/blob"
	"github.com/rromenskyi/ipsupport-airllm/internal/dlp"
	"github.com/rromenskyi/ipsupport-airllm/internal/secrets"
)

// chanSize is the capacity of the internal work channel.
const chanSize = 1024

// Config is the runtime capture policy, read once per enqueue.
type Config struct {
	Enabled       bool
	SampleRate    float64 // fraction of non-incident traffic to capture [0,1]
	Redact        bool
	RetentionDays int
}

// Record is a single request/response to be captured.
type Record struct {
	KeyID            string
	UserID           string
	Ingress          string
	Alias            string
	Provider         string
	UpstreamModel    string
	Status           int
	PromptTokens     int
	CompletionTokens int
	CostUSD          float64
	ModelVersion     string
	Detected         []dlp.Finding
	Body             []byte // plain (possibly redacted) content
	HadIncident      bool
	Redacted         bool // snapshot of capture config Redact at enqueue time
}

// Inserter writes and queries the capture_index.
type Inserter interface {
	Insert(ctx context.Context, row IndexRow) error
	ListExpired(ctx context.Context, before time.Time) ([]IndexRow, error)
	DeleteByID(ctx context.Context, id string) error
}

// Pipeline manages the worker pool and the buffered work channel.
type Pipeline struct {
	bs      blob.Store
	idx     Inserter
	sealer  *secrets.Sealer
	cfg     func() Config
	ch      chan Record
	dropped atomic.Int64
	stopped atomic.Bool
	wg      sync.WaitGroup
	sweepWG sync.WaitGroup
	stopCh  chan struct{}
}

// NewPipeline constructs a Pipeline. Call Start to activate workers.
func NewPipeline(bs blob.Store, idx Inserter, sealer *secrets.Sealer, cfg func() Config) *Pipeline {
	return &Pipeline{
		bs:     bs,
		idx:    idx,
		sealer: sealer,
		cfg:    cfg,
		ch:     make(chan Record, chanSize),
		stopCh: make(chan struct{}),
	}
}

// Start launches n worker goroutines and the retention sweeper.
func (p *Pipeline) Start(workers int) {
	for i := 0; i < workers; i++ {
		p.wg.Add(1)
		go func() {
			defer p.wg.Done()
			for r := range p.ch {
				p.process(r)
			}
		}()
	}
	p.sweepWG.Add(1)
	go func() {
		defer p.sweepWG.Done()
		ticker := time.NewTicker(time.Hour)
		defer ticker.Stop()
		for {
			select {
			case <-p.stopCh:
				return
			case t := <-ticker.C:
				cfg := p.cfg()
				days := cfg.RetentionDays
				if days <= 0 {
					days = 30
				}
				p.sweep(context.Background(), t, days)
			}
		}
	}()
}

// Stop signals the sweeper, drains the work channel, and waits for all
// goroutines to finish. After Stop returns, Enqueue is a safe no-op.
func (p *Pipeline) Stop() {
	p.stopped.Store(true) // must happen before close(p.ch) to guard Enqueue
	close(p.stopCh)
	close(p.ch)
	p.wg.Wait()
	p.sweepWG.Wait()
}

// Enqueue submits a record for capture. It never blocks; if the buffer is
// full the record is dropped and the dropped counter is incremented. Enqueue
// is a safe no-op after Stop.
func (p *Pipeline) Enqueue(r Record) {
	if p.stopped.Load() {
		return
	}
	cfg := p.cfg()
	if !cfg.Enabled {
		return
	}
	if !r.HadIncident && rand.Float64() >= cfg.SampleRate {
		return
	}
	select {
	case p.ch <- r:
	default:
		p.dropped.Add(1)
	}
}

// Dropped returns the total number of records dropped due to a full buffer.
func (p *Pipeline) Dropped() int64 { return p.dropped.Load() }

// process seals the body and writes blob + index row for one record.
func (p *Pipeline) process(r Record) {
	id := newID()
	blobKey := "captures/" + id

	sealed, err := p.sealer.Seal(r.Body)
	if err != nil {
		slog.Error("capture: seal body failed", "err", err)
		return
	}
	if err := p.bs.Put(context.Background(), blobKey, sealed); err != nil {
		slog.Error("capture: blob put failed", "err", err, "key", blobKey)
		return
	}

	row := IndexRow{
		ID:               id,
		TS:               time.Now().UTC(),
		KeyID:            r.KeyID,
		UserID:           r.UserID,
		IngressProtocol:  r.Ingress,
		Alias:            r.Alias,
		ProviderName:     r.Provider,
		UpstreamModel:    r.UpstreamModel,
		Status:           r.Status,
		PromptTokens:     int64(r.PromptTokens),
		CompletionTokens: int64(r.CompletionTokens),
		CostUSD:          r.CostUSD,
		BlobKey:          blobKey,
		Redacted:         r.Redacted,
		ModelVersion:     r.ModelVersion,
		Detected:         r.Detected,
		ReviewStatus:     "unreviewed",
		SecondpassStatus: "pending",
	}

	if err := p.idx.Insert(context.Background(), row); err != nil {
		slog.Error("capture: index insert failed", "err", err)
		// Best-effort: blob is orphaned but we don't fail the caller.
	}
}

// sweep deletes index rows and blobs that are older than retentionDays.
func (p *Pipeline) sweep(ctx context.Context, now time.Time, retentionDays int) {
	before := now.AddDate(0, 0, -retentionDays)
	rows, err := p.idx.ListExpired(ctx, before)
	if err != nil {
		slog.Error("capture sweep: list expired failed", "err", err)
		return
	}
	for _, row := range rows {
		if row.BlobKey != "" {
			if err := p.bs.Delete(ctx, row.BlobKey); err != nil {
				slog.Warn("capture sweep: blob delete failed", "key", row.BlobKey, "err", err)
			}
		}
		if err := p.idx.DeleteByID(ctx, row.ID); err != nil {
			slog.Error("capture sweep: index delete failed", "id", row.ID, "err", err)
		}
	}
}
