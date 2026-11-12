package hostedapi

import "testing"

// The last lesson must not graduate the user: it parks them at "graduate",
// and only the explicit finish click advances to "done".
func TestOnboardingSequence_GraduateParksBeforeDone(t *testing.T) {
	n := len(onboardingSequence)
	if onboardingSequence[n-2] != "graduate" || onboardingSequence[n-1] != "done" {
		t.Fatalf("sequence tail = %v, want [... graduate done]", onboardingSequence[n-3:])
	}
	if got := nextOnboardingStep(onboardingSequence[n-3]); got != "graduate" {
		t.Errorf("last lesson advances to %q, want graduate", got)
	}
}
