package tracecmd

import (
	"strings"
	"testing"
)

func TestParse_ModesAndFields(t *testing.T) {
	cases := []struct {
		args []string
		want Cmd
		mode Mode
	}{
		{nil, Cmd{}, ModeList},
		{[]string{"acme"}, Cmd{Namespace: "acme"}, ModeList},
		{[]string{"acme/api"}, Cmd{Namespace: "acme", Service: "api"}, ModeList},
		{[]string{"--id", "abc"}, Cmd{ID: "abc"}, ModeLines},
		{[]string{"acme/api", "--id", "abc"}, Cmd{ID: "abc", Namespace: "acme", Service: "api"}, ModeLines},
		{[]string{"--id", "abc", "acme"}, Cmd{ID: "abc", Namespace: "acme"}, ModeLines},
		{[]string{"--spans"}, Cmd{Spans: true}, ModeSpans},
		{[]string{"--spans", "acme/api"}, Cmd{Spans: true, Namespace: "acme", Service: "api"}, ModeSpans},
		{[]string{"--spans", "--name", "db.query"}, Cmd{Spans: true, Name: "db.query"}, ModeSpans},
		{[]string{"--spans", "--name", "db.query", "--samples"}, Cmd{Spans: true, Name: "db.query", Samples: true}, ModeSamples},
		{[]string{"--spans", "--id", "abc"}, Cmd{Spans: true, ID: "abc"}, ModeBreakdown},
		{[]string{"--id", "abc", "--spans", "acme"}, Cmd{Spans: true, ID: "abc", Namespace: "acme"}, ModeBreakdown},
	}
	for _, c := range cases {
		got, err := Parse(c.args)
		if err != nil {
			t.Errorf("Parse(%q) = %v", c.args, err)
			continue
		}
		if *got != c.want {
			t.Errorf("Parse(%q) = %+v, want %+v", c.args, *got, c.want)
		}
		if got.Mode() != c.mode {
			t.Errorf("Parse(%q).Mode() = %v, want %v", c.args, got.Mode(), c.mode)
		}
	}
}

func TestParse_Errors(t *testing.T) {
	cases := []struct {
		args []string
		want string
	}{
		{[]string{"--bogus"}, "trace: unknown option --bogus"},
		{[]string{"-v"}, "trace: unknown option -v"},
		{[]string{"--id"}, "trace: --id needs a trace id"},
		{[]string{"--id", "--spans"}, "trace: --id needs a trace id"},
		{[]string{"--id", "a", "--id", "b"}, "trace: --id given more than once"},
		{[]string{"--name"}, "trace: --name needs a span name"},
		{[]string{"--spans", "--name", "a", "--name", "b"}, "trace: --name given more than once"},
		{[]string{"a", "b"}, "trace: expected at most one <namespace>[/<service>] argument"},
		{[]string{"/log/acme/api"}, "trace: a scope is <namespace>[/<service>], not a path"},
		{[]string{"acme/"}, "trace: a scope is <namespace>[/<service>]"},
		{[]string{"/api"}, "trace: a scope is <namespace>[/<service>]"},
		{[]string{"a/b/c"}, "trace: a scope is <namespace>[/<service>]"},
		{[]string{"--name", "db.query"}, "trace: --name needs --spans"},
		{[]string{"--samples"}, "trace: --samples needs --spans --name <span>"},
		{[]string{"--spans", "--samples"}, "trace: --samples needs --spans --name <span>"},
		{[]string{"--spans", "--id", "abc", "--name", "db.query"}, "trace: --name looks across requests, so it can't be combined with --id"},
	}
	for _, c := range cases {
		_, err := Parse(c.args)
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("Parse(%q) = %v, want an error containing %q", c.args, err, c.want)
		}
	}
}

func TestString_RoundTrips(t *testing.T) {
	for _, args := range [][]string{
		nil,
		{"acme/api"},
		{"--id", "abc"},
		{"--id", "abc", "acme"},
		{"--spans"},
		{"--spans", "acme/api"},
		{"--spans", "--name", "db.query"},
		{"--spans", "--name", "db.query", "--samples", "acme"},
		{"--spans", "--id", "abc"},
	} {
		c, err := Parse(args)
		if err != nil {
			t.Fatalf("Parse(%q) = %v", args, err)
		}
		again, err := Parse(strings.Fields(strings.TrimPrefix(c.String(), "trace")))
		if err != nil || *again != *c {
			t.Errorf("String() = %q doesn't round-trip: %+v, %v (want %+v)", c.String(), again, err, c)
		}
	}
}

