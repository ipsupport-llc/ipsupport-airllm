package breaker

import (
	"context"
	"testing"
	"time"
)

// clock is a settable time source for the breaker.
type clock struct{ t time.Time }

func (c *clock) now() time.Time { return c.t }

func newTestBreaker(t *testing.T) (*Breaker, *clock, *[]Transition) {
	t.Helper()
	c := &clock{t: time.Unix(1_700_000_000, 0)}
	var seen []Transition
	b := New(nil, WithClock(c.now), WithObserver(func(tr Transition) { seen = append(seen, tr) }))
	return b, c, &seen
}

var (
	key = Key{Alias: "a", Tier: 0}
	on  = func() Settings { s := Defaults(); s.Enabled = true; return s }()
)

func tripTier(t *testing.T, b *Breaker) {
	t.Helper()
	for range on.Failures {
		b.Record(context.Background(), key, on, true, false)
	}
}

func TestForceOnAClosedTierIsAPlainAdmission(t *testing.T) {
	b, _, _ := newTestBreaker(t)
	if adm := b.Force(context.Background(), key, on); adm != (Admission{}) {
		t.Errorf("Force on a closed tier = %+v, want a plain admission", adm)
	}
}

func TestAdmitReportsWhenAnOpenTierCoolsDown(t *testing.T) {
	b, c, _ := newTestBreaker(t)
	tripTier(t, b)
	adm := b.Admit(context.Background(), key, on)
	if !adm.Skip || !adm.OpenUntil.Equal(c.t.Add(time.Minute)) {
		t.Errorf("Admit on an open tier = %+v, want a skip until the 60s cooldown ends", adm)
	}
}

func TestForceProbesAnOpenTierBeforeItsCooldownEnds(t *testing.T) {
	b, _, seen := newTestBreaker(t)
	tripTier(t, b)
	adm := b.Force(context.Background(), key, on)
	if !adm.Probe || adm.Skip {
		t.Fatalf("Force on a cooling tier = %+v, want the probe", adm)
	}
	last := (*seen)[len(*seen)-1]
	if last.To != HalfOpen || last.Cause != "forced" {
		t.Errorf("transition = %+v, want a forced half-open", last)
	}
	// The forced probe holds the tier: a second forced request skips it,
	// with no cooldown to report.
	if adm := b.Force(context.Background(), key, on); !adm.Skip || !adm.OpenUntil.IsZero() {
		t.Errorf("second Force while the probe is out = %+v, want a skip", adm)
	}
}

func TestAnUnusedForcedProbeReopensTheTierForTheRestOfItsCooldown(t *testing.T) {
	b, c, _ := newTestBreaker(t)
	tripTier(t, b)
	until := c.t.Add(time.Minute)
	b.Force(context.Background(), key, on)
	b.AbandonProbe(context.Background(), key, on)

	if adm := b.Admit(context.Background(), key, on); !adm.Skip || !adm.OpenUntil.Equal(until) {
		t.Errorf("Admit after an unused forced probe = %+v, want a skip until %v", adm, until)
	}
	c.t = until
	if adm := b.Admit(context.Background(), key, on); !adm.Probe {
		t.Errorf("Admit once the cooldown ended = %+v, want the probe", adm)
	}
}

func TestExtendProbeHoldsTheTierPastItsLease(t *testing.T) {
	b, c, _ := newTestBreaker(t)
	tripTier(t, b)
	c.t = c.t.Add(time.Minute)
	first := b.Admit(context.Background(), key, on)
	if !first.Probe || first.ProbeID == "" {
		t.Fatalf("Admit = %+v, want the probe with an id", first)
	}
	lease := ProbeLeaseFor(on)
	c.t = c.t.Add(lease - time.Second)
	b.ExtendProbe(context.Background(), key, on, "someone-else") // not this probe's id: no effect
	b.ExtendProbe(context.Background(), key, on, first.ProbeID)
	c.t = c.t.Add(lease - time.Second) // well past the first lease
	if adm := b.Admit(context.Background(), key, on); !adm.Skip {
		t.Errorf("Admit while an extended probe runs = %+v, want a skip", adm)
	}
	c.t = c.t.Add(2 * time.Second) // past the extension too: a lost probe
	second := b.Admit(context.Background(), key, on)
	if !second.Probe || second.ProbeID == first.ProbeID {
		t.Fatalf("Admit after the extended lease = %+v, want a new probe", second)
	}
	// The lost probe's holder can no longer extend the tier the new one holds.
	b.ExtendProbe(context.Background(), key, on, first.ProbeID)
	c.t = c.t.Add(lease + time.Second)
	if adm := b.Admit(context.Background(), key, on); !adm.Probe {
		t.Errorf("Admit after the second lease = %+v, want a probe: the old id extended it", adm)
	}
	// A recorded probe's tier is not touched.
	b.Record(context.Background(), key, on, false, true)
	b.ExtendProbe(context.Background(), key, on, second.ProbeID)
	if st, _ := b.Status(context.Background(), key, on); st.State != Closed {
		t.Errorf("state after ExtendProbe on a closed tier = %v, want closed", st.State)
	}
}

func TestAnUnusedProbeAfterTheCooldownLetsTheNextRequestProbe(t *testing.T) {
	b, c, _ := newTestBreaker(t)
	tripTier(t, b)
	c.t = c.t.Add(time.Minute)
	b.Admit(context.Background(), key, on)
	b.AbandonProbe(context.Background(), key, on)
	if adm := b.Admit(context.Background(), key, on); !adm.Probe {
		t.Errorf("Admit after an unused probe = %+v, want the probe again", adm)
	}
}
