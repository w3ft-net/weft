package planner

import (
	"testing"
	"time"

	"github.com/w3ft-net/weft/shape/shapetest"
)

// A delegated client ships its cwd with each pipeline (the dirstack
// courier). PlanInDir resolves a relative source argument against that
// cwd, so `tail gamestate.log` typed inside /log/vimgym/api reads
// /log/vimgym/api/gamestate.log rather than being mis-parsed as the
// namespace "gamestate". cwd "" / "/" (a fresh shell at home) keeps the
// legacy lexical parsing so bare `log/...` paths and the `warp.log`
// wildcard are untouched.

func TestPlanInDir_RelativeSourceResolvesAgainstCwd(t *testing.T) {
	plan, err := Plan("tail gamestate.log", shapetest.Group(2), "/log/vimgym/api", time.Time{}, time.Time{})
	if err != nil {
		t.Fatalf("PlanInDir: %v", err)
	}
	if plan.SourceNamespace != "vimgym" {
		t.Errorf("SourceNamespace = %q, want vimgym", plan.SourceNamespace)
	}
	if plan.SourceService != "api" {
		t.Errorf("SourceService = %q, want api", plan.SourceService)
	}
	if plan.SourceKind != "gamestate" {
		t.Errorf("SourceKind = %q, want gamestate", plan.SourceKind)
	}
}

// A relative path one level down from the namespace root resolves the
// same way: `cat api/main.log` from /log/vimgym → /log/vimgym/api/main.log.
func TestPlanInDir_RelativeFromNamespaceRoot(t *testing.T) {
	plan, err := Plan("cat api/main.log", shapetest.Group(2), "/log/vimgym", time.Time{}, time.Time{})
	if err != nil {
		t.Fatalf("PlanInDir: %v", err)
	}
	if plan.SourceNamespace != "vimgym" || plan.SourceService != "api" || plan.SourceKind != "main" {
		t.Errorf("got ns=%q svc=%q kind=%q, want vimgym/api/main", plan.SourceNamespace, plan.SourceService, plan.SourceKind)
	}
}

// Absolute paths are untouched regardless of cwd.
func TestPlanInDir_AbsolutePathIgnoresCwd(t *testing.T) {
	plan, err := Plan("tail /log/acme/api/main.log", shapetest.Group(5), "/log/vimgym/api", time.Time{}, time.Time{})
	if err != nil {
		t.Fatalf("PlanInDir: %v", err)
	}
	if plan.SourceNamespace != "acme" || plan.SourceService != "api" || plan.SourceKind != "main" {
		t.Errorf("got ns=%q svc=%q kind=%q, want acme/api/main", plan.SourceNamespace, plan.SourceService, plan.SourceKind)
	}
}

// A relative glob (`tail *.log`) expands against cwd too: the pattern is
// resolved to /log/vimgym/api/*.log before matching SeenServices. cwd ""
// /"/" leaves it lexical, so `*.log` from home still fails (unsupported
// directory) exactly as before.
func TestExpandSourceGlobs_RelativeGlobResolvesAgainstCwd(t *testing.T) {
	apps := []string{"vimgym/api/gamestate", "vimgym/api/heartbeats"}
	got, err := ExpandSourceGlobs("tail *.log", "/log/vimgym/api", apps)
	if err != nil {
		t.Fatalf("ExpandSourceGlobs: %v", err)
	}
	want := "tail /log/vimgym/api/gamestate.log /log/vimgym/api/heartbeats.log"
	if got != want {
		t.Errorf("\n got:  %s\nwant: %s", got, want)
	}
}

// cwd "/" (home) preserves the legacy lexical forms: a bare `log/...`
// path still parses, and the `warp.log` wildcard still fans out (empty
// namespace = no source predicate).
func TestPlanInDir_HomeCwdPreservesLegacyForms(t *testing.T) {
	plan, err := Plan("tail log/vimgym/api/gamestate.log", shapetest.Group(2), "/", time.Time{}, time.Time{})
	if err != nil {
		t.Fatalf("PlanInDir(log/...): %v", err)
	}
	if plan.SourceNamespace != "vimgym" || plan.SourceService != "api" || plan.SourceKind != "gamestate" {
		t.Errorf("log/ form: got ns=%q svc=%q kind=%q, want vimgym/api/gamestate", plan.SourceNamespace, plan.SourceService, plan.SourceKind)
	}

	wild, err := Plan("cat warp.log", shapetest.Group(2), "/", time.Time{}, time.Time{})
	if err != nil {
		t.Fatalf("PlanInDir(warp.log): %v", err)
	}
	if wild.SourceNamespace != "" {
		t.Errorf("warp.log wildcard: SourceNamespace = %q, want empty (fan-out)", wild.SourceNamespace)
	}
}
