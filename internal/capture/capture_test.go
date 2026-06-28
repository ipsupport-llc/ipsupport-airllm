package capture

import (
	"bytes"
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/ipsupport-llc/ipsupport-airllm/internal/dlp"
	"github.com/ipsupport-llc/ipsupport-airllm/internal/secrets"
)

// memBlob is a simple in-memory blob.Store for testing.
type memBlob struct {
	mu   sync.Mutex
	objs map[string][]byte
}

func newMemBlob() *memBlob { return &memBlob{objs: map[string][]byte{}} }

func (m *memBlob) Put(_ context.Context, key string, data []byte) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	cp := make([]byte, len(data))
	copy(cp, data)
	m.objs[key] = cp
	return nil
}

func (m *memBlob) Get(_ context.Context, key string) ([]byte, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	d, ok := m.objs[key]
	if !ok {
		return nil, fmt.Errorf("not found: %s", key)
	}
	return d, nil
}

func (m *memBlob) Delete(_ context.Context, key string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.objs[key]; !ok {
		return fmt.Errorf("not found: %s", key)
	}
	delete(m.objs, key)
	return nil
}

// fakeInserter records inserted rows.
type fakeInserter struct {
	mu      sync.Mutex
	rows    []IndexRow
	deleted []string
}

func (f *fakeInserter) Insert(_ context.Context, row IndexRow) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.rows = append(f.rows, row)
	return nil
}

func (f *fakeInserter) ListExpired(_ context.Context, before time.Time) ([]IndexRow, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []IndexRow
	for _, r := range f.rows {
		if r.TS.Before(before) {
			out = append(out, r)
		}
	}
	return out, nil
}

func (f *fakeInserter) DeleteByID(_ context.Context, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.deleted = append(f.deleted, id)
	newRows := f.rows[:0]
	for _, r := range f.rows {
		if r.ID != id {
			newRows = append(newRows, r)
		}
	}
	f.rows = newRows
	return nil
}

func (f *fakeInserter) ListExpiredRaw(_ context.Context, before time.Time) ([]IndexRow, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []IndexRow
	for _, r := range f.rows {
		if r.RawBlobKey != "" && r.RawExpiresAt != nil && r.RawExpiresAt.Before(before) {
			out = append(out, IndexRow{ID: r.ID, RawBlobKey: r.RawBlobKey})
		}
	}
	return out, nil
}

func (f *fakeInserter) ClearRaw(_ context.Context, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i := range f.rows {
		if f.rows[i].ID == id {
			f.rows[i].RawBlobKey = ""
			f.rows[i].RawExpiresAt = nil
		}
	}
	return nil
}

// TestPipeline_RawTrainingWindow verifies that a record carrying a RawBody
// writes a second (un-redacted) blob and stamps raw_blob_key + raw_expires_at.
func TestPipeline_RawTrainingWindow(t *testing.T) {
	bs := newMemBlob()
	idx := &fakeInserter{}
	sealer := testSealer(t)
	p := NewPipeline(bs, idx, sealer, func() Config {
		return Config{Enabled: true, SampleRate: 1, Redact: true, RawTraining: true, RawTTLHours: 1}
	})
	p.Start(1)
	p.Enqueue(Record{Ingress: "openai", Body: []byte("[REDACTED:key]"), RawBody: []byte("raw-secret"), Status: 200})
	p.Stop()

	if len(idx.rows) != 1 {
		t.Fatalf("expected 1 index row, got %d", len(idx.rows))
	}
	row := idx.rows[0]
	if row.RawBlobKey == "" || row.RawExpiresAt == nil {
		t.Fatalf("expected raw_blob_key + raw_expires_at set, got %q / %v", row.RawBlobKey, row.RawExpiresAt)
	}
	if len(bs.objs) != 2 {
		t.Fatalf("expected 2 blobs (main + raw), got %d", len(bs.objs))
	}
	sealedRaw, err := bs.Get(context.Background(), row.RawBlobKey)
	if err != nil {
		t.Fatalf("raw blob missing: %v", err)
	}
	plain, err := sealer.Open(sealedRaw)
	if err != nil {
		t.Fatalf("raw blob open: %v", err)
	}
	if string(plain) != "raw-secret" {
		t.Fatalf("raw blob = %q, want raw-secret", plain)
	}
}

