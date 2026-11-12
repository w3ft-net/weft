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

// With no identity of its own an api runs the open one, which needs its
// credential: without it the api must not start.
func TestAdapter_OpenIdentityRefusesToStartUnconfigured(t *testing.T) {
	t.Setenv("WEFT_BASIC_AUTH_USER", "")
	t.Setenv("WEFT_BASIC_AUTH_PASSWORD", "")
	if _, err := (Adapter{}).identity(nil); err == nil || !strings.Contains(err.Error(), "WEFT_BASIC_AUTH_USER") {
		t.Fatalf("identity() = %v, want an error naming the credential", err)
	}
	t.Setenv("WEFT_BASIC_AUTH_USER", "u")
	t.Setenv("WEFT_BASIC_AUTH_PASSWORD", "p")
	if _, err := (Adapter{}).identity(nil); err != nil {
		t.Errorf("a configured open identity was refused: %v", err)
	}
}
