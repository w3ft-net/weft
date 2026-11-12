package apiapp

import (
	"bytes"
	"flag"
	"os"
	"strings"
	"testing"
)

// The golden is the -h output of cmd/api from before it was split.
func TestFlags_AreWhatTheMainAlwaysTook(t *testing.T) {
	fs := flag.NewFlagSet("api", flag.ContinueOnError)
	var out bytes.Buffer
	fs.SetOutput(&out)
	DefineFlags(fs)
	fs.PrintDefaults()

	want, err := os.ReadFile("testdata/api_flags.golden")
	if err != nil {
		t.Fatal(err)
	}
	if out.String() != string(want) {
		t.Errorf("the flag set changed:\n--- got\n%s\n--- want\n%s", out.String(), want)
	}
}

func TestFlags_ParseIntoTheFields(t *testing.T) {
	fs := flag.NewFlagSet("api", flag.ContinueOnError)
	fl := DefineFlags(fs)
	if err := fs.Parse([]string{"-port", "9", "-render-syslog-port", "7", "-redis-tls"}); err != nil {
		t.Fatal(err)
	}
	if fl.Port != "9" || fl.RenderSyslogPort != 7 || !fl.RedisTLS || fl.CoordinatorAddr != "localhost:9025" {
		t.Errorf("flags = %+v", fl)
	}
}

// An api that cannot check tokens must not start.
func TestAdapter_RequiresAJWTSecret(t *testing.T) {
	if err := (Adapter{}).validate(); err == nil || !strings.Contains(err.Error(), "JWTSecret") {
		t.Fatalf("validate() = %v, want an error naming JWTSecret", err)
	}
	if err := (Adapter{JWTSecret: func() string { return "s" }}).validate(); err != nil {
		t.Errorf("an adapter with a secret was refused: %v", err)
	}
}
