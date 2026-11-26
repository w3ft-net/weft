package shell

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"
)

// fakeTransport answers every POST with a fixed status and body, and records
// what was sent.
type fakeTransport struct {
	status int
	body   string
	err    error

	url    string
	header map[string]string
	sent   map[string]any
}

func (f *fakeTransport) Post(_ context.Context, url string, header map[string]string, body []byte) (*ExecResponse, error) {
	f.url, f.header = url, header
	json.Unmarshal(body, &f.sent)
	if f.err != nil {
		return nil, f.err
	}
	if f.status == 0 {
		f.status = 200
	}
	return &ExecResponse{Status: f.status, Body: io.NopCloser(strings.NewReader(f.body))}, nil
}

func sse(events ...string) string {
	var b strings.Builder
	for _, e := range events {
		b.WriteString("data: " + e + "\n\n")
	}
	return b.String()
}

func newExec(t *fakeTransport) *ExecClient {
	return &ExecClient{
		BaseURL:      "http://api.test",
		Headers:      func() map[string]string { return map[string]string{"Authorization": "Bearer tok"} },
		Transport:    t,
		Unauthorized: errors.New("sign in"),
	}
}

func TestExecClient_QuerySendsTheRequest(t *testing.T) {
	ft := &fakeTransport{body: sse(`{"type":"done"}`)}
	_, err := newExec(ft).Query(context.Background(), "tail /log/a", "30m..", []string{"/log", "/tmp"})
	if err != nil {
		t.Fatal(err)
	}
	if ft.url != "http://api.test/api/shell/exec" {
		t.Errorf("url = %q", ft.url)
	}
	if ft.header["Authorization"] != "Bearer tok" || ft.header["Content-Type"] != "application/json" {
		t.Errorf("headers = %v", ft.header)
	}
	if ft.sent["pipeline"] != "tail /log/a" || ft.sent["range"] != "30m.." || ft.sent["follow"] != false {
		t.Errorf("body = %v", ft.sent)
	}
	if got, _ := ft.sent["dirstack"].([]any); len(got) != 2 || got[0] != "/log" {
		t.Errorf("dirstack = %v", ft.sent["dirstack"])
	}
}

func TestExecClient_QueryCollectsRowsNoticesStderrAndCwd(t *testing.T) {
	ft := &fakeTransport{body: sse(
		`{"type":"notice","key":"k","kind":"warning","title":"T","body":"B"}`,
		`{"type":"line","data":{"raw":"one"}}`,
		`{"type":"stderr","text":"oops"}`,
		`{"type":"line","data":{"raw":"two"}}`,
		`{"type":"cwd","stack":["/tmp","/"]}`,
		`{"type":"done"}`,
		`{"type":"line","data":{"raw":"after done"}}`,
	)}
	res, err := newExec(ft).Query(context.Background(), "x", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Rows) != 2 || res.Rows[0].Raw != "one" || res.Rows[1].Raw != "two" {
		t.Errorf("rows = %+v", res.Rows)
	}
	if !strings.Contains(res.Text, "oops") {
		t.Errorf("text = %q", res.Text)
	}
	if len(res.Notices) != 1 || res.Notices[0] != (QueryResultNotice{Key: "k", Kind: "warning", Title: "T", Body: "B"}) {
		t.Errorf("notices = %+v", res.Notices)
	}
	if len(res.FinalDirStack) != 2 || res.FinalDirStack[0] != "/tmp" {
		t.Errorf("cwd = %v", res.FinalDirStack)
	}
}

// The server has sent a terminal error as "message" and as "text"; neither may
// end a failed query as a clean one.
func TestExecClient_ErrorEventEndsTheQuery(t *testing.T) {
	for _, field := range []string{"message", "text"} {
		ft := &fakeTransport{body: sse(`{"type":"error","` + field + `":"no such shard"}`)}
		_, err := newExec(ft).Query(context.Background(), "x", "", nil)
		if err == nil || err.Error() != "no such shard" {
			t.Errorf("%s: err = %v", field, err)
		}
		ft = &fakeTransport{body: sse(`{"type":"error","` + field + `":"gone"}`)}
		err = newExec(ft).Follow(context.Background(), "tail -f /log/a", "", func(Row) {})
		if err == nil || err.Error() != "gone" {
			t.Errorf("%s follow: err = %v", field, err)
		}
	}
}

func TestExecClient_StatusErrors(t *testing.T) {
	ft := &fakeTransport{status: 401, body: "nope"}
	if _, err := newExec(ft).Query(context.Background(), "x", "", nil); err == nil || err.Error() != "sign in" {
		t.Errorf("401: %v", err)
	}
	ft = &fakeTransport{status: 426, body: `{"running":"1","required":"2","upgrade":"update it"}`}
	if _, err := newExec(ft).Query(context.Background(), "x", "", nil); err == nil || !strings.Contains(err.Error(), "server requires 2") {
		t.Errorf("426: %v", err)
	}
	ft = &fakeTransport{status: 500, body: " boom \n"}
	if _, err := newExec(ft).Query(context.Background(), "x", "", nil); err == nil || err.Error() != "HTTP 500: boom" {
		t.Errorf("500: %v", err)
	}
}

func TestExecClient_TimeoutIsUserFacing(t *testing.T) {
	ft := &fakeTransport{err: context.DeadlineExceeded}
	if _, err := newExec(ft).Query(context.Background(), "x", "", nil); !errors.Is(err, errQueryTimedOut) {
		t.Errorf("err = %v", err)
	}
}

func TestExecClient_FollowStreamsRows(t *testing.T) {
	ft := &fakeTransport{body: sse(
		`{"type":"line","data":{"raw":"a"}}`,
		`{"type":"line","data":{"raw":"b"}}`,
	)}
	var got []string
	err := newExec(ft).Follow(context.Background(), "tail -f /log/a", "1h..", func(r Row) { got = append(got, r.Raw) })
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(got, ",") != "a,b" {
		t.Errorf("rows = %v", got)
	}
	if ft.sent["follow"] != true || ft.sent["range"] != "1h.." {
		t.Errorf("body = %v", ft.sent)
	}
}

func TestExecClient_IgnoresUnknownEvents(t *testing.T) {
	ft := &fakeTransport{body: sse(`{"type":"other","step":"x1"}`, `{"type":"line","data":{"raw":"a"}}`, `{"type":"done"}`)}
	res, err := newExec(ft).Query(context.Background(), "x", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Rows) != 1 {
		t.Errorf("rows = %d, want the known line event handled and the unknown one skipped", len(res.Rows))
	}
}