// TestPipeline_NoRawWhenDisabled verifies no raw blob is written without RawBody.
func TestPipeline_NoRawWhenDisabled(t *testing.T) {
	bs := newMemBlob()
	idx := &fakeInserter{}
	p := NewPipeline(bs, idx, testSealer(t), func() Config {
		return Config{Enabled: true, SampleRate: 1, Redact: true}
	})
	p.Start(1)
	p.Enqueue(Record{Ingress: "openai", Body: []byte("hi"), Status: 200})
	p.Stop()

	if len(bs.objs) != 1 {
		t.Fatalf("expected 1 blob (no raw), got %d", len(bs.objs))
	}
	if idx.rows[0].RawBlobKey != "" {
		t.Fatalf("expected empty raw_blob_key, got %q", idx.rows[0].RawBlobKey)
	}
}

// TestPipeline_SweepRaw verifies the raw sweeper deletes expired raw blobs and
// clears their pointers while leaving the main row intact.
func TestPipeline_SweepRaw(t *testing.T) {
	bs := newMemBlob()
	idx := &fakeInserter{}
	p := NewPipeline(bs, idx, testSealer(t), func() Config { return Config{} })

	rawKey := "captures-raw/x"
	_ = bs.Put(context.Background(), rawKey, []byte("sealed"))
	past := time.Now().Add(-time.Hour).UTC()
	idx.rows = append(idx.rows, IndexRow{ID: "x", BlobKey: "captures/x", RawBlobKey: rawKey, RawExpiresAt: &past})

	p.sweepRaw(context.Background(), time.Now())

	if _, err := bs.Get(context.Background(), rawKey); err == nil {
		t.Error("expected expired raw blob to be deleted")
	}
	if idx.rows[0].RawBlobKey != "" || idx.rows[0].RawExpiresAt != nil {
		t.Error("expected raw pointers cleared after sweep")
	}
	if idx.rows[0].ID != "x" {
		t.Error("main row must survive the raw sweep")
	}
}

// TestPipeline_MainSweepDeletesRawBlob verifies the retention sweep deletes a
// row's un-redacted raw copy along with the row, so an expired row never
// orphans a secret blob.
func TestPipeline_MainSweepDeletesRawBlob(t *testing.T) {
	bs := newMemBlob()
	idx := &fakeInserter{}
	p := NewPipeline(bs, idx, testSealer(t), func() Config { return Config{} })

	_ = bs.Put(context.Background(), "captures/x", []byte("main"))
	_ = bs.Put(context.Background(), "captures-raw/x", []byte("raw-secret"))
	old := time.Now().Add(-48 * time.Hour)
	idx.rows = append(idx.rows, IndexRow{ID: "x", TS: old, BlobKey: "captures/x", RawBlobKey: "captures-raw/x"})

	p.sweep(context.Background(), time.Now(), 1) // retention 1 day; row is 2 days old

	if _, err := bs.Get(context.Background(), "captures-raw/x"); err == nil {
		t.Error("retention sweep must delete the raw blob (no orphaned secret)")
	}
	if _, err := bs.Get(context.Background(), "captures/x"); err == nil {
		t.Error("retention sweep must delete the main blob")
	}
}