func TestString_QuotesNamesWithSpaces(t *testing.T) {
	c := &Cmd{Spans: true, Name: "GET /orders/:id"}
	if got, want := c.String(), `trace --spans --name 'GET /orders/:id'`; got != want {
		t.Errorf("String() = %q, want %q", got, want)
	}
}

func TestTitle(t *testing.T) {
	for _, c := range []struct {
		cmd  Cmd
		want string
	}{
		{Cmd{}, "recent traces"},
		{Cmd{Namespace: "acme"}, "recent traces in acme"},
		{Cmd{ID: "abc"}, "abc"},
		{Cmd{Spans: true, ID: "abc"}, "abc"},
		{Cmd{Spans: true}, "all spans"},
		{Cmd{Spans: true, Namespace: "acme", Service: "api"}, "all spans in acme/api"},
		{Cmd{Spans: true, Name: "db.query"}, "db.query"},
	} {
		if got := c.cmd.Title(); got != c.want {
			t.Errorf("%+v Title() = %q, want %q", c.cmd, got, c.want)
		}
	}
}

func TestParseLine(t *testing.T) {
	cases := []struct {
		line string
		want Cmd
		ok   bool
	}{
		{"trace", Cmd{}, true},
		{"trace   acme/api  ", Cmd{Namespace: "acme", Service: "api"}, true},
		{"trace --id abc", Cmd{ID: "abc"}, true},
		{"trace --spans --name 'GET /orders/:id'", Cmd{Spans: true, Name: "GET /orders/:id"}, true},
		{`trace --spans --name "GET /orders"`, Cmd{Spans: true, Name: "GET /orders"}, true},
		{`trace --spans --name 'it'\''s'`, Cmd{Spans: true, Name: "it's"}, true},
		{"trace | jq .lines", Cmd{}, false},
		{"trace --spans > /tmp/x", Cmd{}, false},
		{"trace --spans >> /tmp/x", Cmd{}, false},
		{"trace --id abc | grep x", Cmd{}, false},
		{"cat x.log", Cmd{}, false},
		{"", Cmd{}, false},
		{"trace --bogus", Cmd{}, false},
		{"trace 'unterminated", Cmd{}, false},
	}
	for _, c := range cases {
		got, ok := ParseLine(c.line)
		if ok != c.ok || (ok && *got != c.want) {
			t.Errorf("ParseLine(%q) = %+v, %v; want %+v, %v", c.line, got, ok, c.want, c.ok)
		}
	}
}

func TestParseLine_RoundTripsString(t *testing.T) {
	c := &Cmd{Spans: true, Name: "it's a \"span\"", Namespace: "acme"}
	got, ok := ParseLine(c.String())
	if !ok || *got != *c {
		t.Errorf("ParseLine(%q) = %+v, %v; want %+v", c.String(), got, ok, *c)
	}
}

func TestParse_ScopeRejectsShellCharacters(t *testing.T) {
	for _, a := range []string{"|", ">x", "a;b", "a b", "$x", "a*", "`x`", "a(b)", "a&b"} {
		if _, err := Parse([]string{a}); err == nil {
			t.Errorf("Parse(%q) accepted a scope with a shell character", a)
		}
	}
}

func TestSamplesPipeline(t *testing.T) {
	for _, c := range []struct{ aggregate, name, label, want string }{
		{"trace --spans", "db.query", "acme/orders", "trace --spans --name db.query --samples acme/orders"},
		{"trace --spans acme", "db.query", "acme/orders", "trace --spans --name db.query --samples acme/orders"},
		{"trace --spans acme/api", "db.query", "", "trace --spans --name db.query --samples acme/api"},
		{"trace --spans", "GET /orders/:id", "", "trace --spans --name 'GET /orders/:id' --samples"},
		{"not a trace line", "x", "", "trace --spans --name x --samples"},
	} {
		if got := SamplesPipeline(c.aggregate, c.name, c.label); got != c.want {
			t.Errorf("SamplesPipeline(%q, %q, %q) = %q, want %q", c.aggregate, c.name, c.label, got, c.want)
		}
	}
}

func TestIDPipeline(t *testing.T) {
	if got, want := IDPipeline("abc"), "trace --id abc"; got != want {
		t.Errorf("IDPipeline = %q, want %q", got, want)
	}
	if got, want := IDPipeline("a b"), "trace --id 'a b'"; got != want {
		t.Errorf("IDPipeline = %q, want %q", got, want)
	}
}
