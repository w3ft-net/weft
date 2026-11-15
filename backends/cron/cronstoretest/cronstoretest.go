// Package cronstoretest is the conformance suite for cron.Store: the
// behaviour every implementation of the cron domain (Postgres for the hosted
// build, SQLite for the open one) must show. An implementation's test calls Run
// with a constructor.
package cronstoretest

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"reflect"
	"testing"
	"time"

	"github.com/w3ft-net/weft/backends/cron"
	"github.com/w3ft-net/weft/shape"
)

// Subject is one store under test, fresh and empty.
type Subject struct {
	Store cron.Store
	// Owner owns the watches the suite saves. The open build's is the empty value.
	Owner shape.Added
	// Other is a second owner, for a store that has owners to tell apart. Nil for
	// one that has none.
	Other *shape.Added
	// Namespace makes a namespace for the owner that watches can be homed in. The
	// namespaces are another domain's, so the subject makes one in that domain's
	// terms, in the same database.
	Namespace func(t *testing.T, owner shape.Added, name string) uint
	// DeleteNamespace deletes a namespace made by Namespace, as the sources domain
	// does.
	DeleteNamespace func(t *testing.T, owner shape.Added, name string)
}

// Run runs the suite, building a fresh Subject for each test.
func Run(t *testing.T, newSubject func(t *testing.T) Subject) {
	cases := []struct {
		name string
		fn   func(*testing.T, Subject)
	}{
		{"SaveWatch", saveWatch},
		{"SaveWatchOverwrites", saveWatchOverwrites},
		{"DeleteWatch", deleteWatch},
		{"ListWatches", listWatches},
		{"WatchesBelongToTheirOwner", watchOwners},
		{"AmendMessage", amendMessage},
		{"MoveWatch", moveWatch},
		{"DeletingANamespaceDeletesItsWatches", deletingANamespace},
		{"CountEnabled", countEnabled},
		{"ListEnabled", listEnabled},
		{"RecordRun", recordRun},
		{"SetLoweredPlan", setLoweredPlan},
		{"RecordStrike", recordStrike},
		{"Disable", disable},
		{"SaveAndListResults", saveAndListResults},
		{"LatestRun", latestRun},
		{"CountResults", countResults},
		{"NumericEnds", numericEnds},
		{"NumericPoints", numericPoints},
		{"WindowStats", windowStats},
		{"Series", series},
		{"Incidents", incidents},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) { c.fn(t, newSubject(t)) })
	}
}

// The pipelines the suite saves, and what compiling each gives. The caller
// compiles in production; the suite writes the answers down, so a store that
// derives them itself is held to the same ones.
const (
	plainPipeline = "echo 1"
	alertPipeline = "echo 5 | alert above 3"
	textPipeline  = "# type: text\necho hi"
)

var (
	plain = cron.Compiled{ValueType: cron.ValueNumber}
	above = cron.Compiled{
		ValueType: cron.ValueNumber, HasAlert: true,
		AlertCondition: ptr("above"), AlertThreshold: ptr(3.0),
	}
	text = cron.Compiled{ValueType: cron.ValueText}
)

func ptr[T any](v T) *T { return &v }

// sameJSON reports whether a and b are the same JSON document.
func sameJSON(a, b []byte) bool {
	var x, y any
	return json.Unmarshal(a, &x) == nil && json.Unmarshal(b, &y) == nil && reflect.DeepEqual(x, y)
}

func spec(schedule, pipeline string, enabled bool, c cron.Compiled) cron.WatchSpec {
	return cron.WatchSpec{Schedule: schedule, Pipeline: pipeline, Enabled: enabled, Compiled: c}
}

// save saves a plain enabled watch and returns it.
func save(t *testing.T, s Subject, ns uint, name string) *cron.Watch {
	t.Helper()
	if err := s.Store.SaveWatch(s.Owner, ns, "", name, spec("every 1m", plainPipeline, true, plain)); err != nil {
		t.Fatalf("SaveWatch %q: %v", name, err)
	}
	return get(t, s, ns, name)
}

func get(t *testing.T, s Subject, ns uint, name string) *cron.Watch {
	t.Helper()
	w, err := s.Store.GetWatch(s.Owner, ns, name)
	if err != nil {
		t.Fatalf("GetWatch %q: %v", name, err)
	}
	return w
}

