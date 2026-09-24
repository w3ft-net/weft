package spanview

import "testing"

func TestParseSpansTraceLine(t *testing.T) {
	cases := []struct {
		parts  []string
		wantID string
		wantOK bool
	}{
		{[]string{"trace", "abc-123", "--spans"}, "abc-123", true},
		{[]string{"trace", "--spans", "abc-123"}, "abc-123", true},
		{[]string{"trace", "abc-123"}, "", false},                              // no --spans: falls through to plain trace
		{[]string{"trace", "abc-123", "def-456", "--spans"}, "", false},        // wrong arity
		{[]string{"trace", "--spans"}, "", false},                              // no id
		{[]string{"trace", "abc-123", "--spans", "|", "grep", "x"}, "", false}, // piped
		{[]string{"cat", "warp.log"}, "", false},
		{[]string{}, "", false},
	}
	for _, c := range cases {
		gotID, gotOK := parseSpansTraceLine(c.parts)
		if gotID != c.wantID || gotOK != c.wantOK {
			t.Errorf("parseSpansTraceLine(%v) = (%q, %v), want (%q, %v)", c.parts, gotID, gotOK, c.wantID, c.wantOK)
		}
	}
}
