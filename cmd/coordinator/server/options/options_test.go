package options

import (
	"bytes"
	"flag"
	"os"
	"testing"
)

// The flags are the process's interface to unit files and deploy scripts. The
// golden is the -h output of cmd/coordinator from before it was split.
func TestCoordinatorFlags_AreWhatTheMainAlwaysTook(t *testing.T) {
	fs := flag.NewFlagSet("coordinator", flag.ContinueOnError)
	var out bytes.Buffer
	fs.SetOutput(&out)
	Define(fs)
	fs.PrintDefaults()

	want, err := os.ReadFile("testdata/coordinator_flags.golden")
	if err != nil {
		t.Fatal(err)
	}
	if out.String() != string(want) {
		t.Errorf("the flag set changed:\n--- got\n%s\n--- want\n%s", out.String(), want)
	}
}

func TestCoordinatorFlags_ParseIntoTheFields(t *testing.T) {
	fs := flag.NewFlagSet("coordinator", flag.ContinueOnError)
	fl := Define(fs)
	if err := fs.Parse([]string{"-port", "1", "-compute", "c:2", "-redis-tls", "-customer", "dev"}); err != nil {
		t.Fatal(err)
	}
	if fl.Port != 1 || fl.ComputeAddr != "c:2" || !fl.RedisTLS || fl.Customer != "dev" {
		t.Errorf("flags = %+v", fl)
	}
	if fl.HealthPort != 9021 || fl.LocalAddr != "127.0.0.1:9025" || fl.AWSRegion != "us-east-2" {
		t.Errorf("defaults changed: %+v", fl)
	}
}
