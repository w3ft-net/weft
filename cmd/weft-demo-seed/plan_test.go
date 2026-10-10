package main

import (
	"reflect"
	"testing"
)

func specFor(t *testing.T, name string) customerSpec {
	t.Helper()
	for _, c := range spec {
		if c.Name == name {
			return c
		}
	}
	t.Fatalf("no %q in spec", name)
	return customerSpec{}
}

// Loom's dev stack runs the same Go cmd/loom-producer as prod, which writes
// a bare trace ID as every line's third field, so Loom's schema must point
// trace_id_field at it. acme/globex still run the Python generator, whose
// third field is the event name — pointing trace_id_field there would put
// garbage ("request") in the reserved trace_id column.
func TestEmitsTraceID(t *testing.T) {
	for name, want := range map[string]bool{"loom": true, "acme": false, "globex": false} {
		if got := emitsTraceID(name); got != want {
			t.Errorf("emitsTraceID(%q) = %v, want %v", name, got, want)
		}
	}
}

// Loom mirrors the prod producer host: one agent token per env, shared by
// the env's three apps (a single weftd forwards api+worker+edge). The other
// demo customers keep one token per (app, env).
func TestTokenPlan_LoomIsPerEnv(t *testing.T) {
	got := tokenPlan(specFor(t, "loom"))
	want := []tokenPlanEntry{
		{Label: "loom-sand", Var: "LOOM_SAND_TOKEN", Env: "sand"},
		{Label: "loom-prod", Var: "LOOM_PROD_TOKEN", Env: "prod"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("tokenPlan(loom) = %+v, want %+v", got, want)
	}
}

func TestTokenPlan_OthersArePerAppAndEnv(t *testing.T) {
	got := tokenPlan(specFor(t, "acme"))
	if len(got) != 6 {
		t.Fatalf("tokenPlan(acme) has %d entries, want 6 (3 apps x 2 envs): %+v", len(got), got)
	}
	want := tokenPlanEntry{Label: "acme-api-sand", Var: "ACME_API_SAND_TOKEN", Env: "sand"}
	if got[0] != want {
		t.Errorf("first acme token = %+v, want %+v", got[0], want)
	}
}
