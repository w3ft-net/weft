//go:build test

package coordtest

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/w3ft-net/weft/compute"
	"github.com/w3ft-net/weft/coordinator"
	"github.com/w3ft-net/weft/planner"
	pb "github.com/w3ft-net/weft/proto/weftpb"
	"github.com/w3ft-net/weft/shape/shapetest"
)

// FollowQueries runs a follow: the historical batch first, then the live rows
// the broker broadcasts on the same stream, scoped to the source it names and to
// the caller's own group.
func FollowQueries(t *testing.T, newSubject func(t *testing.T) Subject) {
	withBroker := func(f *queryFixture) *queryFixture {
		f.c.Tail = coordinator.NewTailBroker()
		f.c.TailFanout = coordinator.NewTailFanout(f.c.Tail, "", false)
		return f
	}
	follow := func(t *testing.T, f *queryFixture, ctx context.Context, line string, seed []string) *coordinator.ExecuteResult {
		t.Helper()
		res, err := f.c.ExecuteFollow(ctx, line, f.full(), f.s.PrincipalOf(""), seed)
		if err != nil {
			t.Fatalf("ExecuteFollow: %v", err)
		}
		return res
	}
	next := func(t *testing.T, ctx context.Context, res *coordinator.ExecuteResult) string {
		t.Helper()
		readCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
		defer cancel()
		line, ok, err := res.Stream.Next(readCtx)
		if err != nil || !ok {
			t.Fatalf("Next: ok=%v err=%v; the broadcast row never arrived", ok, err)
		}
		return line.Raw
	}

	// The stream emits the historical batch first; once it is exhausted the
	// broker takes over and rows broadcast at runtime arrive on the same stream.
	t.Run("HistoricalThenBroker", func(t *testing.T) {
		f := withBroker(newQueryFixture(t, newSubject(t)))
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		// No -f in the pipeline: the follow flag drives live tailing, and a plain
		// tail keeps the historical batch small.
		res := follow(t, f, ctx, "tail vimgym.log", nil)
		if len(drainHistorical(t, res.Stream, 500*time.Millisecond)) == 0 {
			t.Fatal("historical phase emitted zero lines; the follow stream is broken")
		}
		const live = "2026-03-28T12:00:00 INFO vimgym i-001 live broadcast"
		f.c.Tail.Broadcast([]coordinator.Row{{LogRow: &pb.LogRow{Raw: live, Namespace: "vimgym"}, Added: f.s.Owner}})
		if got := next(t, ctx, res); got != live {
			t.Errorf("broker row Raw = %q, want the broadcast we sent", got)
		}
	})

	// A follow whose source is a relative path resolves against the courier's
	// cwd, so the live matcher filters on the right namespace, service and kind:
	// from /log/vimgym/api, `tail gamestate.log` follows vimgym/api/gamestate.
	t.Run("RelativeSourceResolvesAgainstCwd", func(t *testing.T) {
		f := withBroker(newQueryFixture(t, newSubject(t)))
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		res := follow(t, f, ctx, "tail gamestate.log", []string{"/log/vimgym/api"})
		drainHistorical(t, res.Stream, 500*time.Millisecond)
		f.c.Tail.Broadcast([]coordinator.Row{{LogRow: &pb.LogRow{Raw: "live gamestate row", Namespace: "vimgym", Service: "api", Kind: "gamestate"}, Added: f.s.Owner}})
		if got := next(t, ctx, res); got != "live gamestate row" {
			t.Errorf("broker row Raw = %q, want the broadcast we sent (the matcher likely filtered on the wrong namespace)", got)
		}
	})

	// The live matcher once ignored the group, so a follow received every
	// group's rows, both for a bare `warp.log` (an empty namespace matches all)
	// and for a namespace name two groups share. The other group's row is
	// broadcast first; the follower must see only its own.
	for name, pipeline := range map[string]string{"WildcardWarpLog": "tail warp.log", "SharedNamespaceName": "tail vimgym.log"} {
		t.Run("DoesNotLeakOtherGroups/"+name, func(t *testing.T) {
			shapetest.NeedsGroup(t)
			f := withBroker(newQueryFixture(t, newSubject(t)))
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()

			res := follow(t, f, ctx, pipeline, nil)
			drainHistorical(t, res.Stream, 500*time.Millisecond)
			f.c.Tail.Broadcast([]coordinator.Row{
				{LogRow: &pb.LogRow{Raw: "OTHER GROUP SECRET", Namespace: "vimgym"}, Added: shapetest.Added(12)},
				{LogRow: &pb.LogRow{Raw: "my own row", Namespace: "vimgym"}, Added: f.s.Owner},
			})
			if got := next(t, ctx, res); got != "my own row" {
				t.Errorf("first live line = %q; the follow leaked another group's row", got)
			}
		})
	}

	// Without a broker the call is rejected rather than returning a stream that
	// silently drops every live row.
	t.Run("NoBrokerErrors", func(t *testing.T) {
		f := newQueryFixture(t, newSubject(t))
		f.c.Tail, f.c.TailFanout = nil, nil
		if _, err := f.c.ExecuteFollow(context.Background(), "tail vimgym.log", f.full(), f.s.PrincipalOf(""), nil); err == nil {
			t.Fatal("expected an error when Tail is nil")
		}
	})

	// Only plannable log queries can be followed: the broker subscribes to log
	// shards, so a bare `echo hi` is ErrNotPlannable rather than a wedge on a
	// subscription nothing broadcasts to.
	t.Run("NonLogPipelineRejected", func(t *testing.T) {
		f := withBroker(newQueryFixture(t, newSubject(t)))
		if _, err := f.c.ExecuteFollow(context.Background(), "echo hi", f.full(), f.s.PrincipalOf(""), nil); !errors.Is(err, planner.ErrNotPlannable) {
			t.Fatalf("err = %v, want ErrNotPlannable", err)
		}
	})
}

