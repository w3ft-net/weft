package api

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

// execStream sends a body through /api/shell/exec and returns the SSE stream
// and whether the coordinator was asked to run anything.
func (f *shellExecFixture) execStream(body map[string]any) (stream string, ranQuery bool) {
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
	return w.Body.String(), len(f.fake.lastReq) > 0
}

// `explain <line>` is answered by the server from the line as typed, and
// runs nothing.
func TestShellExec_Explain(t *testing.T) {
	f := newShellExecFixture(t, "explain")
	cust, _ := f.srv.store.FindOrCreateOrg(fmt.Sprintf("explain-%d", os.Getpid()))
	if err := f.srv.store.SaveShellFunction(cust.ID, "", "find-errors", `cat /log/loom/api/prod.log | grep $1`, "bash"); err != nil {
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
			[]string{"awk", "post-retrieval", "Barrier: stage 1"}},
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
			if ran {
				t.Error("explain asked the coordinator to run something")
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
