package mounts

import (
	"testing"

	"github.com/graybeardcorp/weft/store"
)

// Schema headers stay in the stored pipeline, so a cron file round-trips
// through its own serialization with them in place.
func TestParseCron_KeepsSchemaHeaders(t *testing.T) {
	file := "# schedule: */5 * * * *\n# value: .pct\n# tag: .host\n# a note\ncat /log/a.log | jq -c .\n"
	schedule, pipeline, enabled, err := ParseCron(file)
	if err != nil {
		t.Fatal(err)
	}
	if want := "# value: .pct\n# tag: .host\ncat /log/a.log | jq -c ."; pipeline != want {
		t.Fatalf("pipeline = %q, want %q", pipeline, want)
	}
	got := SerializeCron(&store.CronWatch{Schedule: schedule, Pipeline: pipeline, Enabled: enabled})
	if want := "# schedule: */5 * * * *\n# value: .pct\n# tag: .host\ncat /log/a.log | jq -c .\n"; got != want {
		t.Fatalf("serialized = %q, want %q", got, want)
	}
	if _, _, _, err := ParseCron("# schedule: every 5m\n# value: .pct\n"); err == nil {
		t.Fatal("a file with only headers saved without a pipeline")
	}
}
