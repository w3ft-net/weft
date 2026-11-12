package app

import (
	"bytes"
	"flag"
	"os"
	"strings"
	"testing"

	"github.com/graybeardcorp/weft/coordinator"
	"github.com/graybeardcorp/weft/identity"
	"github.com/graybeardcorp/weft/shape/shapetest"
	"github.com/graybeardcorp/weft/store"
	"github.com/graybeardcorp/weft/tenancy/single"
)

// The flags are the process's interface to unit files and deploy scripts. The
// golden is the -h output of cmd/coordinator from before it was split.
func TestCoordinatorFlags_AreWhatTheMainAlwaysTook(t *testing.T) {
	fs := flag.NewFlagSet("coordinator", flag.ContinueOnError)
	var out bytes.Buffer
	fs.SetOutput(&out)
	DefineCoordinatorFlags(fs)
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
	fl := DefineCoordinatorFlags(fs)
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

// A coordinator with no way to authenticate its callers must not start, so the
// zero adapter is refused with a message that says why.
func TestAdapter_RequiresAnIdentity(t *testing.T) {
	err := Adapter{}.validate()
	if err == nil || !strings.Contains(err.Error(), "Identity") {
		t.Fatalf("validate() = %v, want an error naming Identity", err)
	}
	if err := (Adapter{Identity: noIdentity{}}).validate(); err != nil {
		t.Errorf("an adapter with an Identity was refused: %v", err)
	}
}

// The open adapter is a usable adapter: every optional part may be nil.
func TestAdapter_OpenAdapterNeedsNothingElse(t *testing.T) {
	a := Adapter{
		Coordinator: coordinator.Adapter{Orgs: single.Orgs{}},
		Identity:    noIdentity{},
	}
	if err := a.validate(); err != nil {
		t.Fatal(err)
	}
	if got := a.retention(nil, "us-east-2"); got != nil {
		t.Errorf("an adapter without Retention built one: %v", got)
	}
	if a.notifier(nil) == nil {
		t.Error("an adapter without a Notifier built none: the alert stage needs one that sends no email")
	}
	// the open adapter has no organizations: its mapper yields none under every
	// shape, whether or not the shape has a tenant column
	if org := a.Coordinator.Orgs.OrgOf(shapetest.Added(7)); org != 0 {
		t.Errorf("the open adapter's OrgOf = %d, want 0 (no organizations)", org)
	}
}

type noIdentity struct{}

func (noIdentity) Open(*store.Store) identity.Server { return nil }
