package logdb

import (
	"sort"
	"testing"
	"time"

	"github.com/w3ft-net/weft/backends/shardstore"
	"github.com/w3ft-net/weft/shard"
)

// A window rotation is two cohorts of shards: the current window's, and the
// next window's, which is launched and made INGESTING PrewarmLead early so it is
// ready the instant the window opens. Readiness is the whole point of that lead;
// routing writes to the next cohort before its window opens is not, and it
// breaks reads: a query routes by window, so a row stored on a shard whose
// window has not started is invisible to any query for its own timestamp. In
// prod this surfaced at the 2026-11-16T00:00Z rotation as a ten-minute hole in
// every service's history (and as "0 coordinators" pages from crons that read
// the last five minutes): rows written from 23:50 landed on the shards of the
// window starting 00:00.

// rotation is a registry holding the current cohort (shard-current) and the
// prewarmed next one (shard-next), both INGESTING and serving ns 1 / "svc", with
// the clock set to at.
type rotation struct {
	reg  *ShardRegistry
	cut  time.Time // where the current window ends and the next begins
	week time.Duration
}

func newRotation(t *testing.T, cut, at time.Time) rotation {
	t.Helper()
	const week = 7 * 24 * time.Hour
	reg := NewShardRegistry()
	reg.now = func() time.Time { return at }
	for _, w := range []struct {
		id         string
		start, end time.Time
	}{
		{"shard-current", cut.Add(-week), cut},
		{"shard-next", cut, cut.Add(week)},
	} {
		live, err := shard.NewDuckDBShard(w.id, "")
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { live.Close() })
		reg.Add(&Shard{
			ShardRow: ShardRow{ID: w.id, WindowStart: w.start, WindowEnd: w.end, State: StateIngesting},
			Services: []shardstore.ShardService{{NamespaceID: 1, Service: "svc"}},
			Live:     live,
		})
	}
	return rotation{reg: reg, cut: cut, week: week}
}

// wroteTo is every shard the registry hands a write to, over enough picks to
// cover its round-robin.
func (r rotation) wroteTo(t *testing.T) []string {
	t.Helper()
	got := map[string]struct{}{}
	for i := 0; i < 20; i++ {
		sh := r.reg.IngestingShard(1, "svc")
		if sh == nil {
			t.Fatal("no INGESTING shard serves the service")
		}
		got[sh.ID()] = struct{}{}
	}
	return sortedIDs(got)
}

// Before the next window opens, writes go to the current cohort alone, even
// though the next one is INGESTING and ready.
func TestIngestingShard_BeforeTheNextWindowOpensWritesGoToTheCurrentCohort(t *testing.T) {
	cut := time.Now().UTC().Truncate(time.Second).Add(PrewarmLead / 2)
	r := newRotation(t, cut, cut.Add(-time.Second))
	if got := r.wroteTo(t); !equalIDs(got, "shard-current") {
		t.Errorf("1s before the next window opens, writes go to %v, want [shard-current]", got)
	}
}

// From the instant the next window opens, writes go to the next cohort alone:
// the switch has no gap, because that cohort was ready before the window opened.
func TestIngestingShard_FromTheNextWindowsStartWritesGoToTheNextCohort(t *testing.T) {
	cut := time.Now().UTC().Truncate(time.Second).Add(PrewarmLead / 2)
	r := newRotation(t, cut, cut)
	if got := r.wroteTo(t); !equalIDs(got, "shard-next") {
		t.Errorf("at the next window's start, writes go to %v, want [shard-next]", got)
	}
}

