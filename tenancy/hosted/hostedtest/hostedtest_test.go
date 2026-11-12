package hostedtest_test

import (
	"reflect"
	"testing"

	"github.com/graybeardcorp/weft/coordinator"
	"github.com/graybeardcorp/weft/store"
	"github.com/graybeardcorp/weft/tenancy/hosted/hostedapp"
	_ "github.com/graybeardcorp/weft/tenancy/hosted/hostedtest"
)

// Tests of a hosted build install their tenancy through this package, and
// production passes it through hostedapp. They are written separately because
// this package cannot import hostedapp without a cycle, so a test keeps them
// from drifting.
func TestInstalled_IsWhatProductionPasses(t *testing.T) {
	want := hostedapp.Tenancy()
	got := coordinator.DefaultAdapter
	var db *store.Store
	if reflect.TypeOf(got.Orgs) != reflect.TypeOf(want.Orgs) {
		t.Errorf("Orgs %T, production passes %T", got.Orgs, want.Orgs)
	}
	if reflect.TypeOf(got.Scoper(db)) != reflect.TypeOf(want.Scoper(db)) {
		t.Errorf("Scoper %T, production passes %T", got.Scoper(db), want.Scoper(db))
	}
	if reflect.TypeOf(got.Authorizer(db)) != reflect.TypeOf(want.Authorizer(db)) {
		t.Errorf("Authorizer %T, production passes %T", got.Authorizer(db), want.Authorizer(db))
	}
}
