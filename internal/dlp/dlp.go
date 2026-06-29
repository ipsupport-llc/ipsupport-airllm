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
	"unicode"
)

// DetectorVersion identifies the deterministic detection pipeline that
// produced a set of weak labels. It is stamped onto each capture's
// model_version so the flywheel/dataset can track which detector generation
// labelled a row. Bump it when detection rules change materially.
const DetectorVersion = "regex+entropy/v1"

// Finding is one detected sensitive span (byte offsets into the scanned text).
type Finding struct {
	Label string `json:"label"`
	Start int    `json:"start"`
	End   int    `json:"end"`
}

// pattern is a built-in detection rule. category and defaultOn drive the UI
// toggles and the default-enabled set; validate (optional) post-filters a regex
// match (e.g. Luhn for cards, octet ranges for IPs).
type pattern struct {
	label     string
	category  string // "secret" | "pii"
	re        *regexp.Regexp
	validate  func(string) bool
	defaultOn bool
}

// patterns are ordered most-specific first; overlapping matches are merged with
// the earliest pattern's label winning. Secret patterns are on by default; the
// PII patterns are opt-in ("Sensitive Info Detection" — the operator toggles
// them on per workspace).
var patterns = []pattern{
	{label: "private_key", category: "secret", defaultOn: true, re: regexp.MustCompile(`(?s)-----BEGIN [A-Z ]*PRIVATE KEY-----.*?-----END [A-Z ]*PRIVATE KEY-----`)},
	{label: "anthropic_key", category: "secret", defaultOn: true, re: regexp.MustCompile(`sk-ant-[A-Za-z0-9_\-]{20,}`)},
	{label: "openai_key", category: "secret", defaultOn: true, re: regexp.MustCompile(`sk-(?:proj-)?[A-Za-z0-9_\-]{20,}`)},
	{label: "openrouter_key", category: "secret", defaultOn: true, re: regexp.MustCompile(`sk-or-[A-Za-z0-9_\-]{20,}`)},
	{label: "xai_key", category: "secret", defaultOn: true, re: regexp.MustCompile(`xai-[A-Za-z0-9]{20,}`)},
	{label: "github_token", category: "secret", defaultOn: true, re: regexp.MustCompile(`gh[pousr]_[A-Za-z0-9]{36,}`)},
	{label: "aws_access_key", category: "secret", defaultOn: true, re: regexp.MustCompile(`A(?:KIA|SIA)[0-9A-Z]{16}`)},
	{label: "google_api_key", category: "secret", defaultOn: true, re: regexp.MustCompile(`AIza[0-9A-Za-z_\-]{35}`)},
	{label: "slack_token", category: "secret", defaultOn: true, re: regexp.MustCompile(`xox[baprs]-[0-9A-Za-z\-]{10,}`)},
	{label: "stripe_key", category: "secret", defaultOn: true, re: regexp.MustCompile(`[rs]k_(?:live|test)_[0-9A-Za-z]{16,}`)},
	{label: "jwt", category: "secret", defaultOn: true, re: regexp.MustCompile(`eyJ[A-Za-z0-9_\-]{10,}\.eyJ[A-Za-z0-9_\-]{10,}\.[A-Za-z0-9_\-]{10,}`)},
	{label: "bearer_token", category: "secret", defaultOn: true, re: regexp.MustCompile(`(?i)bearer\s+[A-Za-z0-9._\-]{20,}`)},

	// PII — opt-in. Off by default; the operator enables them per workspace.
	{label: "email", category: "pii", re: regexp.MustCompile(`[A-Za-z0-9._%+\-]+@[A-Za-z0-9.\-]+\.[A-Za-z]{2,}`)},
	{label: "phone", category: "pii", re: regexp.MustCompile(`\+?\d{0,3}[ .\-]?\(?\d{3}\)?[ .\-]?\d{3}[ .\-]?\d{4}\b`)},
	{label: "ssn", category: "pii", re: regexp.MustCompile(`\b\d{3}-\d{2}-\d{4}\b`)},
	{label: "credit_card", category: "pii", validate: luhnValid, re: regexp.MustCompile(`\b\d(?:[ \-]?\d){12,18}\b`)},
	{label: "ip_address", category: "pii", validate: ipv4Valid, re: regexp.MustCompile(`\b(?:\d{1,3}\.){3}\d{1,3}\b`)},
}

// luhnValid reports whether the digits in s (13–19 of them) pass the Luhn
// checksum — used to keep credit_card from flagging arbitrary long numbers.
func luhnValid(s string) bool {
	digits := make([]int, 0, len(s))
	for i := 0; i < len(s); i++ {
		if s[i] >= '0' && s[i] <= '9' {
			digits = append(digits, int(s[i]-'0'))
		}
	}
	if len(digits) < 13 || len(digits) > 19 {
		return false
	}
	sum, dbl := 0, false
	for i := len(digits) - 1; i >= 0; i-- {
		d := digits[i]
		if dbl {
			if d *= 2; d > 9 {
				d -= 9
			}
		}
		sum += d
		dbl = !dbl
	}
	return sum%10 == 0
}

// ipv4Valid reports whether s is a dotted-quad with each octet in 0–255.
func ipv4Valid(s string) bool {
	parts := strings.Split(s, ".")
	if len(parts) != 4 {
		return false
	}
	for _, p := range parts {
		if len(p) == 0 || len(p) > 3 {
			return false
		}
		n := 0
		for i := 0; i < len(p); i++ {
			if p[i] < '0' || p[i] > '9' {
				return false
			}
			n = n*10 + int(p[i]-'0')
		}
		if n > 255 {
			return false
		}
	}
	return true
}

// entropyCandidate matches long token-like runs to entropy-check.
var entropyCandidate = regexp.MustCompile(`[A-Za-z0-9+/=_\-]{24,}`)

