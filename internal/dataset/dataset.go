// Package dataset exports reviewed DLP captures as a labeled JSONL artifact
// for offline fine-tuning of the token-classification model.
package dataset

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/rromenskyi/ipsupport-airllm/internal/capture"
)

// Store provides reviewed captures for export.
type Store interface {
	// ListReviewed returns captures with review_status 'confirmed' or
	// 'false_negative'. The implementation is responsible for the filter.
	ListReviewed(ctx context.Context) ([]capture.IndexRow, error)
}

// BodyReader returns the decrypted body bytes for the given blob key.
// The caller is responsible for decryption (wrapping secrets.Sealer.Open).
type BodyReader func(ctx context.Context, blobKey string) ([]byte, error)

// BlobWriter writes an artifact to blob storage.
type BlobWriter interface {
	Put(ctx context.Context, key string, data []byte) error
}

// Export generates a JSONL training artifact from reviewed captures and writes
// it to the blob store. It returns the artifact key and the number of JSONL
// lines written (one per message that has attributed findings).
//
// Text/offset alignment
//
// The capture pipeline (captureBody + dlpEnforce) stores DLP findings as
// per-message byte offsets: each Finding's Start/End is an offset into the
// individual llm.Message.Content string, NOT into any concatenated text or
// JSON wrapper.
//
// To keep training spans valid we emit one JSONL line per message that has
// at least one attributed finding (text = msg.Content, spans = findings for
// that message). Attribution uses a greedy bounds check: a finding f is
// assigned to the first message m where f.End <= len(m.Content) and f.Start ≥ 0
// and f.End > f.Start. Each finding is used at most once. This is correct for
// the common case because dlpEnforce processes messages in order and findings
// are bounded by each message's content length. Edge case: if an earlier
// message's content is longer than a later one, a finding from the later message
// might be attributed to the earlier — reviewers should use per-message scopes
// when setting gold_labels to avoid this.
func Export(
	ctx context.Context,
	store Store,
	readBody BodyReader,
	writer BlobWriter,
) (artifactKey string, count int, err error) {
	rows, err := store.ListReviewed(ctx)
	if err != nil {
		return "", 0, fmt.Errorf("dataset: list reviewed: %w", err)
	}

	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)

	for _, row := range rows {
		// Prefer the un-redacted raw-window copy (aligned spans) when it exists
		// and has not expired; otherwise use the durable (possibly redacted) body.
		blobKey := row.BlobKey
		if row.RawBlobKey != "" && row.RawExpiresAt != nil && row.RawExpiresAt.After(time.Now()) {
			blobKey = row.RawBlobKey
		}
		if blobKey == "" {
			continue
		}

		body, berr := readBody(ctx, blobKey)
		if berr != nil {
			// Unreadable blob (swept or unavailable): skip, best-effort export.
			continue
		}

		var sb storedBody
		if jerr := json.Unmarshal(body, &sb); jerr != nil {
			continue
		}

		// Prefer gold_labels (reviewer-corrected) over detected (machine-only).
		spans := row.GoldLabels
		if len(spans) == 0 {
			spans = row.Detected
		}
		if len(spans) == 0 {
			continue
		}

		// Greedily attribute each finding to the first message whose content
		// length accommodates it (see alignment comment above).
		used := make([]bool, len(spans))
		for _, msg := range sb.Messages {
			if msg.Content == "" {
				continue
			}
			var msgSpans []spanExport
			for j, f := range spans {
				if used[j] {
					continue
				}
				if f.Start >= 0 && f.End > f.Start && f.End <= len(msg.Content) {
					msgSpans = append(msgSpans, spanExport{
						Label: f.Label,
						Start: f.Start,
						End:   f.End,
					})
					used[j] = true
				}
			}
			if len(msgSpans) == 0 {
				continue
			}
			if encErr := enc.Encode(exportLine{Text: msg.Content, Spans: msgSpans}); encErr != nil {
				return "", 0, fmt.Errorf("dataset: encode: %w", encErr)
			}
			count++
		}
	}

	artifactKey = fmt.Sprintf("datasets/%s.jsonl", timeKey())
	if putErr := writer.Put(ctx, artifactKey, buf.Bytes()); putErr != nil {
		return "", 0, fmt.Errorf("dataset: write artifact: %w", putErr)
	}
	return artifactKey, count, nil
}

// storedBody mirrors the JSON format written by captureBody.
type storedBody struct {
	Messages []struct {
		Role    string `json:"role"`
		Content string `json:"content"`
	} `json:"messages"`
}

// exportLine is one record in the JSONL artifact.
type exportLine struct {
	Text  string       `json:"text"`
	Spans []spanExport `json:"spans"`
}

// spanExport is a labeled byte span within the text field.
type spanExport struct {
	Label string `json:"label"`
	Start int    `json:"start"`
	End   int    `json:"end"`
}

func timeKey() string {
	return time.Now().UTC().Format("20060102-150405")
}
