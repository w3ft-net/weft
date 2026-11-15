package api

import (
	"bytes"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"

	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	pb "github.com/w3ft-net/weft/proto/weftpb"
)

// execStream sends a body through /api/shell/exec and returns the SSE stream
// and the pipelines the coordinator was asked to run.
func (f *shellExecFixture) execStream(body map[string]any) (stream string, ran []*pb.CoordinatorExecuteRequest) {
	f.t.Helper()
	body["range"] = "1d.."
	raw, _ := json.Marshal(body)
	req := httptest.NewRequest("POST", "/api/shell/exec", bytes.NewReader(raw))
	req.Header.Set("Authorization", f.bearer)
	w := httptest.NewRecorder()
	f.srv.Handler().ServeHTTP(w, req)
	if w.Code != 200 {
		f.t.Fatalf("status %d: %s", w.Code, w.Body.String())
	}
	for len(f.fake.lastReq) > 0 {
		ran = append(ran, <-f.fake.lastReq)
	}
	return w.Body.String(), ran
}

// `explain <line>` is answered by the server from the line as typed. It
// asks the coordinator for nothing but the count of the lines the optimized
// stages return, never the pipeline or a stage that isn't optimized.
func TestShellExec_Explain(t *testing.T) {
	f := newShellExecFixture(t, "explain")
	f.fake.reply = func(*pb.CoordinatorExecuteRequest) ([]string, error) { return []string{"1234567"}, nil }
	if err := f.a.st.Bins.Save(f.a.owner, "", "find-errors", `cat /log/loom/api/prod.log | grep $1`, "bash"); err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name string
		body map[string]any
		want []string
	}{
		{"all optimized",
			map[string]any{"pipeline": "explain cat /log/x/y/main.log | grep ERROR"},
			[]string{"Every stage: optimized retrieval."}},
		{"a barrier",
			map[string]any{"pipeline": `explain cat /log/x/y/main.log | awk '{ printf "%s\n", $0 }'`},
			[]string{"awk", "post-retrieval", "Barrier: stage 1", "1,234,567 lines retrieved"}},
		{"relative to the client's cwd",
			map[string]any{"pipeline": "explain cat main.log | grep ERROR", "dirstack": []string{"/log/x/y"}},
			[]string{"Every stage: optimized retrieval."}},
		{"a saved script is analyzed through its body",
			map[string]any{"pipeline": `explain find-errors boom | awk '{ printf "%s\n", $0 }'`},
			[]string{"grep boom", "Barrier: stage 2"}},
		{"bare explain says how to use it",
			map[string]any{"pipeline": "explain"},
			[]string{"usage: explain"}},
		{"an option is refused, not read as a command",
			map[string]any{"pipeline": "explain -v cat /log/x/y/main.log | grep ERROR"},
			[]string{`"type":"stderr"`, "explain: unknown option -v"}},
		{"a long option is refused",
			map[string]any{"pipeline": "explain --json cat /log/x/y/main.log"},
			[]string{"explain: unknown option --json"}},
		{"not a log query",
			map[string]any{"pipeline": "explain echo hi"},
			[]string{`"type":"stderr"`, "explain:"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			stream, ran := f.execStream(tt.body)
			for _, req := range ran {
				if !strings.HasSuffix(req.Pipeline, " | wc -l") || strings.Contains(req.Pipeline, "awk") {
					t.Errorf("explain asked the coordinator to run %q", req.Pipeline)
				}
			}
			for _, w := range tt.want {
				if !strings.Contains(stream, w) {
					t.Errorf("stream lacks %q:\n%s", w, stream)
				}
			}
		})
	}
}

// Only the word explain is taken; a command that merely starts with it goes
// to the coordinator like any other line.
func TestShellExec_ExplainIsAWordNotAPrefix(t *testing.T) {
	f := newShellExecFixture(t, "explainword")
	got := f.post(map[string]any{"pipeline": "explainer foo", "range": "1d.."})
	if got.Pipeline != "explainer foo" {
		t.Errorf("coordinator got %q", got.Pipeline)
	}
}

