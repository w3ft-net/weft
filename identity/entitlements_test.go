package identity

import (
	"testing"
	"time"
)

// No limits reads everything: the horizon is the zero time and nothing is
// clamped. This is what a build with no plans runs on.
func TestLimits_ZeroIsUnlimited(t *testing.T) {
	var l Limits
	if h := l.Horizon(); !h.IsZero() {
		t.Errorf("Horizon = %v, want the zero time", h)
	}
	old := time.Now().AddDate(-10, 0, 0)
	if got := l.Clamp(old); !got.Equal(old) {
		t.Errorf("Clamp(%v) = %v, want it unchanged", old, got)
	}
}

func TestLimits_ClampPushesForwardToTheHorizon(t *testing.T) {
	l := Limits{Retention: 24 * time.Hour}
	got := l.Clamp(time.Now().AddDate(0, 0, -30))
	if age := time.Since(got); age < 23*time.Hour || age > 25*time.Hour {
		t.Errorf("Clamp of 30 days ago is %s old, want about a day", age)
	}
	recent := time.Now().Add(-time.Hour)
	if got := l.Clamp(recent); !got.Equal(recent) {
		t.Errorf("Clamp of a recent time = %v, want it unchanged", got)
	}
	// A zero since asked for no lower bound, and keeps asking for none.
	if got := l.Clamp(time.Time{}); !got.IsZero() {
		t.Errorf("Clamp(zero) = %v, want zero", got)
	}
}
