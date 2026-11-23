package routing

import (
	"fmt"
	"testing"
)

// TestNextRRKeepsNoCounterForPassthrough: passthrough names are whatever a
// client sends, so a counter per name would grow the map without bound.
func TestNextRRKeepsNoCounterForPassthrough(t *testing.T) {
	r := NewRouter(nil)
	for i := 0; i < 100; i++ {
		r.NextRR(fmt.Sprintf("provider/model-%d", i))
	}
	if r.NextRR("text") != 0 || r.NextRR("text") != 1 {
		t.Fatal("an alias keeps rotating")
	}
	n := 0
	r.rr.Range(func(any, any) bool { n++; return true })
	if n != 1 {
		t.Errorf("round-robin map holds %d entries, want 1 (the alias)", n)
	}
}
