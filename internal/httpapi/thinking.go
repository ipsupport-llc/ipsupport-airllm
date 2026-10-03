package httpapi

import "strings"

const (
	thinkOpen  = "<think>"
	thinkClose = "</think>"
)

// thinkFilter cuts <think>…</think> blocks out of a reply, for targets whose
// thinking is off but whose model writes its thoughts inline anyway. Text is
// fed in as it streams: a chunk can end partway through a tag, so a tail that
// could be the start of one is held back until the next chunk shows what it
// is. The whitespace a model leaves between a block and its answer goes too.
type thinkFilter struct {
	inside bool   // within a block: text is dropped until its closing tag
	held   string // a tail that may be the start of the next tag
	trim   bool   // just past a block: leading whitespace is dropped
}

// push returns the part of s that is reply text and can be sent now.
func (f *thinkFilter) push(s string) string {
	s = f.held + s
	f.held = ""
	var out strings.Builder
	for s != "" {
		if f.inside {
			i := strings.Index(strings.ToLower(s), thinkClose)
			if i < 0 {
				f.held = tagPrefixSuffix(s, thinkClose)
				break
			}
			s = s[i+len(thinkClose):]
			f.inside, f.trim = false, true
			continue
		}
		i := strings.Index(strings.ToLower(s), thinkOpen)
		if i < 0 {
			f.held = tagPrefixSuffix(s, thinkOpen)
			out.WriteString(f.text(s[:len(s)-len(f.held)]))
			break
		}
		out.WriteString(f.text(s[:i]))
		s = s[i+len(thinkOpen):]
		f.inside = true
	}
	return out.String()
}

// flush returns what is still held once the reply has ended: the start of a
// tag that never completed was reply text after all. An unclosed block is
// dropped.
func (f *thinkFilter) flush() string {
	held := f.held
	f.held = ""
	if f.inside {
		return ""
	}
	return f.text(held)
}

func (f *thinkFilter) text(s string) string {
	if !f.trim {
		return s
	}
	s = strings.TrimLeft(s, " \t\r\n")
	if s != "" {
		f.trim = false
	}
	return s
}

// tagPrefixSuffix returns the longest tail of s that is a proper prefix of
// tag, compared without regard to case.
func tagPrefixSuffix(s, tag string) string {
	lower := strings.ToLower(s)
	for n := min(len(tag)-1, len(s)); n > 0; n-- {
		if strings.HasPrefix(tag, lower[len(s)-n:]) {
			return s[len(s)-n:]
		}
	}
	return ""
}

// stripThinkBlocks cuts every <think>…</think> block out of a whole reply.
func stripThinkBlocks(s string) string {
	var f thinkFilter
	return f.push(s) + f.flush()
}
