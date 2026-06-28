package capture

import (
	"bytes"
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/rromenskyi/ipsupport-airllm/internal/dlp"
	"github.com/rromenskyi/ipsupport-airllm/internal/secrets"
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