// The count is a query of the caller's own: their token, their client's
// directory and range, the optimized stages as typed and a wc -l.
func TestShellExec_ExplainCountsAsTheCaller(t *testing.T) {
	f := newShellExecFixture(t, "explaincount")
	f.fake.reply = func(*pb.CoordinatorExecuteRequest) ([]string, error) { return []string{"259662"}, nil }

	stream, ran := f.execStream(map[string]any{
		"pipeline": `explain cat main.log | grep "query rpc: done" | awk '{ printf "%s\n", $0 }'`,
		"dirstack": []string{"/log/x/y"},
	})
	if len(ran) != 1 {
		t.Fatalf("coordinator ran %d queries, want the one count", len(ran))
	}
	if want := `cat main.log | grep "query rpc: done" | wc -l`; ran[0].Pipeline != want {
		t.Errorf("count ran %q, want %q", ran[0].Pipeline, want)
	}
	if ran[0].TimeStartNs == 0 || ran[0].TimeEndNs <= ran[0].TimeStartNs {
		t.Errorf("count has no time range: %d..%d", ran[0].TimeStartNs, ran[0].TimeEndNs)
	}
	if got := <-f.fake.authSeen; got != f.bearer {
		t.Errorf("count ran with authorization %q, want the caller's %q", got, f.bearer)
	}
	if !strings.Contains(stream, "259,662 lines retrieved with optimization") {
		t.Errorf("stream lacks the count:\n%s", stream)
	}
}

// A pipeline that is all optimized retrieval is its own result: explain
// asks for no count of it.
func TestShellExec_ExplainOfAFullyOptimizedPipelineRunsNothing(t *testing.T) {
	f := newShellExecFixture(t, "explainnone")
	_, ran := f.execStream(map[string]any{"pipeline": "explain cat /log/x/y/main.log | grep ERROR"})
	if len(ran) != 0 {
		t.Errorf("coordinator ran %v", ran)
	}
}

// A count that fails is advice that couldn't be given, not an error in the
// explain: the placement still prints, with the reason.
func TestShellExec_ExplainSurvivesACountThatFails(t *testing.T) {
	line := `explain cat /log/x/y/main.log | grep ERROR | awk '{ printf "%s\n", $0 }'`
	for _, tt := range []struct {
		name   string
		reply  func(*pb.CoordinatorExecuteRequest) ([]string, error)
		want   string
		budget time.Duration // default 50ms
	}{
		{"an error", func(*pb.CoordinatorExecuteRequest) ([]string, error) {
			return nil, status.Error(codes.PermissionDenied, "no access to x/y")
		}, "line count unavailable: no access to x/y", 0},
		{"not a number", func(*pb.CoordinatorExecuteRequest) ([]string, error) { return []string{"many"}, nil },
			"line count unavailable: no count came back", 0},
		{"more than a line", func(*pb.CoordinatorExecuteRequest) ([]string, error) { return []string{"1", "2"}, nil },
			"line count unavailable: no count came back", 0},
		{"too slow", func(*pb.CoordinatorExecuteRequest) ([]string, error) {
			time.Sleep(300 * time.Millisecond)
			return []string{"1"}, nil
		}, "line count unavailable: took longer than 50ms", 0},
		// The coordinator enforces the deadline it was sent, and its timer and ours
		// race for it: when its wins, the stream ends before our context has expired,
		// as a cancellation or an expired deadline. Either way the count took too long.
		{"cancelled by the coordinator's own deadline", func(*pb.CoordinatorExecuteRequest) ([]string, error) {
			return nil, status.Error(codes.Canceled, "stream terminated by RST_STREAM with error code: CANCEL")
		}, "line count unavailable: took longer than 50ms", 0},
		{"deadline exceeded by the coordinator's clock", func(*pb.CoordinatorExecuteRequest) ([]string, error) {
			return nil, status.Error(codes.DeadlineExceeded, "context deadline exceeded")
		}, "line count unavailable: took longer than 50ms", 0},
		// A cancellation long before the deadline is not the deadline: it keeps its own
		// reason, and is reported at once, not after waiting out the budget.
		{"cancelled for another reason", func(*pb.CoordinatorExecuteRequest) ([]string, error) {
			return nil, status.Error(codes.Canceled, "the coordinator is shutting down")
		}, "line count unavailable: the coordinator is shutting down", 5 * time.Second},
	} {
		t.Run(tt.name, func(t *testing.T) {
			defer func(old time.Duration) { explainCountBudget = old }(explainCountBudget)
			explainCountBudget = 50 * time.Millisecond
			if tt.budget != 0 {
				explainCountBudget = tt.budget
			}
			f := newShellExecFixture(t, "explainfail")
			f.fake.reply = tt.reply
			began := time.Now()
			stream, _ := f.execStream(map[string]any{"pipeline": line})
			if tt.budget >= time.Second && time.Since(began) > time.Second {
				t.Errorf("a cancellation well before the deadline waited %s for it", time.Since(began))
			}
			for _, w := range []string{tt.want, "post-retrieval", "Barrier: stage 2"} {
				if !strings.Contains(stream, w) {
					t.Errorf("stream lacks %q:\n%s", w, stream)
				}
			}
			if strings.Contains(stream, `"type":"error"`) || strings.Contains(stream, `"type":"stderr"`) {
				t.Errorf("a failed count surfaced as an error:\n%s", stream)
			}
		})
	}
}
