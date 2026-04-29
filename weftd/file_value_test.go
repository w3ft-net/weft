package weftd

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

func TestParseFileValueSpec(t *testing.T) {
	cases := []struct {
		in       string
		wantPath string
		wantSteps []FileValueStep
		wantErr  bool
	}{
		{"/etc/version", "/etc/version", nil, false},
		{"/etc/identity.json:.shard_id", "/etc/identity.json", []FileValueStep{{Field: "shard_id"}}, false},
		{"/etc/x.json:.foo.bar.baz", "/etc/x.json", []FileValueStep{{Field: "foo"}, {Field: "bar"}, {Field: "baz"}}, false},
		{"/etc/x.json:.items[0].name", "/etc/x.json", []FileValueStep{{Field: "items"}, {Index: 0}, {Field: "name"}}, false},
		{"/etc/x.json:.a[2][0]", "/etc/x.json", []FileValueStep{{Field: "a"}, {Index: 2}, {Index: 0}}, false},
		{"", "", nil, true},
		{":.foo", "", nil, true},                  // empty path
		{"/etc/x.json:foo", "/etc/x.json:foo", nil, false}, // ":foo" doesn't match the ":." separator → treated as plain path
		{"/etc/x.json:.[", "", nil, true},          // unclosed bracket
		{"/etc/x.json:.foo!", "", nil, true},       // invalid char in field
		{"/etc/x.json:.foo[abc]", "", nil, true},  // non-int index
	}
	for _, tc := range cases {
		got, err := ParseFileValueSpec(tc.in)
		if (err != nil) != tc.wantErr {
			t.Errorf("ParseFileValueSpec(%q): err=%v, wantErr=%v", tc.in, err, tc.wantErr)
			continue
		}
		if tc.wantErr {
			continue
		}
		if got.Path != tc.wantPath {
			t.Errorf("ParseFileValueSpec(%q).Path = %q, want %q", tc.in, got.Path, tc.wantPath)
		}
		if !reflect.DeepEqual(got.JSONPath, tc.wantSteps) {
			t.Errorf("ParseFileValueSpec(%q).JSONPath = %v, want %v", tc.in, got.JSONPath, tc.wantSteps)
		}
	}
}

func TestApplyJSONPath(t *testing.T) {
	doc := map[string]any{}
	json.Unmarshal([]byte(`{
		"shard_id": "shard-abc",
		"meta": {"role": "shard-small", "replicas": 3},
		"items": [{"name": "first"}, {"name": "second"}],
		"flags": {"active": true},
		"ratio": 0.75,
		"missing_null": null
	}`), &doc)

	cases := []struct {
		path []FileValueStep
		want string
		ok   bool
	}{
		{[]FileValueStep{{Field: "shard_id"}}, "shard-abc", true},
		{[]FileValueStep{{Field: "meta"}, {Field: "role"}}, "shard-small", true},
		{[]FileValueStep{{Field: "meta"}, {Field: "replicas"}}, "3", true},
		{[]FileValueStep{{Field: "items"}, {Index: 1}, {Field: "name"}}, "second", true},
		{[]FileValueStep{{Field: "flags"}, {Field: "active"}}, "true", true},
		{[]FileValueStep{{Field: "ratio"}}, "0.75", true},
		{[]FileValueStep{{Field: "missing"}}, "", false},
		{[]FileValueStep{{Field: "missing_null"}}, "", false},
		{[]FileValueStep{{Field: "items"}, {Index: 99}}, "", false},
		{[]FileValueStep{{Field: "shard_id"}, {Field: "nope"}}, "", false}, // descend into a string
	}
	for _, tc := range cases {
		got, ok := applyJSONPath(doc, tc.path)
		if ok != tc.ok || got != tc.want {
			t.Errorf("applyJSONPath(%v) = (%q, %v), want (%q, %v)", tc.path, got, ok, tc.want, tc.ok)
		}
	}
}

func TestFileValueReader_RawAndJSON(t *testing.T) {
	dir := t.TempDir()

	plain := filepath.Join(dir, "version")
	if err := os.WriteFile(plain, []byte("  v1.2.3\n"), 0644); err != nil {
		t.Fatal(err)
	}
	identity := filepath.Join(dir, "identity.json")
	if err := os.WriteFile(identity, []byte(`{"shard_id":"shard-xyz","role":"shard-small"}`), 0644); err != nil {
		t.Fatal(err)
	}

	rPlain := NewFileValueReader(context.Background(), FileValueSpec{Path: plain})
	if got := rPlain.Current(); got != "v1.2.3" {
		t.Errorf("plain Current() = %q, want %q", got, "v1.2.3")
	}

	specJSON, err := ParseFileValueSpec(identity + ":.shard_id")
	if err != nil {
		t.Fatal(err)
	}
	rJSON := NewFileValueReader(context.Background(), specJSON)
	if got := rJSON.Current(); got != "shard-xyz" {
		t.Errorf("json Current() = %q, want %q", got, "shard-xyz")
	}
}

func TestFileValueReader_LiveRefresh(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "identity.json")
	if err := os.WriteFile(path, []byte(`{"shard_id":"old"}`), 0644); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	spec, _ := ParseFileValueSpec(path + ":.shard_id")
	r := NewFileValueReader(ctx, spec)
	if got := r.Current(); got != "old" {
		t.Fatalf("initial Current() = %q, want old", got)
	}

	// Rewrite via rename-into-place so the directory watch triggers
	// (matches editor behavior on save).
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(`{"shard_id":"new"}`), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(tmp, path); err != nil {
		t.Fatal(err)
	}

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if r.Current() == "new" {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("Current() never picked up new value (still %q)", r.Current())
}
