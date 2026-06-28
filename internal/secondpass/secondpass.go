// Package secondpass runs a stronger DLP scan on captured traffic off the
// hot path, flagging false positives (fast-layer alerts the engine doesn't
// confirm) and false negatives (secrets the fast layer missed). It is off by
// default and never touches the request path.
package secondpass

import (
	"context"
	"encoding/json"
	"log/slog"
	"sync"
	"time"

	"github.com/rromenskyi/ipsupport-airllm/internal/dlp"
)

// Engine scans text for sensitive spans using a stronger detector.
type Engine interface {
	Scan(ctx context.Context, text string) ([]dlp.Finding, error)
}

// llmFinding is the raw parse target from LLM JSON output.
type llmFinding struct {
	Label string  `json:"label"`
	Start int     `json:"start"`
	End   int     `json:"end"`
	Score float64 `json:"score"`
}

// LLMEngine is an Engine backed by an injected LLM chat function. The Chat
// closure is provided by the httpapi layer so this package stays free of
// provider dependencies.
type LLMEngine struct {
	// Chat calls an LLM with the given prompt and returns raw text (expected JSON).
	Chat func(ctx context.Context, prompt string) (rawJSON string, err error)
	// MinScore is called on each Scan to read the current threshold, so a
	// config change via PUT /api/admin/secondpass takes effect without restart.
	MinScore func() float64
}

// scanInstruction is prepended to the user text before calling Chat.
const scanInstruction = `Scan the following text for sensitive data (API keys, tokens, credentials, PII). ` +
	`Return ONLY a JSON array. Each element must be an object with fields: ` +
	`"label" (string type name), "start" (int byte offset), "end" (int byte offset), "score" (float 0.0-1.0). ` +
	`Return [] if nothing is found. No explanation, no markdown fences, no prose.

TEXT:
`

// Scan calls the LLM and parses its JSON output into DLP findings.
// Malformed output results in no findings (fail-safe, no crash).
func (e *LLMEngine) Scan(ctx context.Context, text string) ([]dlp.Finding, error) {
	raw, err := e.Chat(ctx, scanInstruction+text)
	if err != nil {
		return nil, err
	}
	return parseFindings(raw, e.MinScore()), nil
}

// parseFindings extracts dlp.Finding values from an LLM JSON array, filtering
// by minScore and dropping degenerate spans (end <= start). Any parse error
// returns nil (no crash).
func parseFindings(raw string, minScore float64) []dlp.Finding {
	var items []llmFinding
	if err := json.Unmarshal([]byte(raw), &items); err != nil {
		slog.Debug("secondpass: malformed LLM output", "err", err)
		return nil
	}
	var out []dlp.Finding
	for _, f := range items {
		if f.Score < minScore {
			continue
		}
		if f.End <= f.Start {
			continue
		}
		out = append(out, dlp.Finding{Label: f.Label, Start: f.Start, End: f.End})
	}
	return out
}

// PendingRow is the subset of capture_index the Job needs.
type PendingRow struct {
	ID       string
	BlobKey  string
	Detected []dlp.Finding
}

// Store is the secondpass view of the capture index.
type Store interface {
	PendingForSecondPass(ctx context.Context, limit int) ([]PendingRow, error)
	UpdateSecondPass(ctx context.Context, id, status string, labels []dlp.Finding) error
}

// WebhookSender fires a webhook event. The implementation looks up endpoints
// and delivers the payload; secondpass never touches the webhooks table directly.
type WebhookSender func(ctx context.Context, event string, payload []byte)

// Job processes pending captures in the background using a stronger engine.
type Job struct {
	store     Store
	readBody  func(ctx context.Context, blobKey string) ([]byte, error)
	engine    Engine
	sendHook  WebhookSender // nil = no webhooks
	batchSize int

	stopCh    chan struct{}
	doneCh    chan struct{}
	startOnce sync.Once
	stopOnce  sync.Once
}

// NewJob creates a Job. batchSize <= 0 defaults to 50.
func NewJob(
	store Store,
	readBody func(ctx context.Context, blobKey string) ([]byte, error),
	engine Engine,
	sendHook WebhookSender,
	batchSize int,
) *Job {
	if batchSize <= 0 {
		batchSize = 50
	}
	return &Job{
		store:     store,
		readBody:  readBody,
		engine:    engine,
		sendHook:  sendHook,
		batchSize: batchSize,
		stopCh:    make(chan struct{}),
		doneCh:    make(chan struct{}),
	}
}

