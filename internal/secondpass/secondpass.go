// Package secondpass runs a stronger DLP scan on captured traffic off the
// hot path, flagging false positives (fast-layer alerts the engine doesn't
// confirm) and false negatives (secrets the fast layer missed). It is off by
// default and never touches the request path.
package secondpass

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/ipsupport-llc/ipsupport-airllm/internal/dlp"
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
// Malformed output is a scan error (processOne leaves the row pending for
// retry), never a confident "zero findings" — conflating the two would let
// an LLM that merely failed to format its output clear a genuinely
// confirmed secret as a false positive.
func (e *LLMEngine) Scan(ctx context.Context, text string) ([]dlp.Finding, error) {
	raw, err := e.Chat(ctx, scanInstruction+text)
	if err != nil {
		return nil, err
	}
	findings, err := parseFindings(raw, e.MinScore())
	if err != nil {
		return nil, fmt.Errorf("secondpass: malformed LLM output: %w", err)
	}
	return findings, nil
}

// parseFindings extracts dlp.Finding values from an LLM JSON array, filtering
// by minScore and dropping degenerate spans (end <= start).
func parseFindings(raw string, minScore float64) ([]dlp.Finding, error) {
	var items []llmFinding
	if err := json.Unmarshal([]byte(raw), &items); err != nil {
		return nil, err
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
	return out, nil
}

// PendingRow is the subset of capture_index the Job needs.
type PendingRow struct {
	ID           string
	BlobKey      string
	Detected     []dlp.Finding
	Redacted     bool       // snapshot of capture config Redact at enqueue time
	RawBlobKey   string     // un-redacted training copy; "" when none
	RawExpiresAt *time.Time // TTL of the raw copy; nil when none
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
	// allowRaw is called on each row to read the current policy, so a config
	// change via PUT /api/admin/secondpass takes effect without restart. nil
	// or a false result means never use the raw (un-redacted) window — the
	// engine runs through an operator-configured model alias, which may be a
	// third-party provider, so sending un-redacted secrets there must be an
	// explicit opt-in, not a side effect of enabling raw_training for
	// byte-alignment accuracy.
	allowRaw func() bool

	stopCh    chan struct{}
	doneCh    chan struct{}
	startOnce sync.Once
	stopOnce  sync.Once
}

// NewJob creates a Job. batchSize <= 0 defaults to 50. A nil allowRaw means
// the raw (un-redacted) window is never sent to the engine.
func NewJob(
	store Store,
	readBody func(ctx context.Context, blobKey string) ([]byte, error),
	engine Engine,
	sendHook WebhookSender,
	batchSize int,
	allowRaw func() bool,
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
		allowRaw:  allowRaw,
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
	// Prefer the un-redacted raw-window copy when it exists and has not expired:
	// its byte offsets align with the fast-layer detections, so false-positive
	// clearing is accurate. Otherwise fall back to the (possibly redacted) main body.
	key := row.BlobKey
	usingRaw := false
	if j.allowRaw != nil && j.allowRaw() &&
		row.RawBlobKey != "" && row.RawExpiresAt != nil && row.RawExpiresAt.After(time.Now()) {
		key = row.RawBlobKey
		usingRaw = true
	}

	body, err := j.readBody(ctx, key)
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

	// Confirm/clear (false_positive -> dlp.alert_cleared) requires aligned text:
	// when we scanned a redacted body the engine cannot re-find the masked secret,
	// so an apparent false_positive is actually a confirmed detection that can no
	// longer be verified. Treat it as confirmed to avoid erasing valid alerts.
	// This downgrade is skipped when a raw-window copy was used (usingRaw).
	if row.Redacted && !usingRaw && status == "false_positive" {
		status = "confirmed"
		falsePositives = nil
	}

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
