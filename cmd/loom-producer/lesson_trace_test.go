package main

import (
	"regexp"
	"strings"
	"testing"

	"github.com/graybeardcorp/weft/coordinator/onboarding"
)

// The trace-find lesson tells the user "Loom's checkout flow hops from the
// API to an edge payment gateway to a worker ... copy one of the IDs
// printed above". For that to be true, the lesson's grep has to select
// chain-start lines (event name `checkout`) and nothing else. Ambient
// traffic also mentions /checkout as a request *path*, so a bare
// `grep checkout` hands the user single-hop trace IDs and the trace
// lesson shows one line from one service instead of the cross-service
// story.
func TestTraceFindLessonSelectsChainsNotAmbientCheckoutRequests(t *testing.T) {
	lesson := onboarding.Lookup("shell-trace-find")
	if lesson == nil {
		t.Fatal("no shell-trace-find lesson")
	}
	m := regexp.MustCompile(`grep\s+(?:'([^']*)'|"([^"]*)"|(\S+))`).FindStringSubmatch(lesson.Action)
	if m == nil {
		t.Fatalf("no grep in lesson action %q", lesson.Action)
	}
	pattern := m[1] + m[2] + m[3]
	re, err := regexp.Compile(pattern)
	if err != nil {
		t.Fatalf("grep pattern %q: %v", pattern, err)
	}

	rng := newRNG("lesson-trace-test")

	var ambient, ambientMentioningCheckout int
	for i := 0; i < 2000; i++ {
		level := "INFO"
		if i%7 == 0 {
			level = "ERROR"
		}
		line := LineFor(rng, "api", level, "0123456789abcdef0123456789abcdef")
		ambient++
		if strings.Contains(line, "/checkout") {
			ambientMentioningCheckout++
		}
		if re.MatchString(line) {
			t.Fatalf("grep %q selects an ambient line (single-hop trace ID): %s", pattern, line)
		}
	}
	if ambientMentioningCheckout == 0 {
		t.Fatalf("test is vacuous: none of %d ambient lines mention /checkout", ambient)
	}

	for i := 0; i < 50; i++ {
		chain := formatLine("INFO", "fedcba9876543210fedcba9876543210", checkoutContent(rng))
		if !re.MatchString(chain) {
			t.Fatalf("grep %q misses a chain-start line: %s", pattern, chain)
		}
	}
}
