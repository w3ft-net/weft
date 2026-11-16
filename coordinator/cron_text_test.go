package coordinator

import (
	"strings"
	"testing"
)

func TestParseCronOutput_Text(t *testing.T) {
	text := cronSchema{text: true}
	points, err := parseCronOutput(text, "i-0abc lease-a\ni-0def lease-b\n")
	if err != nil || len(points) != 2 || points[0].text != "i-0abc" || points[0].tag != "lease-a" {
		t.Fatalf("points=%+v err=%v", points, err)
	}
	if _, err := parseCronOutput(text, "i-0abc lease-a extra\n"); err == nil || !strings.Contains(err.Error(), "has 3 fields") {
		t.Fatalf("three fields on a text cron: %v", err)
	}
	headers := cronSchema{text: true, value: mustSpec(t, ".holder"), tag: mustSpec(t, ".lease")}
	points, err = parseCronOutput(headers, `{"lease":"a","holder":"i-0abc"}`)
	if err != nil || len(points) != 1 || points[0].text != "i-0abc" || points[0].tag != "a" {
		t.Fatalf("JSON text points=%+v err=%v", points, err)
	}
	if _, err := parseCronOutput(headers, `{"lease":"a"}`); err == nil || !strings.Contains(err.Error(), "has no .holder") {
		t.Fatalf("missing text value: %v", err)
	}
}