// drainHistorical reads stream until Next blocks for quiet without producing a
// line: the historical phase has drained and the stream waits on the broker.
func drainHistorical(t *testing.T, stream compute.LineStream, quiet time.Duration) []string {
	t.Helper()
	var out []string
	for {
		ctx, cancel := context.WithTimeout(context.Background(), quiet)
		line, ok, err := stream.Next(ctx)
		cancel()
		if err != nil {
			if errors.Is(err, context.DeadlineExceeded) {
				return out
			}
			t.Fatalf("Next: %v", err)
		}
		if !ok {
			return out
		}
		out = append(out, line.Raw)
	}
}

// ScriptTimeouts runs saved scripts under the coordinator's execution limit: a
// runaway loop is cancelled instead of holding a goroutine and a cron slot for
// ever, and a script that finishes inside the limit returns normally.
func ScriptTimeouts(t *testing.T, newSubject func(t *testing.T) Subject) {
	t.Run("InfiniteLoopHitsTimeout", func(t *testing.T) {
		s := newSubject(t)
		s.saveBin(t, "runaway", `while true; do echo spin; done`)
		c := s.coordinator(t, coordinator.CoordinatorConfig{ScriptExecutionTimeout: 200 * time.Millisecond})

		start := time.Now()
		res, err := c.Execute(context.Background(), "runaway", coordinator.TimeRange{}, s.PrincipalOf(""))
		if err != nil {
			t.Fatalf("Execute returned a synchronous error before the stream drained: %v", err)
		}
		// The timeout surfaces on the terminal read, after the watcher cancels the
		// interpreter and the pipes close.
		_, err = res.Drain(context.Background())
		if err == nil {
			t.Fatal("expected a timeout error, got nil")
		}
		if !strings.Contains(err.Error(), "execution time limit") {
			t.Errorf("err = %q, want a message about the execution time limit", err)
		}
		// 200ms limit, 500ms grace: generous headroom for a slow machine, none for
		// a regression that ignores the limit.
		if elapsed := time.Since(start); elapsed > 2*time.Second {
			t.Errorf("took %s; expected a return within the grace period of the 200ms limit", elapsed)
		}
	})

	t.Run("QuickScriptCompletesNormally", func(t *testing.T) {
		s := newSubject(t)
		s.saveBin(t, "quick", `echo ok`)
		c := s.coordinator(t, coordinator.CoordinatorConfig{ScriptExecutionTimeout: 5 * time.Second})
		stdout, _, err := s.run(t, c, "quick")
		if err != nil {
			t.Fatal(err)
		}
		if strings.TrimSpace(stdout) != "ok" {
			t.Errorf("stdout = %q, want ok", stdout)
		}
	})
}
