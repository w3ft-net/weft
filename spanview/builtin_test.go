package spanview

import "testing"

func TestParseSpansLine(t *testing.T) {
	cases := []struct {
		parts        []string
		wantPipeline string
		wantTitle    string
		wantOK       bool
	}{
		// Phase 1: `trace <id> --spans`.
		{[]string{"trace", "abc-123", "--spans"}, "trace abc-123", "abc-123", true},
		{[]string{"trace", "--spans", "abc-123"}, "trace abc-123", "abc-123", true},
		{[]string{"trace", "abc-123"}, "", "", false},                              // no --spans: falls through to plain trace
		{[]string{"trace", "abc-123", "def-456", "--spans"}, "", "", false},        // wrong arity
		{[]string{"trace", "--spans"}, "", "", false},                              // no id
		{[]string{"trace", "abc-123", "--spans", "|", "grep", "x"}, "", "", false}, // piped

		// Phase 2: `trace spans [name]`.
		{[]string{"trace", "spans"}, "trace spans", "all spans", true},
		{[]string{"trace", "spans", "shard.Query"}, "trace spans shard.Query", "shard.Query", true},
		{[]string{"trace", "spans", "shard.Query", "extra"}, "", "", false}, // wrong arity
		{[]string{"trace", "spans", "shard.Query", "|", "grep", "x"}, "", "", false},

		{[]string{"cat", "warp.log"}, "", "", false},
		{[]string{}, "", "", false},
	}
	for _, c := range cases {
		got, ok := parseSpansLine(c.parts)
		if ok != c.wantOK {
			t.Errorf("parseSpansLine(%v) ok = %v, want %v", c.parts, ok, c.wantOK)
			continue
		}
		if !ok {
			continue
		}
		if got.Pipeline != c.wantPipeline || got.Title != c.wantTitle {
			t.Errorf("parseSpansLine(%v) = (%q, %q), want (%q, %q)", c.parts, got.Pipeline, got.Title, c.wantPipeline, c.wantTitle)
		}
	}
}
