package spanview

import "testing"

func TestParseSpansLine(t *testing.T) {
	cases := []struct {
		parts        []string
		wantPipeline string
		wantTitle    string
		wantOK       bool
	}{
		// One request's breakdown: `trace --spans --id <id>`.
		{[]string{"trace", "--spans", "--id", "abc-123"}, "trace --spans --id abc-123", "abc-123", true},
		{[]string{"trace", "--id", "abc-123", "--spans"}, "trace --spans --id abc-123", "abc-123", true},
		{[]string{"trace", "--id", "abc-123", "--spans", "acme/api"}, "trace --spans --id abc-123 acme/api", "abc-123", true},
		{[]string{"trace", "--id", "abc-123"}, "", "", false},                              // no --spans: plain lines
		{[]string{"trace", "--spans", "--id"}, "", "", false},                              // no id given
		{[]string{"trace", "--spans", "--id", "abc-123", "|", "grep", "x"}, "", "", false}, // piped

		// Operations across requests: `trace --spans [--name <span>] [scope]`.
		{[]string{"trace", "--spans"}, "trace --spans", "all spans", true},
		{[]string{"trace", "--spans", "acme/api"}, "trace --spans acme/api", "all spans in acme/api", true},
		{[]string{"trace", "--spans", "--name", "shard.Query"}, "trace --spans --name shard.Query", "shard.Query", true},
		{[]string{"trace", "--spans", "--name", "shard.Query", "extra", "more"}, "", "", false}, // two scopes
		{[]string{"trace", "--spans", "--name", "shard.Query", "|", "grep", "x"}, "", "", false},
		{[]string{"trace", "--spans", "--name", "shard.Query", "--samples"}, "", "", false}, // the samples list is not a view of its own

		// Not views: the recent-traces list and one request's lines.
		{[]string{"trace"}, "", "", false},
		{[]string{"trace", "acme"}, "", "", false},

		{[]string{"cat", "warp.log"}, "", "", false},
		{[]string{}, "", "", false},
	}
	for _, c := range cases {
		got, ok := ParseSpansLine(c.parts)
		if ok != c.wantOK {
			t.Errorf("ParseSpansLine(%v) ok = %v, want %v", c.parts, ok, c.wantOK)
			continue
		}
		if !ok {
			continue
		}
		if got.Pipeline != c.wantPipeline || got.Title != c.wantTitle {
			t.Errorf("ParseSpansLine(%v) = (%q, %q), want (%q, %q)", c.parts, got.Pipeline, got.Title, c.wantPipeline, c.wantTitle)
		}
	}
}