func saveWatch(t *testing.T, s Subject) {
	ns := s.Namespace(t, s.Owner, "ops")
	if err := s.Store.SaveWatch(s.Owner, ns, "user-1", "errors", spec("*/5 * * * *", alertPipeline, true, above)); err != nil {
		t.Fatal(err)
	}
	w := get(t, s, ns, "errors")
	if w.ID == 0 || w.Namespace != ns || w.Name != "errors" || w.Owner != s.Owner || w.Schedule != "*/5 * * * *" ||
		w.Pipeline != alertPipeline || !w.Enabled || w.CreatedBy != "user-1" || w.CreatedAt.IsZero() ||
		w.ConsecutiveStrikes != 0 || w.LastRunAt != nil || w.LastStatus != "" || len(w.ContentHash) != 64 {
		t.Fatalf("saved watch: %+v", w)
	}
	c := w.Compiled
	if !c.HasAlert || c.ValueType != cron.ValueNumber || c.AlertCondition == nil || *c.AlertCondition != "above" ||
		c.AlertThreshold == nil || *c.AlertThreshold != 3 || c.ValueField != nil || c.TagField != nil {
		t.Fatalf("compiled facts: %+v", c)
	}
	// Text type round-trips; the plain watch has no alert facts.
	if err := s.Store.SaveWatch(s.Owner, ns, "", "notes", spec("every 1h", textPipeline, true, text)); err != nil {
		t.Fatal(err)
	}
	if n := get(t, s, ns, "notes"); n.Compiled.ValueType != cron.ValueText || n.Compiled.HasAlert || n.Compiled.AlertCondition != nil || n.CreatedBy != "" {
		t.Fatalf("text watch: %+v", n)
	}
	// The same name at the owner level is a different watch.
	top := save(t, s, 0, "errors")
	if top.ID == w.ID || top.Namespace != 0 {
		t.Fatalf("owner-level watch: %+v beside %+v", top, w)
	}
	if _, err := s.Store.GetWatch(s.Owner, ns, "absent"); !errors.Is(err, cron.ErrNotFound) {
		t.Fatalf("GetWatch of an absent watch: %v, want ErrNotFound", err)
	}
	// A plan round-trips as JSON (a store may reformat it).
	plan := []byte(`{"shard_sql":"select 1"}`)
	sp := spec("every 1m", plainPipeline, true, plain)
	sp.LoweredPlan = plan
	if err := s.Store.SaveWatch(s.Owner, ns, "", "planned", sp); err != nil {
		t.Fatal(err)
	}
	if got := get(t, s, ns, "planned").LoweredPlan; !sameJSON(got, plan) {
		t.Fatalf("lowered plan %q, want %q", got, plan)
	}
}

func saveWatchOverwrites(t *testing.T, s Subject) {
	ns := s.Namespace(t, s.Owner, "ops")
	if err := s.Store.SaveWatch(s.Owner, ns, "author", "w", spec("every 1m", plainPipeline, true, plain)); err != nil {
		t.Fatal(err)
	}
	first := get(t, s, ns, "w")
	if err := s.Store.SaveWatch(s.Owner, ns, "someone-else", "w", spec("every 5m", alertPipeline, true, above)); err != nil {
		t.Fatal(err)
	}
	w := get(t, s, ns, "w")
	if w.ID != first.ID || w.Schedule != "every 5m" || w.Pipeline != alertPipeline || !w.Compiled.HasAlert ||
		w.ContentHash == first.ContentHash || w.CreatedBy != "author" {
		t.Fatalf("after overwrite: %+v (was %+v): same row, new content, original creator", w, first)
	}

	// Strikes are wiped by a save that turns a benched watch back on, not by one
	// that leaves it as it was.
	if _, err := s.Store.RecordStrike(context.Background(), w.ID, 10); err != nil {
		t.Fatal(err)
	}
	if err := s.Store.SaveWatch(s.Owner, ns, "", "w", spec("every 5m", alertPipeline, true, above)); err != nil {
		t.Fatal(err)
	}
	if got := get(t, s, ns, "w"); got.ConsecutiveStrikes != 1 {
		t.Fatalf("an unchanged save changed strikes: %d, want 1", got.ConsecutiveStrikes)
	}
	if err := s.Store.SaveWatch(s.Owner, ns, "", "w", spec("every 5m", alertPipeline, false, above)); err != nil {
		t.Fatal(err)
	}
	if got := get(t, s, ns, "w"); got.Enabled {
		t.Fatal("a save did not turn the watch off")
	}
	if err := s.Store.SaveWatch(s.Owner, ns, "", "w", spec("every 5m", alertPipeline, true, above)); err != nil {
		t.Fatal(err)
	}
	if got := get(t, s, ns, "w"); !got.Enabled || got.ConsecutiveStrikes != 0 {
		t.Fatalf("re-enabling: enabled=%v strikes=%d, want on and 0", got.Enabled, got.ConsecutiveStrikes)
	}
}

