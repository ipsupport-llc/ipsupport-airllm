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
