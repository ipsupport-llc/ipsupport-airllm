package dlp

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"unicode/utf8"
)

// model layer: the gateway calls an external BERT-NER sidecar over HTTP for
// fuzzy/contextual PII that the deterministic rules miss. The sidecar returns
// labelled spans with confidence scores.

type modelRequest struct {
	Text string `json:"text"`
}

type modelFinding struct {
	Label string  `json:"label"`
	Start int     `json:"start"`
	End   int     `json:"end"`
	Score float64 `json:"score"`
}

type modelResponse struct {
	Findings  []modelFinding `json:"findings"`
	Truncated bool           `json:"truncated"`
}

// ModelScan calls the sidecar at baseURL ("POST {baseURL}/scan") and returns
// findings at or above minScore, labelled "pii:<entity>". Errors are returned
// so the caller can fail open (the deterministic layer still applies).
// truncated reports the sidecar's own "truncated" flag: it scanned only a
// prefix of text (DLP_MAX_CHARS in app.py) — findings past that prefix are
// simply absent, not false negatives the model missed, but the caller should
// still surface this as incomplete coverage rather than a clean scan.
// truncated is only meaningful when err is nil.
func ModelScan(ctx context.Context, hc *http.Client, baseURL string, minScore float64, text string) (findings []Finding, truncated bool, err error) {
	body, err := json.Marshal(modelRequest{Text: text})
	if err != nil {
		return nil, false, err
	}
	// Tolerate a configured URL that already ends in /scan: before the pool
	// preserved URL paths, such a URL worked (its path was dropped).
	endpoint := strings.TrimSuffix(strings.TrimRight(baseURL, "/"), "/scan") + "/scan"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, false, err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := hc.Do(req)
	if err != nil {
		return nil, false, err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		// Drain so the transport can reuse this connection for the next scan —
		// left unread, a non-2xx body forces a fresh connection (and, over
		// TLS, a fresh handshake) per failed scan instead of pooling it, and
		// ModelScan runs on every message the BERT layer is enabled for.
		_, _ = io.Copy(io.Discard, resp.Body)
		return nil, false, fmt.Errorf("dlp model sidecar returned %d", resp.StatusCode)
	}

	var mr modelResponse
	if err := json.NewDecoder(resp.Body).Decode(&mr); err != nil {
		return nil, false, err
	}
	out := make([]Finding, 0, len(mr.Findings))
	runes := utf8.RuneCountInString(text)
	for _, f := range mr.Findings {
		// Out-of-range spans are dropped, not clamped: clamping a bogus end
		// to len(text) would redact the whole rest of the message.
		if f.Score < minScore || f.Start < 0 || f.End <= f.Start || f.End > runes {
			continue
		}
		// The sidecar's start/end are Python string indices — Unicode
		// code-point offsets (transformers' TokenClassificationPipeline
		// convention) — not UTF-8 byte offsets. Every other offset in this
		// codebase (Finding.Start/End, dlp.Redact) is a Go byte offset into
		// the string, so convert here: for pure-ASCII text the two coincide
		// (no behavior change), but any non-ASCII character before a match
		// would otherwise shift the slice boundary and corrupt the
		// redaction — or the UTF-8 encoding itself.
		start := runeOffsetToByteOffset(text, f.Start)
		end := runeOffsetToByteOffset(text, f.End)
		if start >= end {
			continue
		}
		out = append(out, Finding{Label: "pii:" + f.Label, Start: start, End: end})
	}
	return out, mr.Truncated, nil
}

// runeOffsetToByteOffset converts a Unicode code-point offset into the
// equivalent UTF-8 byte offset into s. An offset at or beyond the end of s
// (including one past its last rune) clamps to len(s).
func runeOffsetToByteOffset(s string, runeIdx int) int {
	if runeIdx <= 0 {
		return 0
	}
	n := 0
	for i := range s {
		if n == runeIdx {
			return i
		}
		n++
	}
	return len(s)
}