// A write is readable by a query for its own instant: every shard a write can
// go to is one that query reads.
func TestIngestingShard_AWriteIsReadableByAQueryForItsInstant(t *testing.T) {
	cut := time.Now().UTC().Truncate(time.Second).Add(PrewarmLead / 2)
	for _, at := range []time.Time{cut.Add(-PrewarmLead / 2), cut.Add(-time.Second), cut, cut.Add(time.Second)} {
		r := newRotation(t, cut, at)
		tr := TimeRange{Start: at.Add(-time.Minute), End: at.Add(time.Minute)}
		read := map[string]struct{}{}
		for _, e := range mustMatch(t, r.reg, tr, servingFor(r.reg, "svc"), "swatch") {
			read[e.ID] = struct{}{}
		}
		for _, id := range r.wroteTo(t) {
			if _, ok := read[id]; !ok {
				t.Errorf("at %s a write goes to %s, but a query for the surrounding two minutes reads only %v",
					at.Format(time.TimeOnly), id, sortedIDs(read))
			}
		}
	}
}

// When no INGESTING shard's window holds the clock (a rotation that is late, a
// successor that never came), writes still land on the INGESTING shards rather
// than the dead-letter queue.
func TestIngestingShard_WithNoWindowHoldingTheClockWritesStillLand(t *testing.T) {
	cut := time.Now().UTC().Truncate(time.Second)
	r := newRotation(t, cut, cut.Add(30*24*time.Hour)) // past both windows
	if got := r.wroteTo(t); len(got) == 0 {
		t.Fatal("no shard takes the write: it would be dead-lettered")
	}
}

// A shard with no window (dev and test stacks) takes writes at any time.
func TestIngestingShard_AShardWithNoWindowTakesWrites(t *testing.T) {
	live, err := shard.NewDuckDBShard("shard-windowless", "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { live.Close() })
	reg := NewShardRegistry()
	reg.Add(&Shard{
		ShardRow: ShardRow{ID: "shard-windowless", State: StateIngesting},
		Services: []shardstore.ShardService{{NamespaceID: 1, Service: "svc"}},
		Live:     live,
	})
	if sh := reg.IngestingShard(1, "svc"); sh == nil || sh.ID() != "shard-windowless" {
		t.Fatalf("IngestingShard = %v, want shard-windowless", sh)
	}
}

func equalIDs(got []string, want ...string) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}

func sortedIDs(set map[string]struct{}) []string {
	out := make([]string, 0, len(set))
	for id := range set {
		out = append(out, id)
	}
	sort.Strings(out)
	return out
}

// Two clocks (a coordinator's, and the writer's that stamped the row) can
// disagree about which window an instant falls in by a moment. A row stamped just
// short of a boundary can land on the next window's shard, and one just past it
// on the previous window's, so a query reads a shard whose window ends or starts
// within RoutingSkew of its range.
func TestMatchingEntries_ReadsAShardWhoseWindowIsWithinTheSkewOfTheRange(t *testing.T) {
	cut := time.Now().UTC().Truncate(time.Second).Add(PrewarmLead)
	r := newRotation(t, cut, cut)
	read := func(start, end time.Time) []string {
		got := map[string]struct{}{}
		for _, e := range mustMatch(t, r.reg, TimeRange{Start: start, End: end}, servingFor(r.reg, "svc"), "swatch") {
			got[e.ID] = struct{}{}
		}
		return sortedIDs(got)
	}

	for _, c := range []struct {
		name       string
		start, end time.Time
		want       []string
	}{
		{"a range ending inside the skew before the next window reads it",
			cut.Add(-time.Minute), cut.Add(-time.Second), []string{"shard-current", "shard-next"}},
		{"a range ending past the skew before the next window does not",
			cut.Add(-time.Minute), cut.Add(-RoutingSkew - time.Second), []string{"shard-current"}},
		{"a range starting inside the skew after the current window reads it",
			cut.Add(time.Second), cut.Add(time.Minute), []string{"shard-current", "shard-next"}},
		{"a range starting past the skew after the current window does not",
			cut.Add(RoutingSkew + time.Second), cut.Add(time.Minute), []string{"shard-next"}},
	} {
		if got := read(c.start, c.end); !equalIDs(got, c.want...) {
			t.Errorf("%s: reads %v, want %v", c.name, got, c.want)
		}
	}
}