const entropyThreshold = 3.6 // bits/char; random base64 is ~5-6, English ~2-3

// CustomPattern is an operator-defined detection rule, already compiled.
type CustomPattern struct {
	Label string
	Re    *regexp.Regexp
}

// PatternSet selects which detectors run. Enabled maps a built-in label to
// on/off; a label absent from the map uses that pattern's default. Custom holds
// compiled operator patterns. Entropy toggles the high-entropy heuristic.
type PatternSet struct {
	Enabled map[string]bool
	Custom  []CustomPattern
	Entropy bool
}

// PatternInfo describes a built-in pattern for the config validator and the UI.
type PatternInfo struct {
	Label     string `json:"label"`
	Category  string `json:"category"`
	DefaultOn bool   `json:"default_on"`
}

// BuiltinPatterns returns metadata for every built-in pattern plus the entropy
// heuristic, in display order.
func BuiltinPatterns() []PatternInfo {
	out := make([]PatternInfo, 0, len(patterns)+1)
	for _, p := range patterns {
		out = append(out, PatternInfo{Label: p.label, Category: p.category, DefaultOn: p.defaultOn})
	}
	out = append(out, PatternInfo{Label: "high_entropy", Category: "secret", DefaultOn: true})
	return out
}

// patternEnabled reports whether a label is on for ps, falling back to def when
// the label is not explicitly set (so partial/legacy configs keep working).
func patternEnabled(ps PatternSet, label string, def bool) bool {
	if ps.Enabled != nil {
		if v, ok := ps.Enabled[label]; ok {
			return v
		}
	}
	return def
}

// Scan returns all sensitive spans using the default pattern set (secret rules
// + entropy; PII patterns off). Kept for back-compat callers and tests.
func Scan(s string) []Finding {
	return ScanWith(s, PatternSet{Entropy: true})
}

// ScanWith returns sensitive spans for the selected pattern set, merged and
// sorted by position. Named rules take precedence: an entropy hit is only added
// when it does not overlap a named finding (so "VAR=sk-..." reports openai_key,
// not a generic high-entropy span that swallows the variable name).
func ScanWith(s string, ps PatternSet) []Finding {
	var named []Finding
	for _, p := range patterns {
		if !patternEnabled(ps, p.label, p.defaultOn) {
			continue
		}
		for _, loc := range p.re.FindAllStringIndex(s, -1) {
			if p.validate != nil && !p.validate(s[loc[0]:loc[1]]) {
				continue
			}
			named = append(named, Finding{Label: p.label, Start: loc[0], End: loc[1]})
		}
	}
	for _, c := range ps.Custom {
		for _, loc := range c.Re.FindAllStringIndex(s, -1) {
			if loc[1] <= loc[0] {
				continue // skip zero-width matches (e.g. a custom regex like `\d*`)
			}
			named = append(named, Finding{Label: c.Label, Start: loc[0], End: loc[1]})
		}
	}
	named = Merge(named)

	all := append([]Finding(nil), named...)
	if ps.Entropy {
		for _, loc := range entropyCandidate.FindAllStringIndex(s, -1) {
			tok := s[loc[0]:loc[1]]
			// Require mixed character classes so hex digests, git SHAs, and UUIDs
			// (single-case) don't trip the entropy detector; real secret tokens
			// are almost always upper+lower+digit.
			if !mixedClasses(tok) || shannon(tok) < entropyThreshold {
				continue
			}
			if overlapsAny(loc[0], loc[1], named) {
				continue
			}
			all = append(all, Finding{Label: "high_entropy", Start: loc[0], End: loc[1]})
		}
	}
	return Merge(all)
}

func overlapsAny(start, end int, fs []Finding) bool {
	for _, f := range fs {
		if start < f.End && f.Start < end {
			return true
		}
	}
	return false
}

func mixedClasses(s string) bool {
	var up, lo, dig bool
	for i := 0; i < len(s); i++ {
		switch c := s[i]; {
		case c >= 'A' && c <= 'Z':
			up = true
		case c >= 'a' && c <= 'z':
			lo = true
		case c >= '0' && c <= '9':
			dig = true
		}
	}
	return up && lo && dig
}

// Redact replaces every finding with a [REDACTED:label] marker. Adjacent
// findings with the same label, separated by only whitespace (or nothing), are
// collapsed into a single marker — so a model entity the NER splits into pieces
// ("Acme Corporation" → two ORG spans) renders as one [REDACTED:label] rather
// than several back to back.
func Redact(s string, findings []Finding) string {
	if len(findings) == 0 {
		return s
	}
	findings = coalesceAdjacent(s, findings)
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

// coalesceAdjacent merges runs of same-label findings whose gap is empty or all
// whitespace into one span (first start … last end). Input must be sorted by
// start and non-overlapping (as Merge produces). It does not mutate findings.
func coalesceAdjacent(s string, findings []Finding) []Finding {
	out := make([]Finding, 0, len(findings))
	for _, f := range findings {
		if n := len(out); n > 0 {
			prev := out[n-1]
			if prev.Label == f.Label && f.Start >= prev.End && f.End <= len(s) && isBlank(s[prev.End:f.Start]) {
				if f.End > prev.End {
					out[n-1].End = f.End
				}
				continue
			}
		}
		out = append(out, f)
	}
	return out
}

// isBlank reports whether s is empty or contains only whitespace.
func isBlank(s string) bool {
	for _, r := range s {
		if !unicode.IsSpace(r) {
			return false
		}
	}
	return true
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

// Merge sorts findings by start and drops any that overlap an earlier one, so
// redaction offsets never collide. The earliest (most specific) rule wins.
// It is used to combine the regex layer with the model layer.
func Merge(in []Finding) []Finding {
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
