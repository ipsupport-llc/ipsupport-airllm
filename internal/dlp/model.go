package dlp

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
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
	Findings []modelFinding `json:"findings"`
}

// ModelScan calls the sidecar at baseURL ("POST {baseURL}/scan") and returns
// findings at or above minScore, labelled "pii:<entity>". Errors are returned
// so the caller can fail open (the deterministic layer still applies).
func ModelScan(ctx context.Context, hc *http.Client, baseURL string, minScore float64, text string) ([]Finding, error) {
	body, err := json.Marshal(modelRequest{Text: text})
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(baseURL, "/")+"/scan", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := hc.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return nil, fmt.Errorf("dlp model sidecar returned %d", resp.StatusCode)
	}

	var mr modelResponse
	if err := json.NewDecoder(resp.Body).Decode(&mr); err != nil {
		return nil, err
	}
	out := make([]Finding, 0, len(mr.Findings))
	for _, f := range mr.Findings {
		if f.Score < minScore || f.Start < 0 || f.End > len(text) || f.Start >= f.End {
			continue
		}
		out = append(out, Finding{Label: "pii:" + f.Label, Start: f.Start, End: f.End})
	}
	return out, nil
}