func testSealer(t *testing.T) *secrets.Sealer {
	t.Helper()
	key := make([]byte, 32)
	for i := range key {
		key[i] = byte(i + 1)
	}
	s, err := secrets.New(key)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// TestPipelineCaptures verifies that a record is written to blob + index.
func TestPipelineCaptures(t *testing.T) {
	bs := newMemBlob()
	idx := &fakeInserter{}
	sealer := testSealer(t)
	p := NewPipeline(bs, idx, sealer, func() Config {
		return Config{Enabled: true, SampleRate: 1, Redact: true}
	})
	p.Start(2)
	p.Enqueue(Record{Ingress: "openai", Body: []byte("hi"), Status: 200})
	p.Stop()

	if len(idx.rows) != 1 {
		t.Fatalf("expected 1 index row, got %d", len(idx.rows))
	}
	if len(bs.objs) != 1 {
		t.Fatalf("expected 1 blob, got %d", len(bs.objs))
	}
	// Body must be sealed (ciphertext != plaintext).
	for _, v := range bs.objs {
		if bytes.Equal(v, []byte("hi")) {
			t.Error("blob body must be sealed, not plaintext")
		}
		// Verify it decrypts back.
		plain, err := sealer.Open(v)
		if err != nil {
			t.Fatalf("blob is not valid ciphertext: %v", err)
		}
		if string(plain) != "hi" {
			t.Errorf("decrypted body mismatch: %q", plain)
		}
	}
}

// TestPipelineDisabled verifies that nothing is captured when disabled.
func TestPipelineDisabled(t *testing.T) {
	bs := newMemBlob()
	idx := &fakeInserter{}
	p := NewPipeline(bs, idx, testSealer(t), func() Config {
		return Config{Enabled: false, SampleRate: 1}
	})
	p.Start(1)
	p.Enqueue(Record{Ingress: "openai", Body: []byte("hi"), Status: 200})
	p.Stop()

	if len(idx.rows) != 0 || len(bs.objs) != 0 {
		t.Fatalf("disabled: expected nothing captured, rows=%d objs=%d", len(idx.rows), len(bs.objs))
	}
}

// TestPipelineCapturesIncidentAtZeroSampleRate verifies that a DLP incident
// is always captured even when SampleRate=0.
func TestPipelineCapturesIncidentAtZeroSampleRate(t *testing.T) {
	bs := newMemBlob()
	idx := &fakeInserter{}
	p := NewPipeline(bs, idx, testSealer(t), func() Config {
		return Config{Enabled: true, SampleRate: 0, Redact: true}
	})
	p.Start(1)
	p.Enqueue(Record{
		Ingress:     "openai",
		Body:        []byte("secret"),
		Status:      200,
		HadIncident: true,
		Detected:    []dlp.Finding{{Label: "openai_key", Start: 0, End: 6}},
	})
	p.Stop()

	if len(idx.rows) != 1 {
		t.Fatalf("incident must be captured at SampleRate=0, rows=%d", len(idx.rows))
	}
}

// TestPipelineDropCounter verifies that Enqueue drops gracefully when buffer
// is full and increments the dropped counter.
func TestPipelineDropCounter(t *testing.T) {
	bs := newMemBlob()
	idx := &fakeInserter{}
	// Pipeline not started (no workers), buffer size tiny = 0 equivalent via blocking.
	// Use a blocking inserter to hold workers and fill the channel.
	p := NewPipeline(bs, idx, testSealer(t), func() Config {
		return Config{Enabled: true, SampleRate: 1}
	})
	// Do not start workers; directly overflow the channel.
	for i := 0; i < chanSize+10; i++ {
		p.Enqueue(Record{Body: []byte("x")})
	}
	if p.Dropped() == 0 {
		t.Error("expected non-zero dropped counter after overflow")
	}
}

// TestEnqueueAfterStop verifies that Enqueue after Stop is a safe no-op and
// does not panic with "send on closed channel".
func TestEnqueueAfterStop(t *testing.T) {
	bs := newMemBlob()
	idx := &fakeInserter{}
	p := NewPipeline(bs, idx, testSealer(t), func() Config {
		return Config{Enabled: true, SampleRate: 1}
	})
	p.Start(1)
	p.Stop()
	// Must not panic; the stopped guard must catch it before reaching the channel.
	p.Enqueue(Record{Body: []byte("after-stop")})
	if p.Dropped() != 0 {
		t.Error("post-stop Enqueue must not count as a drop (early return)")
	}
}

// TestSweepDeletesExpiredRows verifies sweep removes expired rows + their blobs.
func TestSweepDeletesExpiredRows(t *testing.T) {
	bs := newMemBlob()
	idx := &fakeInserter{}
	sealer := testSealer(t)
	p := NewPipeline(bs, idx, sealer, func() Config {
		return Config{Enabled: true, SampleRate: 1, Redact: true, RetentionDays: 30}
	})

	// Pre-populate index with an expired row and a fresh row.
	oldTime := time.Now().Add(-31 * 24 * time.Hour)
	freshTime := time.Now().Add(-1 * time.Hour)

	_ = bs.Put(context.Background(), "captures/old-id", []byte("old"))
	_ = bs.Put(context.Background(), "captures/new-id", []byte("new"))
	idx.rows = []IndexRow{
		{ID: "old-id", TS: oldTime, BlobKey: "captures/old-id"},
		{ID: "new-id", TS: freshTime, BlobKey: "captures/new-id"},
	}

	p.sweep(context.Background(), time.Now(), 30)

	if len(idx.rows) != 1 {
		t.Fatalf("expected 1 remaining row after sweep, got %d", len(idx.rows))
	}
	if idx.rows[0].ID != "new-id" {
		t.Errorf("expected new-id to survive, got %s", idx.rows[0].ID)
	}
	if _, ok := bs.objs["captures/old-id"]; ok {
		t.Error("old blob must be deleted by sweep")
	}
	if _, ok := bs.objs["captures/new-id"]; !ok {
		t.Error("new blob must survive sweep")
	}
}
