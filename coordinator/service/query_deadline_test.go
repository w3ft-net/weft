package service

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	weftcfg "github.com/w3ft-net/weft/config"
	"github.com/w3ft-net/weft/coordinator"
	pb "github.com/w3ft-net/weft/proto/weftpb"
	"github.com/w3ft-net/weft/shape/shapetest"
)

// spin never finishes on its own, so only the deadline, or the caller going
// away, can end it.
const spin = `while true; do :; done`

type deadlineFixture struct {
	srv *Server
	ctx context.Context
}

func newDeadlineFixture(t *testing.T, seconds int) *deadlineFixture {
	t.Helper()
	env := newSvcEnv(t)
	cfg := weftcfg.Defaults()
	cfg.QueryDeadlineSeconds = seconds
	ptr := &atomic.Pointer[weftcfg.Runtime]{}
	ptr.Store(cfg)
	return &deadlineFixture{
		srv: env.server(coordinator.CoordinatorConfig{}, &Server{CfgPtr: ptr}),
		ctx: env.callerCtx(context.Background(), "deadline-owner"),
	}
}

func (f *deadlineFixture) execute(ctx context.Context, req *pb.CoordinatorExecuteRequest) (out string, err error) {
	s := &collectStream{ctx: ctx}
	err = f.srv.Execute(req, s)
	return s.out.String(), err
}

// A query that outlives the deadline is cancelled and the caller is told so,
// in words of ours: what happened and what to change, no internals.
func TestExecute_DeadlineEndsASlowQuery(t *testing.T) {
	f := newDeadlineFixture(t, 1)
	start := time.Now()
	_, err := f.execute(f.ctx, &pb.CoordinatorExecuteRequest{Pipeline: spin})
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("the query ran %s past a 1 s deadline", elapsed)
	}
	st, ok := status.FromError(err)
	if !ok || st.Code() != codes.DeadlineExceeded {
		t.Fatalf("err = %v, want a DeadlineExceeded status", err)
	}
	for _, want := range []string{"did not finish in the time allowed (1 second)", "Narrow the time range"} {
		if !strings.Contains(st.Message(), want) {
			t.Errorf("message lacks %q:\n%s", want, st.Message())
		}
	}
	for _, internal := range []string{"context", "deadline exceeded", "rpc error", "goroutine"} {
		if strings.Contains(st.Message(), internal) {
			t.Errorf("message leaks %q:\n%s", internal, st.Message())
		}
	}
}

// The message carries the explain of the caller's pipeline when it is a log
// query, and without anything to say about it, nothing.
func TestQueryDeadlineError_CarriesTheExplain(t *testing.T) {
	f := newDeadlineFixture(t, 60)
	err := f.srv.queryDeadlineError(f.ctx, f.ctx, shapetest.Added(1),
		`cat /log/x/y/main.log | grep ERROR | awk '{ printf "%s\n", $0 }'`, nil, 60*time.Second)
	msg := status.Convert(err).Message()
	for _, want := range []string{"(60 seconds)", "grep ERROR", "optimized retrieval", "post-retrieval", "Barrier: stage 2"} {
		if !strings.Contains(msg, want) {
			t.Errorf("message lacks %q:\n%s", want, msg)
		}
	}
	if strings.Contains(msg, "\x1b[") {
		t.Errorf("message carries terminal colors:\n%q", msg)
	}

	msg = status.Convert(f.srv.queryDeadlineError(f.ctx, f.ctx, shapetest.Added(1), spin, nil, 60*time.Second)).Message()
	if strings.Contains(msg, "Barrier") || strings.Contains(msg, "optimized") {
		t.Errorf("a pipeline explain can't analyze got an explain:\n%s", msg)
	}
	if !strings.Contains(msg, "did not finish") {
		t.Errorf("message lacks the statement:\n%s", msg)
	}
}

// The deadline ends a query the caller still wants; a caller who went away
// is not told it timed out, and a follow, or a deadline of 0, runs on.
func TestExecute_DeadlineDoesNotApplyTo(t *testing.T) {
	for _, tc := range []struct {
		name    string
		seconds int
		req     *pb.CoordinatorExecuteRequest
		cancel  time.Duration // the caller goes away after this
	}{
		{"a caller who left first", 30, &pb.CoordinatorExecuteRequest{Pipeline: spin}, 200 * time.Millisecond},
		{"a follow", 1, &pb.CoordinatorExecuteRequest{Pipeline: spin, Follow: true}, 1500 * time.Millisecond},
		{"a deadline of 0", 0, &pb.CoordinatorExecuteRequest{Pipeline: spin}, 1500 * time.Millisecond},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newDeadlineFixture(t, tc.seconds)
			ctx, cancel := context.WithCancel(f.ctx)
			defer cancel()
			time.AfterFunc(tc.cancel, cancel)
			if _, err := f.execute(ctx, tc.req); err != nil {
				t.Errorf("err = %v, want the stream to end quietly when its caller left", err)
			}
		})
	}
}

// A query that finishes inside the deadline is untouched by it.
func TestExecute_DeadlineLeavesAFastQueryAlone(t *testing.T) {
	f := newDeadlineFixture(t, 1)
	out, err := f.execute(f.ctx, &pb.CoordinatorExecuteRequest{Pipeline: "echo hello"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "hello") {
		t.Errorf("output %q", out)
	}
}
