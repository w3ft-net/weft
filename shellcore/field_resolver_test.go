package shellcore

import "testing"

func TestParseFieldSpec(t *testing.T) {
	cases := []struct {
		in       string
		wantIdx  int
		wantPath string
		wantErr  bool
	}{
		{"", 0, "", false},
		{"1", 1, "", false},
		{"42", 42, "", false},
		{".foo", 0, "foo", false},
		{".foo.bar", 0, "foo.bar", false},
		{".items.0.name", 0, "items.0.name", false},
		{"0", 0, "", true},
		{"-1", 0, "", true},
		{"abc", 0, "", true},
		{".", 0, "", true},
	}
	for _, c := range cases {
		got, err := ParseFieldSpec(c.in)
		if (err != nil) != c.wantErr {
			t.Errorf("ParseFieldSpec(%q): err=%v, wantErr=%v", c.in, err, c.wantErr)
			continue
		}
		if got.Index != c.wantIdx || got.Path != c.wantPath {
			t.Errorf("ParseFieldSpec(%q): got {%d, %q}, want {%d, %q}",
				c.in, got.Index, got.Path, c.wantIdx, c.wantPath)
		}
	}
}

func TestResolveField_Positional(t *testing.T) {
	spec, _ := ParseFieldSpec("2")
	cases := []struct {
		line, want string
	}{
		{"alpha beta gamma", "beta"},
		{"  one   two   three  ", "two"},
		{"only", ""}, // out of range → empty, no error
		{"", ""},
	}
	for _, c := range cases {
		got, err := ResolveField(spec, c.line)
		if err != nil {
			t.Errorf("ResolveField(%q): unexpected err %v", c.line, err)
		}
		if got != c.want {
			t.Errorf("ResolveField(%q): got %q, want %q", c.line, got, c.want)
		}
	}
}

func TestResolveField_JSONPath(t *testing.T) {
	specFoo, _ := ParseFieldSpec(".foo")
	specNested, _ := ParseFieldSpec(".req.latency_ms")
	specMissing, _ := ParseFieldSpec(".nope")
	specArr, _ := ParseFieldSpec(".items.1")

	line := `{"foo":"hello","req":{"latency_ms":12.5,"path":"/api"},"items":["a","b","c"]}`

	if got, _ := ResolveField(specFoo, line); got != "hello" {
		t.Errorf("foo: got %q, want hello", got)
	}
	if got, _ := ResolveField(specNested, line); got != "12.5" {
		t.Errorf("nested: got %q, want 12.5", got)
	}
	if got, _ := ResolveField(specMissing, line); got != "" {
		t.Errorf("missing: got %q, want empty", got)
	}
	if got, _ := ResolveField(specArr, line); got != "b" {
		t.Errorf("arr index: got %q, want b", got)
	}
}

func TestResolveField_JSONPath_InvalidLine(t *testing.T) {
	spec, _ := ParseFieldSpec(".foo")
	_, err := ResolveField(spec, "not json at all")
	if err == nil {
		t.Fatal("expected error for non-JSON line in JSON-path mode")
	}
}

func TestResolveField_JSONPath_NumericTopLevel(t *testing.T) {
	// `jq '.cpu_pct'` emits a stream of bare numbers, one per line.
	// Our chart pipeline doesn't try to ResolveField from such lines
	// (no -y .path is set when input is bare-scalar JSON), but if a
	// caller does, gjson handles it without panicking.
	spec, _ := ParseFieldSpec(".foo")
	got, err := ResolveField(spec, "5")
	if err != nil {
		t.Fatalf("unexpected error on bare scalar: %v", err)
	}
	if got != "" {
		t.Errorf("got %q, want empty (no .foo on bare scalar)", got)
	}
}
