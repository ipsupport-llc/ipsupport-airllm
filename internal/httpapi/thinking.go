package httpapi

import "strings"

const (
	thinkOpen  = "<think>"
	thinkClose = "</think>"
)

// thinkFilter cuts the thinking a model writes inline — <think>…</think>
// blocks at the start of its reply — for targets whose thinking is off.
// Only leading blocks are the model's thoughts: once reply text has started,
// a later "<think>" is part of the answer and passes through. Text is fed in
// as it streams, so a start that may still turn into a tag is held back
// until the next chunk shows what it is. The whitespace a model leaves
// between its thoughts and its answer goes too.
type thinkFilter struct {
	replying bool   // reply text has started: everything passes through
	inside   bool   // within a block: text is dropped until its closing tag
	pending  string // a start that may yet turn out to be a tag
	trim     bool   // just past a block: leading whitespace is dropped
}

// push returns the part of s that is reply text and can be sent now.
func (f *thinkFilter) push(s string) string {
	if f.replying {
		return s
	}
	s = f.pending + s
	f.pending = ""
	for {
		if f.inside {
			i := indexASCIIFold(s, thinkClose)
			if i < 0 {
				f.pending = partialTagTail(s, thinkClose)
				return ""
			}
			s = s[i+len(thinkClose):]
			f.inside, f.trim = false, true
			continue
		}
		rest := strings.TrimLeft(s, " \t\r\n")
		switch {
		case hasASCIIFoldPrefix(rest, thinkOpen):
			s = rest[len(thinkOpen):]
			f.inside = true
		case len(rest) < len(thinkOpen) && hasASCIIFoldPrefix(rest, thinkOpen[:len(rest)]):
			f.pending = s
			return ""
		default:
			f.replying = true
			return f.text(s)
		}
	}
}

// flush returns what is still pending once the reply has ended: a start that
// never became a tag was reply text after all. An unclosed block is dropped.
func (f *thinkFilter) flush() string {
	pending := f.pending
	f.pending = ""
	if f.inside {
		return ""
	}
	return f.text(pending)
}

func (f *thinkFilter) text(s string) string {
	if f.trim {
		s = strings.TrimLeft(s, " \t\r\n")
	}
	return s
}

// indexASCIIFold is strings.Index for an ASCII lower-case tag, ignoring ASCII
// case only, so every index is a byte offset into s itself.
func indexASCIIFold(s, tag string) int {
	for i := 0; i+len(tag) <= len(s); i++ {
		if hasASCIIFoldPrefix(s[i:], tag) {
			return i
		}
	}
	return -1
}

// hasASCIIFoldPrefix reports whether s starts with the ASCII lower-case
// prefix, ignoring ASCII case only.
func hasASCIIFoldPrefix(s, prefix string) bool {
	if len(s) < len(prefix) {
		return false
	}
	for i := 0; i < len(prefix); i++ {
		c := s[i]
		if 'A' <= c && c <= 'Z' {
			c += 'a' - 'A'
		}
		if c != prefix[i] {
			return false
		}
	}
	return true
}

// partialTagTail returns the longest tail of s that is a proper prefix of
// tag: the part of a closing tag a chunk may have been cut through.
func partialTagTail(s, tag string) string {
	for n := min(len(tag)-1, len(s)); n > 0; n-- {
		if hasASCIIFoldPrefix(s[len(s)-n:], tag[:n]) {
			return s[len(s)-n:]
		}
	}
	return ""
}

// stripThinkBlocks cuts the leading thinking blocks out of a whole reply.
func stripThinkBlocks(s string) string {
	var f thinkFilter
	return f.push(s) + f.flush()
}