func deleteWatch(t *testing.T, s Subject) {
	ns := s.Namespace(t, s.Owner, "ops")
	w := save(t, s, ns, "w")
	if err := s.Store.DeleteWatch(s.Owner, ns, "w"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Store.GetWatch(s.Owner, ns, "w"); !errors.Is(err, cron.ErrNotFound) {
		t.Fatalf("a deleted watch: %v, want ErrNotFound", err)
	}
	if list, _ := s.Store.ListWatches(s.Owner); len(list) != 0 {
		t.Fatalf("a deleted watch is listed: %+v", list)
	}
	if err := s.Store.DeleteWatch(s.Owner, ns, "w"); err != nil {
		t.Fatalf("deleting twice: %v", err)
	}
	// Saving the name again brings it back as the same row.
	if again := save(t, s, ns, "w"); again.ID != w.ID {
		t.Fatalf("revived as id %d, want the same row %d", again.ID, w.ID)
	}
}

func listWatches(t *testing.T, s Subject) {
	ns := s.Namespace(t, s.Owner, "ops")
	save(t, s, ns, "b")
	save(t, s, 0, "c")
	save(t, s, ns, "a")
	list, err := s.Store.ListWatches(s.Owner)
	if err != nil || len(list) != 3 || list[0].Name != "a" || list[1].Name != "b" || list[2].Name != "c" {
		t.Fatalf("ListWatches: %+v err=%v, want a, b, c", list, err)
	}
}

func watchOwners(t *testing.T, s Subject) {
	if s.Other == nil {
		t.Skip("the store has no owners to tell apart")
	}
	ns := s.Namespace(t, s.Owner, "ops")
	theirNS := s.Namespace(t, *s.Other, "ops")
	save(t, s, ns, "mine")
	if err := s.Store.SaveWatch(*s.Other, theirNS, "", "theirs", spec("every 1m", plainPipeline, true, plain)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Store.GetWatch(*s.Other, ns, "mine"); !errors.Is(err, cron.ErrNotFound) {
		t.Fatalf("another owner read my watch: %v", err)
	}
	list, _ := s.Store.ListWatches(*s.Other)
	if len(list) != 1 || list[0].Name != "theirs" || list[0].Owner != *s.Other {
		t.Fatalf("the other owner lists %+v", list)
	}
	if n, _ := s.Store.CountEnabled(*s.Other); n != 1 {
		t.Fatalf("the other owner counts %d, want 1", n)
	}
	if err := s.Store.DeleteWatch(*s.Other, ns, "mine"); err != nil {
		t.Fatal(err)
	}
	get(t, s, ns, "mine") // still there
	if err := s.Store.MoveWatch(*s.Other, ns, "mine", ns, "stolen"); !errors.Is(err, cron.ErrNotFound) {
		t.Fatalf("moving another owner's watch: %v, want ErrNotFound", err)
	}
}

func moveWatch(t *testing.T, s Subject) {
	ops := s.Namespace(t, s.Owner, "ops")
	app := s.Namespace(t, s.Owner, "app")
	w := save(t, s, ops, "w")
	save(t, s, app, "taken")
	if err := s.Store.SaveResults([]cron.Result{{WatchID: w.ID, RanAt: time.Now().Add(-time.Minute), Stdout: "1", Value: ptr(1.0)}}); err != nil {
		t.Fatal(err)
	}
	if err := s.Store.MoveWatch(s.Owner, ops, "w", app, "taken"); !errors.Is(err, cron.ErrExists) {
		t.Fatalf("moving onto a held name: %v, want ErrExists", err)
	}
	if err := s.Store.MoveWatch(s.Owner, ops, "absent", app, "x"); !errors.Is(err, cron.ErrNotFound) {
		t.Fatalf("moving an absent watch: %v, want ErrNotFound", err)
	}
	if err := s.Store.MoveWatch(s.Owner, ops, "w", app, "renamed"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Store.GetWatch(s.Owner, ops, "w"); !errors.Is(err, cron.ErrNotFound) {
		t.Fatalf("the old home still has it: %v", err)
	}
	moved := get(t, s, app, "renamed")
	if moved.ID != w.ID {
		t.Fatalf("moved to id %d, want the same row %d", moved.ID, w.ID)
	}
	if rs, _ := s.Store.ListResults(moved.ID, 10, ""); len(rs) != 1 {
		t.Fatalf("results did not move with the watch: %+v", rs)
	}
	// To the owner level and back.
	if err := s.Store.MoveWatch(s.Owner, app, "renamed", 0, "top"); err != nil {
		t.Fatal(err)
	}
	if top := get(t, s, 0, "top"); top.ID != w.ID || top.Namespace != 0 {
		t.Fatalf("owner-level move: %+v", top)
	}
}

// A watch homed in a namespace does not outlive it: deleting the namespace takes
// its watches, and what they recorded, and no other home's.
func deletingANamespace(t *testing.T, s Subject) {
	ops := s.Namespace(t, s.Owner, "ops")
	app := s.Namespace(t, s.Owner, "app")
	gone := save(t, s, ops, "gone")
	if err := s.Store.DeleteWatch(s.Owner, ops, "gone"); err != nil { // a deleted one goes too
		t.Fatal(err)
	}
	live := save(t, s, ops, "live")
	keep := save(t, s, app, "keep")
	top := save(t, s, 0, "top")
	for _, w := range []*cron.Watch{gone, live, keep, top} {
		if err := s.Store.SaveResults([]cron.Result{num(w.ID, ago(time.Minute), 1, "")}); err != nil {
			t.Fatal(err)
		}
	}
	inc, err := s.Store.OpenOrTouchIncident(live.ID, "")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Store.RecordDeliveries(inc.ID, []string{"pager"}); err != nil {
		t.Fatal(err)
	}

	s.DeleteNamespace(t, s.Owner, "ops")

	if _, err := s.Store.GetWatch(s.Owner, ops, "live"); !errors.Is(err, cron.ErrNotFound) {
		t.Fatalf("a watch outlived its namespace: %v", err)
	}
	for _, w := range []*cron.Watch{gone, live} {
		if rs, _ := s.Store.ListResults(w.ID, 10, ""); len(rs) != 0 {
			t.Fatalf("watch %d's results outlived it: %+v", w.ID, rs)
		}
	}
	if d, _ := s.Store.IncidentDeliveries(inc.ID); len(d) != 0 {
		t.Fatalf("an incident's deliveries outlived its watch: %+v", d)
	}
	if again, err := s.Store.OpenOrTouchIncident(keep.ID, ""); err != nil || again.WatchID != keep.ID {
		t.Fatalf("another namespace's incidents: %+v err=%v", again, err)
	}
	list, _ := s.Store.ListWatches(s.Owner)
	if len(list) != 2 {
		t.Fatalf("after the delete: %+v, want the other namespace's watch and the owner-level one", list)
	}
	for _, w := range []*cron.Watch{keep, top} {
		if rs, _ := s.Store.ListResults(w.ID, 10, ""); len(rs) != 1 {
			t.Fatalf("watch %d lost its results: %+v", w.ID, rs)
		}
	}
}

func countEnabled(t *testing.T, s Subject) {
	ns := s.Namespace(t, s.Owner, "ops")
	save(t, s, ns, "a")
	save(t, s, ns, "b")
	if err := s.Store.SaveWatch(s.Owner, ns, "", "off", spec("every 1m", plainPipeline, false, plain)); err != nil {
		t.Fatal(err)
	}
	if n, err := s.Store.CountEnabled(s.Owner); err != nil || n != 2 {
		t.Fatalf("CountEnabled = %d err=%v, want 2: a disabled watch is free", n, err)
	}
	if err := s.Store.DeleteWatch(s.Owner, ns, "a"); err != nil {
		t.Fatal(err)
	}
	if n, _ := s.Store.CountEnabled(s.Owner); n != 1 {
		t.Fatalf("after a delete: %d, want 1", n)
	}
}

func listEnabled(t *testing.T, s Subject) {
	ns := s.Namespace(t, s.Owner, "ops")
	save(t, s, ns, "on")
	if err := s.Store.SaveWatch(s.Owner, ns, "", "off", spec("every 1m", plainPipeline, false, plain)); err != nil {
		t.Fatal(err)
	}
	want := map[string]shape.Added{"on": s.Owner}
	if s.Other != nil {
		theirNS := s.Namespace(t, *s.Other, "ops")
		if err := s.Store.SaveWatch(*s.Other, theirNS, "", "theirs", spec("every 1m", plainPipeline, true, plain)); err != nil {
			t.Fatal(err)
		}
		want["theirs"] = *s.Other
	}
	list, err := s.Store.ListEnabled(context.Background())
	if err != nil || len(list) != len(want) {
		t.Fatalf("ListEnabled: %+v err=%v, want %d watches across owners, none disabled", list, err, len(want))
	}
	for _, w := range list {
		if owner, ok := want[w.Name]; !ok || w.Owner != owner || !w.Enabled {
			t.Errorf("listed %q owned by %+v enabled=%v", w.Name, w.Owner, w.Enabled)
		}
	}
}

func recordRun(t *testing.T, s Subject) {
	ctx := context.Background()
	w := save(t, s, 0, "w")
	if _, err := s.Store.RecordStrike(ctx, w.ID, 10); err != nil {
		t.Fatal(err)
	}
	at := time.Now().Add(-time.Minute).Truncate(time.Millisecond)
	// An errored run records the status and keeps the strikes.
	if err := s.Store.RecordRun(ctx, w.ID, at, cron.StatusError, false); err != nil {
		t.Fatal(err)
	}
	got := get(t, s, 0, "w")
	if got.LastRunAt == nil || !got.LastRunAt.Equal(at) || got.LastStatus != cron.StatusError || got.ConsecutiveStrikes != 1 {
		t.Fatalf("after an errored run: %+v", got)
	}
	// A clean run resets them.
	if err := s.Store.RecordRun(ctx, w.ID, at.Add(time.Second), cron.StatusOK, true); err != nil {
		t.Fatal(err)
	}
	if got := get(t, s, 0, "w"); got.LastStatus != cron.StatusOK || got.ConsecutiveStrikes != 0 {
		t.Fatalf("after a clean run: %+v", got)
	}
	if err := s.Store.RecordRun(ctx, 999999, at, cron.StatusOK, true); !errors.Is(err, cron.ErrNotFound) {
		t.Fatalf("recording a run of an absent watch: %v, want ErrNotFound", err)
	}
}

func setLoweredPlan(t *testing.T, s Subject) {
	ctx := context.Background()
	w := save(t, s, 0, "w")
	if err := s.Store.SetLoweredPlan(ctx, w.ID, []byte(`{"a":1}`)); err != nil {
		t.Fatal(err)
	}
	if got := get(t, s, 0, "w").LoweredPlan; !sameJSON(got, []byte(`{"a":1}`)) {
		t.Fatalf("plan %q", got)
	}
	if err := s.Store.SetLoweredPlan(ctx, w.ID, nil); err != nil {
		t.Fatal(err)
	}
	if got := get(t, s, 0, "w").LoweredPlan; len(got) != 0 {
		t.Fatalf("a cleared plan reads %q", got)
	}
}

func recordStrike(t *testing.T, s Subject) {
	ctx := context.Background()
	w := save(t, s, 0, "w")
	for i := 1; i <= 2; i++ {
		benched, err := s.Store.RecordStrike(ctx, w.ID, 3)
		if err != nil || benched {
			t.Fatalf("strike %d: benched=%v err=%v, want neither", i, benched, err)
		}
	}
	if got := get(t, s, 0, "w"); got.ConsecutiveStrikes != 2 || !got.Enabled {
		t.Fatalf("after two strikes: %+v", got)
	}
	if benched, err := s.Store.RecordStrike(ctx, w.ID, 3); err != nil || !benched {
		t.Fatalf("the third strike: benched=%v err=%v, want it benched", benched, err)
	}
	if got := get(t, s, 0, "w"); got.Enabled || got.ConsecutiveStrikes != 3 {
		t.Fatalf("after the third strike: %+v, want disabled with 3", got)
	}
	if _, err := s.Store.RecordStrike(ctx, 999999, 3); !errors.Is(err, cron.ErrNotFound) {
		t.Fatalf("a strike on an absent watch: %v, want ErrNotFound", err)
	}
}

func disable(t *testing.T, s Subject) {
	ctx := context.Background()
	w := save(t, s, 0, "w")
	if did, err := s.Store.Disable(ctx, w.ID); err != nil || !did {
		t.Fatalf("Disable: did=%v err=%v, want it to disable", did, err)
	}
	if get(t, s, 0, "w").Enabled {
		t.Fatal("the watch is still enabled")
	}
	if did, err := s.Store.Disable(ctx, w.ID); err != nil || did {
		t.Fatalf("Disable again: did=%v err=%v, want false: it was already off", did, err)
	}
}

// ago is a time that long before now.
func ago(d time.Duration) time.Time { return time.Now().Add(-d).Truncate(time.Millisecond) }

func num(w uint, at time.Time, v float64, tag string) cron.Result {
	return cron.Result{WatchID: w, RanAt: at, Stdout: "x", Value: ptr(v), Tag: tag}
}

func saveAndListResults(t *testing.T, s Subject) {
	w := save(t, s, 0, "w")
	if err := s.Store.SaveResults(nil); err != nil {
		t.Fatalf("an empty batch: %v", err)
	}
	r := cron.Result{WatchID: w.ID, RanAt: ago(3 * time.Minute), Stdout: "out", Stderr: "err", ExitCode: 2, DurationMs: 150, Value: ptr(4.5), Tag: "host-a"}
	txt := cron.Result{WatchID: w.ID, RanAt: ago(2 * time.Minute), Stdout: "ok", TextValue: ptr("ok")}
	third := num(w.ID, ago(time.Minute), 9, "")
	if err := s.Store.SaveResults([]cron.Result{r, txt, third}); err != nil {
		t.Fatal(err)
	}
	got, err := s.Store.ListResults(w.ID, 10, "")
	if err != nil || len(got) != 3 {
		t.Fatalf("ListResults: %+v err=%v", got, err)
	}
	if got[0].Value == nil || *got[0].Value != 9 || got[1].TextValue == nil || *got[1].TextValue != "ok" || got[1].Value != nil {
		t.Fatalf("newest first: %+v", got)
	}
	back := got[2]
	if back.ID == 0 || back.WatchID != w.ID || !back.RanAt.Equal(r.RanAt) || back.Stdout != "out" || back.Stderr != "err" ||
		back.ExitCode != 2 || back.DurationMs != 150 || back.Value == nil || *back.Value != 4.5 || back.Tag != "host-a" || back.TextValue != nil {
		t.Fatalf("a result round trip: %+v", back)
	}
	if two, _ := s.Store.ListResults(w.ID, 2, ""); len(two) != 2 {
		t.Fatalf("limit 2 returned %d", len(two))
	}
	if tagged, _ := s.Store.ListResults(w.ID, 10, "host-a"); len(tagged) != 1 || tagged[0].Tag != "host-a" {
		t.Fatalf("tag filter: %+v", tagged)
	}
	other := save(t, s, 0, "other")
	if rs, _ := s.Store.ListResults(other.ID, 10, ""); len(rs) != 0 {
		t.Fatalf("another watch's results: %+v", rs)
	}
}

func latestRun(t *testing.T, s Subject) {
	w := save(t, s, 0, "w")
	if rs, err := s.Store.LatestRun(w.ID); err != nil || len(rs) != 0 {
		t.Fatalf("LatestRun of a watch that never ran: %+v err=%v", rs, err)
	}
	old := ago(10 * time.Minute)
	latest := ago(time.Minute)
	if err := s.Store.SaveResults([]cron.Result{
		num(w.ID, old, 100, "z"),
		num(w.ID, latest, 5, "b"),
		num(w.ID, latest, 7, "a"),
		num(w.ID, latest, 5, "a"),
		{WatchID: w.ID, RanAt: ago(30 * time.Second), Stdout: "boom", ExitCode: 1}, // a failed run: no value, no text
	}); err != nil {
		t.Fatal(err)
	}
	rs, err := s.Store.LatestRun(w.ID)
	if err != nil || len(rs) != 3 {
		t.Fatalf("LatestRun: %+v err=%v, want the three rows of the newest run that stored a value", rs, err)
	}
	// Numbers highest first, then by tag.
	if *rs[0].Value != 7 || *rs[1].Value != 5 || rs[1].Tag != "a" || *rs[2].Value != 5 || rs[2].Tag != "b" {
		t.Fatalf("order: %+v", rs)
	}
}

func countResults(t *testing.T, s Subject) {
	w := save(t, s, 0, "w")
	if err := s.Store.SaveResults([]cron.Result{
		num(w.ID, ago(5*time.Hour), 1, ""),
		num(w.ID, ago(30*time.Minute), 2, "a"),
		{WatchID: w.ID, RanAt: ago(20 * time.Minute), Stdout: "", Tag: "a"}, // ran, said nothing
		{WatchID: w.ID, RanAt: ago(10 * time.Minute), Stdout: "t", TextValue: ptr("t"), Tag: "b"},
	}); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		name     string
		since    time.Duration
		tag      string
		nonEmpty bool
		want     int64
	}{
		{"all since an hour", time.Hour, "", false, 3},
		{"all since six hours", 6 * time.Hour, "", false, 4},
		{"non-empty since an hour", time.Hour, "", true, 2},
		{"tag a", time.Hour, "a", false, 2},
		{"non-empty tag a", time.Hour, "a", true, 1},
		{"a window with nothing", time.Minute, "", false, 0},
	} {
		got, err := s.Store.CountResults(w.ID, time.Now().Add(-c.since), c.tag, c.nonEmpty)
		if err != nil || got != c.want {
			t.Errorf("%s: %d err=%v, want %d", c.name, got, err, c.want)
		}
	}
}

func numericEnds(t *testing.T, s Subject) {
	w := save(t, s, 0, "w")
	since := time.Now().Add(-time.Hour)
	if e, err := s.Store.NumericEnds(w.ID, since); err != nil || e != (cron.Ends{}) {
		t.Fatalf("no results: %+v err=%v", e, err)
	}
	if err := s.Store.SaveResults([]cron.Result{
		num(w.ID, ago(3*time.Hour), 99, ""), // outside the window
		num(w.ID, ago(50*time.Minute), 10, ""),
		{WatchID: w.ID, RanAt: ago(40 * time.Minute), Stdout: "t", TextValue: ptr("t")}, // not numeric
		num(w.ID, ago(30*time.Minute), 30, ""),
		num(w.ID, ago(10*time.Minute), 20, ""),
	}); err != nil {
		t.Fatal(err)
	}
	e, err := s.Store.NumericEnds(w.ID, since)
	if err != nil || e.N != 3 || e.First != 10 || e.Last != 20 {
		t.Fatalf("NumericEnds = %+v err=%v, want 3 results, first 10, last 20", e, err)
	}
}

func numericPoints(t *testing.T, s Subject) {
	w := save(t, s, 0, "w")
	a, b := ago(40*time.Minute), ago(20*time.Minute)
	if err := s.Store.SaveResults([]cron.Result{
		num(w.ID, b, 2, ""),
		num(w.ID, ago(3*time.Hour), 99, ""),
		num(w.ID, a, 1, "x"),
		{WatchID: w.ID, RanAt: ago(30 * time.Minute), Stdout: "t", TextValue: ptr("t")},
	}); err != nil {
		t.Fatal(err)
	}
	pts, err := s.Store.NumericPoints(w.ID, time.Now().Add(-time.Hour))
	if err != nil || len(pts) != 2 || !pts[0].At.Equal(a) || pts[0].Value != 1 || !pts[1].At.Equal(b) || pts[1].Value != 2 {
		t.Fatalf("NumericPoints = %+v err=%v, want the two numeric points in the window, oldest first", pts, err)
	}
}

func windowStats(t *testing.T, s Subject) {
	w := save(t, s, 0, "w")
	since := time.Now().Add(-time.Hour)
	if st, err := s.Store.WindowStats(w.ID, since, ""); err != nil || st.Count != 0 {
		t.Fatalf("no results: %+v err=%v", st, err)
	}
	if err := s.Store.SaveResults([]cron.Result{
		num(w.ID, ago(3*time.Hour), 1000, ""), // outside the window
		num(w.ID, ago(50*time.Minute), 2, "a"),
		num(w.ID, ago(40*time.Minute), 4, "a"),
		num(w.ID, ago(30*time.Minute), 6, "b"),
		{WatchID: w.ID, RanAt: ago(20 * time.Minute), Stdout: "t", TextValue: ptr("t")}, // never counts
	}); err != nil {
		t.Fatal(err)
	}
	all, err := s.Store.WindowStats(w.ID, since, "")
	if err != nil || all.Count != 3 || all.Sum != 12 || all.SumSq != 56 || all.Min != 2 || all.Max != 6 {
		t.Fatalf("all tags: %+v err=%v, want count 3 sum 12 sumsq 56 min 2 max 6", all, err)
	}
	if all.Mean() != 4 || math.Abs(all.Stddev()-math.Sqrt(56.0/3-16)) > 1e-9 {
		t.Fatalf("mean %v stddev %v", all.Mean(), all.Stddev())
	}
	a, err := s.Store.WindowStats(w.ID, since, "a")
	if err != nil || a.Count != 2 || a.Sum != 6 || a.Min != 2 || a.Max != 4 {
		t.Fatalf("tag a: %+v err=%v", a, err)
	}
	if none, _ := s.Store.WindowStats(w.ID, since, "c"); none.Count != 0 {
		t.Fatalf("an unknown tag: %+v", none)
	}
}

func series(t *testing.T, s Subject) {
	w := save(t, s, 0, "w")
	t1, t2, t3 := ago(50*time.Minute), ago(40*time.Minute), ago(30*time.Minute)
	if err := s.Store.SaveResults([]cron.Result{
		num(w.ID, t3, 3, "b"),
		num(w.ID, t1, 1, "a"),
		num(w.ID, t2, 2, "a"),
		num(w.ID, ago(3*time.Hour), 99, "a"), // before the window
	}); err != nil {
		t.Fatal(err)
	}
	start, end := time.Now().Add(-time.Hour), time.Now()
	got, err := s.Store.Series(w.ID, start, end)
	if err != nil {
		t.Fatal(err)
	}
	if got.Granularity != "raw" || !got.Start.Equal(start) || !got.End.Equal(end) {
		t.Fatalf("series header: %+v, want raw over the asked window", got)
	}
	if len(got.Buckets) != 3 || got.Buckets[0].Tag != "a" || got.Buckets[0].Sum != 1 || got.Buckets[1].Sum != 2 || got.Buckets[2].Tag != "b" {
		t.Fatalf("buckets: %+v, want the three points oldest first, tags kept", got.Buckets)
	}
	for _, b := range got.Buckets {
		if b.Count != 1 || b.Min != b.Sum || b.Max != b.Sum || b.SumSq != b.Sum*b.Sum {
			t.Errorf("a raw bucket should be one value: %+v", b)
		}
	}
	// An empty window is an empty series, not an error.
	if empty, err := s.Store.Series(w.ID, end, start); err != nil || len(empty.Buckets) != 0 {
		t.Fatalf("an inverted window: %+v err=%v", empty, err)
	}
}

func incidents(t *testing.T, s Subject) {
	w := save(t, s, 0, "w")
	inc, err := s.Store.OpenOrTouchIncident(w.ID, "")
	if err != nil || inc.ID == "" || inc.WatchID != w.ID || inc.Tag != "" || inc.ClosedAt != nil || inc.OpenedAt.IsZero() {
		t.Fatalf("OpenOrTouchIncident: %+v err=%v", inc, err)
	}
	again, err := s.Store.OpenOrTouchIncident(w.ID, "")
	if err != nil || again.ID != inc.ID {
		t.Fatalf("a second tick opened another incident: %+v err=%v, want %s", again, err, inc.ID)
	}
	tagged, err := s.Store.OpenOrTouchIncident(w.ID, "host-a")
	if err != nil || tagged.ID == inc.ID {
		t.Fatalf("a tag shares an incident with the untagged: %+v err=%v", tagged, err)
	}

	if d, err := s.Store.IncidentDeliveries(inc.ID); err != nil || len(d) != 0 {
		t.Fatalf("deliveries before any: %+v err=%v", d, err)
	}
	if err := s.Store.RecordDeliveries(inc.ID, []string{"slack:ops", "pager"}); err != nil {
		t.Fatal(err)
	}
	if err := s.Store.RecordDeliveries(inc.ID, []string{"pager", "email"}); err != nil {
		t.Fatalf("recording a target twice: %v", err)
	}
	if err := s.Store.RecordDeliveries(inc.ID, nil); err != nil {
		t.Fatalf("recording none: %v", err)
	}
	d, _ := s.Store.IncidentDeliveries(inc.ID)
	if len(d) != 3 || !d["slack:ops"] || !d["pager"] || !d["email"] {
		t.Fatalf("deliveries: %+v", d)
	}
	if other, _ := s.Store.IncidentDeliveries(tagged.ID); len(other) != 0 {
		t.Fatalf("another incident's deliveries: %+v", other)
	}

	// Closing ends the stretch; the next alert is a new incident, and the closed
	// one keeps its deliveries.
	if err := s.Store.CloseIncident(w.ID, ""); err != nil {
		t.Fatal(err)
	}
	if err := s.Store.CloseIncident(w.ID, ""); err != nil {
		t.Fatalf("closing with none open: %v", err)
	}
	next, err := s.Store.OpenOrTouchIncident(w.ID, "")
	if err != nil || next.ID == inc.ID {
		t.Fatalf("after a close: %+v err=%v, want a new incident", next, err)
	}
	if kept, _ := s.Store.IncidentDeliveries(inc.ID); len(kept) != 3 {
		t.Fatalf("the closed incident lost its deliveries: %+v", kept)
	}
	if still, _ := s.Store.OpenOrTouchIncident(w.ID, "host-a"); still.ID != tagged.ID {
		t.Fatal("closing the untagged incident closed the tagged one")
	}
}

func amendMessage(t *testing.T, s Subject) {
	ns := s.Namespace(t, s.Owner, "ops")
	if err := s.Store.SaveWatch(s.Owner, ns, "u1", "errors", spec("every 1m", plainPipeline, true, plain)); err != nil {
		t.Fatal(err)
	}
	if err := s.Store.AmendMessage(s.Owner, ns, "", "errors", "Watch the error rate"); err != nil {
		t.Fatalf("AmendMessage: %v", err)
	}
	if w := get(t, s, ns, "errors"); w.Message != "Watch the error rate" || w.CreatedBy != "u1" || w.AuthoredBy != "u1" {
		t.Fatalf("after amend: message=%q created=%q authored=%q", w.Message, w.CreatedBy, w.AuthoredBy)
	}

	// Only the author may amend when asked to check.
	if err := s.Store.AmendMessage(s.Owner, ns, "u2", "errors", "mine"); !errors.Is(err, cron.ErrNotYours) {
		t.Fatalf("amend by another user = %v, want ErrNotYours", err)
	}
	if err := s.Store.AmendMessage(s.Owner, ns, "u1", "errors", "mine"); err != nil {
		t.Fatalf("amend by the author: %v", err)
	}
	if err := s.Store.AmendMessage(s.Owner, ns, "", "absent", "x"); !errors.Is(err, cron.ErrNotFound) {
		t.Fatalf("amend of an absent watch = %v, want ErrNotFound", err)
	}

	// An edit is a new version: its author is whoever wrote it, its message is
	// empty, and the creator is still the first.
	if err := s.Store.SaveWatch(s.Owner, ns, "u2", "errors", spec("every 5m", plainPipeline, true, plain)); err != nil {
		t.Fatal(err)
	}
	if w := get(t, s, ns, "errors"); w.Message != "" || w.CreatedBy != "u1" || w.AuthoredBy != "u2" {
		t.Fatalf("after an edit: message=%q created=%q authored=%q", w.Message, w.CreatedBy, w.AuthoredBy)
	}
}
