package secondpass

import (
	"context"
	"testing"

	"github.com/ipsupport-llc/ipsupport-airllm/internal/dlp"
)

// TestJob_DisabledReadsNothing: a disabled job must not list rows, read or
// decrypt bodies, or call the engine — before, it did all of that every tick
// and logged one engine error per row.
func TestJob_DisabledReadsNothing(t *testing.T) {
	store := newFakeStore(PendingRow{ID: "id1", BlobKey: "k1", Detected: []dlp.Finding{{Label: "key", Start: 0, End: 5}}})
	reads := 0
	readBody := func(context.Context, string) ([]byte, error) { reads++; return []byte("hello"), nil }
	job := NewJob(store, readBody, &fakeEngine{}, nil, 10, nil)
	enabled := false
	job.SetEnabled(func() bool { return enabled })

	job.RunOnce(context.Background())
	if reads != 0 || len(store.updates) != 0 {
		t.Fatalf("disabled job read %d bodies and made %d updates", reads, len(store.updates))
	}

	enabled = true
	job.RunOnce(context.Background())
	if reads != 1 || len(store.updates) != 1 {
		t.Fatalf("enabled job: %d reads, %d updates, want 1 and 1", reads, len(store.updates))
	}
}
