// Package dlp detects secrets, tokens, and other sensitive strings in text.
// Detection is deterministic (regular expressions plus a Shannon-entropy
// check); a fuzzy model layer can be added later behind the same Scan API.
// The package never returns the secret value, only labelled spans, so callers
// can redact or block without logging the secret itself.
package dlp

import (
	"math"
	"regexp"
	"sort"
	"strings"
)

// Finding is one detected sensitive span (byte offsets into the scanned text).
type Finding struct {
	Label string `json:"label"`
	Start int    `json:"start"`
	End   int    `json:"end"`
}

type rule struct {
	label string
	re    *regexp.Regexp
}

// rules are ordered most-specific first; overlapping matches are merged with
// the earliest rule's label winning.
var rules = []rule{
	{"private_key", regexp.MustCompile(`(?s)-----BEGIN [A-Z ]*PRIVATE KEY-----.*?-----END [A-Z ]*PRIVATE KEY-----`)},
	{"anthropic_key", regexp.MustCompile(`sk-ant-[A-Za-z0-9_\-]{20,}`)},
	{"openai_key", regexp.MustCompile(`sk-(?:proj-)?[A-Za-z0-9_\-]{20,}`)},
	{"openrouter_key", regexp.MustCompile(`sk-or-[A-Za-z0-9_\-]{20,}`)},
	{"xai_key", regexp.MustCompile(`xai-[A-Za-z0-9]{20,}`)},
	{"github_token", regexp.MustCompile(`gh[pousr]_[A-Za-z0-9]{36,}`)},
	{"aws_access_key", regexp.MustCompile(`A(?:KIA|SIA)[0-9A-Z]{16}`)},
	{"google_api_key", regexp.MustCompile(`AIza[0-9A-Za-z_\-]{35}`)},
	{"slack_token", regexp.MustCompile(`xox[baprs]-[0-9A-Za-z\-]{10,}`)},
	{"stripe_key", regexp.MustCompile(`[rs]k_(?:live|test)_[0-9A-Za-z]{16,}`)},
	{"jwt", regexp.MustCompile(`eyJ[A-Za-z0-9_\-]{10,}\.eyJ[A-Za-z0-9_\-]{10,}\.[A-Za-z0-9_\-]{10,}`)},
	{"bearer_token", regexp.MustCompile(`(?i)bearer\s+[A-Za-z0-9._\-]{20,}`)},
}

// entropyCandidate matches long token-like runs to entropy-check.
var entropyCandidate = regexp.MustCompile(`[A-Za-z0-9+/=_\-]{24,}`)

const entropyThreshold = 3.6 // bits/char; random base64 is ~5-6, English ~2-3

// Scan returns all sensitive spans found in s, merged and sorted by position.
func Scan(s string) []Finding {
	var found []Finding
	for _, r := range rules {
		for _, loc := range r.re.FindAllStringIndex(s, -1) {
			found = append(found, Finding{Label: r.label, Start: loc[0], End: loc[1]})
		}
	}
	for _, loc := range entropyCandidate.FindAllStringIndex(s, -1) {
		if shannon(s[loc[0]:loc[1]]) >= entropyThreshold {
			found = append(found, Finding{Label: "high_entropy", Start: loc[0], End: loc[1]})
		}
	}
	return merge(found)
}

// Redact replaces every finding with a [REDACTED:label] marker.
func Redact(s string, findings []Finding) string {
	if len(findings) == 0 {
		return s
	}
	var b strings.Builder
	prev := 0
	for _, f := range findings {
		if f.Start < prev || f.Start > len(s) || f.End > len(s) {
			continue
		}
		b.WriteString(s[prev:f.Start])
		b.WriteString("[REDACTED:" + f.Label + "]")
		prev = f.End
	}
	b.WriteString(s[prev:])
	return b.String()
}

// Labels returns the distinct finding labels, sorted.
func Labels(findings []Finding) []string {
	set := map[string]bool{}
	for _, f := range findings {
		set[f.Label] = true
	}
	out := make([]string, 0, len(set))
	for l := range set {
		out = append(out, l)
	}
	sort.Strings(out)
	return out
}

// merge sorts findings by start and drops any that overlap an earlier one, so
// redaction offsets never collide. The earliest (most specific) rule wins.
func merge(in []Finding) []Finding {
	if len(in) <= 1 {
		return in
	}
	sort.SliceStable(in, func(a, b int) bool {
		if in[a].Start != in[b].Start {
			return in[a].Start < in[b].Start
		}
		return in[a].End > in[b].End // prefer the longer span at the same start
	})
	out := in[:0:0]
	end := -1
	for _, f := range in {
		if f.Start < end {
			continue // overlaps a kept finding
		}
		out = append(out, f)
		end = f.End
	}
	return out
}

// shannon returns the Shannon entropy (bits per character) of s.
func shannon(s string) float64 {
	if s == "" {
		return 0
	}
	var counts [256]int
	for i := 0; i < len(s); i++ {
		counts[s[i]]++
	}
	n := float64(len(s))
	var h float64
	for _, c := range counts {
		if c == 0 {
			continue
		}
		p := float64(c) / n
		h -= p * math.Log2(p)
	}
	return h
}