// Start launches the background ticker. interval <= 0 defaults to 5 minutes.
// Calling Start more than once is a no-op (guarded by sync.Once).
func (j *Job) Start(ctx context.Context, interval time.Duration) {
	if interval <= 0 {
		interval = 5 * time.Minute
	}
	j.startOnce.Do(func() {
		go func() {
			defer close(j.doneCh)
			ticker := time.NewTicker(interval)
			defer ticker.Stop()
			for {
				select {
				case <-j.stopCh:
					return
				case <-ctx.Done():
					return
				case <-ticker.C:
					j.RunOnce(ctx)
				}
			}
		}()
	})
}

// Stop signals the background goroutine and waits for it to exit.
// Calling Stop more than once is a no-op (guarded by sync.Once).
func (j *Job) Stop() {
	j.stopOnce.Do(func() {
		close(j.stopCh)
		<-j.doneCh
	})
}

// RunOnce processes one batch of pending captures. It is also called directly
// in tests.
func (j *Job) RunOnce(ctx context.Context) {
	rows, err := j.store.PendingForSecondPass(ctx, j.batchSize)
	if err != nil {
		slog.Error("secondpass: list pending failed", "err", err)
		return
	}
	for _, row := range rows {
		j.processOne(ctx, row)
	}
}

func (j *Job) processOne(ctx context.Context, row PendingRow) {
	body, err := j.readBody(ctx, row.BlobKey)
	if err != nil {
		slog.Error("secondpass: read body failed", "id", row.ID, "err", err)
		return // leave pending for retry
	}

	engineFindings, err := j.engine.Scan(ctx, string(body))
	if err != nil {
		slog.Error("secondpass: engine scan failed", "id", row.ID, "err", err)
		return // leave pending for retry
	}

	status, falsePositives, misses := diff(row.Detected, engineFindings)

	if err := j.store.UpdateSecondPass(ctx, row.ID, status, engineFindings); err != nil {
		slog.Error("secondpass: update failed", "id", row.ID, "err", err)
		return
	}

	if j.sendHook == nil {
		return
	}

	if len(misses) > 0 {
		payload, _ := json.Marshal(map[string]any{
			"event":       "dlp.false_negative",
			"capture_id":  row.ID,
			"miss_count":  len(misses),
			"miss_labels": dlp.Labels(misses),
		})
		j.sendHook(ctx, "dlp.false_negative", payload)
	} else if len(falsePositives) > 0 {
		// Only fire alert_cleared when there are no misses (pure FP case).
		payload, _ := json.Marshal(map[string]any{
			"event":         "dlp.alert_cleared",
			"capture_id":    row.ID,
			"cleared_count": len(falsePositives),
		})
		j.sendHook(ctx, "dlp.alert_cleared", payload)
	}
}

// diff compares fast-layer detections with engine findings and returns:
//   - status: "clean" | "confirmed" | "false_positive" | "false_negative"
//     (false_negative is preferred when both FP and FN occur)
//   - falsePositives: indices into detected whose spans were not confirmed
//   - misses: engine findings not covered by any detected span
func diff(detected, engine []dlp.Finding) (status string, falsePositives []int, misses []dlp.Finding) {
	for i, d := range detected {
		if !anyOverlap(d, engine) {
			falsePositives = append(falsePositives, i)
		}
	}
	for _, e := range engine {
		if !anyOverlap(e, detected) {
			misses = append(misses, e)
		}
	}

	hasFP := len(falsePositives) > 0
	hasFN := len(misses) > 0
	switch {
	case hasFN:
		return "false_negative", falsePositives, misses
	case hasFP:
		return "false_positive", falsePositives, misses
	case len(detected) > 0:
		return "confirmed", nil, nil
	default:
		return "clean", nil, nil
	}
}

// anyOverlap reports whether f overlaps any finding in others.
func anyOverlap(f dlp.Finding, others []dlp.Finding) bool {
	for _, o := range others {
		if f.Start < o.End && o.Start < f.End {
			return true
		}
	}
	return false
}
