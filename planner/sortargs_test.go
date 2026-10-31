package planner

import (
	"reflect"
	"strings"
	"testing"
	"time"
)

// parseSortArgs reads a sort stage the way warp's sort does, and says which
// stages order `key value` lines by their value.
func TestParseSortArgs(t *testing.T) {
	asc := &ValueOrder{}
	desc := &ValueOrder{Reverse: true}
	for _, c := range []struct {
		args  string
		plain bool
		order *ValueOrder
	}{
		{"-k2 -n", true, asc},
		{"-k2,2 -n", true, asc},
		{"-k 2 -n", true, asc},
		{"-n -k2", true, asc},
		{"-k2n", true, asc},
		{"-k2,2n", true, asc},
		{"-k2 -rn", true, &ValueOrder{Reverse: true, TieReverse: true}},
		{"-r -n -k2", true, &ValueOrder{Reverse: true, TieReverse: true}},
		{"-k2 -nr", true, &ValueOrder{Reverse: true, TieReverse: true}},
		// The key's own flags are the key's: -k2,2nr reverses the key, not the
		// tie-break; a global -r alongside does both.
		{"-k2,2nr", true, desc},
		{"-k2nr", true, desc},
		{"-k2,2rn", true, desc},
		{"-k2,2nr -r", true, &ValueOrder{Reverse: true, TieReverse: true}},
		{"-k2,2n -r", true, &ValueOrder{TieReverse: true}},
		// With a modifier on the key, the global -n does not reach it.
		{"-n -k2r", true, nil},
		// Plain, but not an order by the value.
		{"", true, nil},
		{"-n", true, nil},
		{"-rn", true, nil},
		{"-k2", true, nil},
		{"-k1 -n", true, nil},
		{"-k3 -n", true, nil},
		{"-k2,3n", true, nil},
		{"-k2n -k1", true, nil},
		// Not plain: -u, -t, files, unknown or malformed keys.
		{"-k2 -n -u", false, nil},
		{"-un", false, nil},
		{"-t , -k2 -n", false, nil},
		{"-k2 -n file", false, nil},
		{"-nk2", false, nil},
		{"-k", false, nil},
		{"-k0", false, nil},
		{"-k2,1", false, nil},
		{"-k2.1 -n", false, nil},
		{"-k2,2,2", false, nil},
		{"-z", false, nil},
		{"-", false, nil},
	} {
		got := parseSortArgs(strings.Fields(c.args))
		if got.plain != c.plain || !reflect.DeepEqual(got.order, c.order) {
			t.Errorf("sort %s: plain=%v order=%+v, want plain=%v order=%+v", c.args, got.plain, got.order, c.plain, c.order)
		}
	}
}

// The fold shows in Explain: the dropped sort and the order say what they are,
// and the plan has no compute stages left.
func TestKeyedAggregate_ExplainShowsTheFoldedSorts(t *testing.T) {
	out := Explain("cat /log/ns/svc/main.log | sort -k2 -n | awk '{ c[$2]++ } END { for (k in c) print k, c[k] }' | sort -k2 -rn | head -n 3")
	for _, want := range []string{"dropped: the aggregate does not depend on row order", "applied to the aggregate's lines in compute", "keeps the first of them", "Compute stages: (none"} {
		if !strings.Contains(out, want) {
			t.Errorf("explain lacks %q:\n%s", want, out)
		}
	}
}

// A saved count with an ordering carries the order: a cron fire sends it too.
func TestKeyedAggregate_ASavedCountKeepsItsOrder(t *testing.T) {
	lp, err := Lower("cat /log/ns/svc/main.log | awk '{ c[$1]++ } END { for (k in c) print k, c[k] }' | sort -k2 -rn | head -n 2", 1, "bolt", noResolver{})
	if err != nil {
		t.Fatal(err)
	}
	want := &ValueOrder{Reverse: true, TieReverse: true}
	if lp.Group == nil || !reflect.DeepEqual(lp.Group.Order, want) || lp.Group.Limit != 2 || len(lp.ComputeStages) != 0 {
		t.Errorf("lowered group %+v compute %v, want order %+v limit 2 and no stages", lp.Group, lp.ComputeStages, want)
	}
}

// Dropping a sort ahead of a sum is the plan for shards with the facts; the
// base plan keeps both stages, since the sum alone cannot be lowered there.
func TestKeyedAggregate_ASortBeforeASumNeedsTheSameFacts(t *testing.T) {
	plan, err := Plan("cat /log/ns/svc/main.log | sort -n | awk '"+keyedSum+"'", 1, "", time.Time{}, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.ComputeStages) != 2 || plan.Group != nil {
		t.Errorf("base plan: compute %v group %+v, want both stages", plan.ComputeStages, plan.Group)
	}
	if plan.Typed == nil || strings.Join(plan.Typed.Requires, ",") != "$2,$2#int" || len(plan.Typed.Exact.ComputeStages) != 0 {
		t.Errorf("typed %+v, want it to require $2,$2#int and the exact plan to need no stages", plan.Typed)
	}
}
