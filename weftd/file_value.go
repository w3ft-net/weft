package weftd

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync/atomic"

	"github.com/fsnotify/fsnotify"
	"github.com/graybeardcorp/weft/internal/weftlog"
)

// FileValueSpec describes a dimension value sourced from a file on
// disk. Path is the file to read; JSONPath is an optional dot-path
// extracted from the file's JSON contents. An empty JSONPath means
// "use the whole file's contents, trimmed of surrounding whitespace."
//
// See docs/plans/dimensions-from-files.md for the broader design and
// the choice of the `path[:.json.path]` config syntax.
type FileValueSpec struct {
	Path     string
	JSONPath []FileValueStep // nil = read raw file
}

// FileValueStep is one segment of a JSON path. Either a struct-field
// name or an array index, but never both. Determined at parse time.
type FileValueStep struct {
	Field string // non-empty for field access
	Index int    // valid when Field is empty
}

// ParseFileValueSpec parses the textual config form. Accepts:
//
//	"/path"                       → read the whole file, trim, return
//	"/path:.foo"                  → JSON: top-level field `foo`
//	"/path:.foo.bar"              → JSON: nested fields
//	"/path:.items[0].name"        → JSON: array index then field
//
// The separator between path and JSON expression is `:.` (colon then
// dot) so that paths containing `:` (rare, but allowed on Unix) stay
// usable in the no-JSON form.
func ParseFileValueSpec(s string) (FileValueSpec, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return FileValueSpec{}, fmt.Errorf("empty file-value spec")
	}
	idx := strings.Index(s, ":.")
	if idx == -1 {
		return FileValueSpec{Path: s}, nil
	}
	path := s[:idx]
	if path == "" {
		return FileValueSpec{}, fmt.Errorf("file-value spec %q: empty path", s)
	}
	steps, err := parseJSONPath(s[idx+1:]) // include the leading "."
	if err != nil {
		return FileValueSpec{}, fmt.Errorf("file-value spec %q: %w", s, err)
	}
	return FileValueSpec{Path: path, JSONPath: steps}, nil
}

// parseJSONPath turns ".foo.bar[0].baz" into a step list. Accepts
// only field access and bare integer indexing — no filters, pipes,
// or computed indices.
func parseJSONPath(p string) ([]FileValueStep, error) {
	if !strings.HasPrefix(p, ".") {
		return nil, fmt.Errorf("json path must start with '.': %q", p)
	}
	var steps []FileValueStep
	i := 1 // skip leading dot
	for i < len(p) {
		// Field segment: read until '.', '[', or end.
		start := i
		for i < len(p) && p[i] != '.' && p[i] != '[' {
			i++
		}
		if i > start {
			name := p[start:i]
			if !validFieldName(name) {
				return nil, fmt.Errorf("invalid field name %q", name)
			}
			steps = append(steps, FileValueStep{Field: name})
		}
		// Array indices: consume one or more `[n]` groups.
		for i < len(p) && p[i] == '[' {
			close := strings.IndexByte(p[i:], ']')
			if close == -1 {
				return nil, fmt.Errorf("unclosed '[' in %q", p)
			}
			idxStr := p[i+1 : i+close]
			n, err := strconv.Atoi(idxStr)
			if err != nil || n < 0 {
				return nil, fmt.Errorf("invalid array index %q in %q", idxStr, p)
			}
			steps = append(steps, FileValueStep{Index: n})
			i += close + 1
		}
		// Allow a trailing dot before the next field or before EOL
		// (e.g. ".foo.bar" splits cleanly because of how we consume).
		if i < len(p) && p[i] == '.' {
			i++
		}
	}
	if len(steps) == 0 {
		return nil, fmt.Errorf("empty json path")
	}
	return steps, nil
}

// validFieldName accepts JSON field names made of letters, digits,
// underscores, and hyphens. Sufficient for typical config payloads
// (e.g., shard_id, role, region). Quoted-key access is intentionally
// out of scope.
func validFieldName(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if !(r >= 'a' && r <= 'z' ||
			r >= 'A' && r <= 'Z' ||
			r >= '0' && r <= '9' ||
			r == '_' || r == '-') {
			return false
		}
	}
	return true
}

// applyJSONPath evaluates steps against parsed JSON data. Returns
// the leaf as a string + ok flag. Numbers are formatted with %v so
// integers stay integer-shaped; booleans become "true"/"false".
func applyJSONPath(data any, steps []FileValueStep) (string, bool) {
	cur := data
	for _, step := range steps {
		if step.Field != "" {
			obj, ok := cur.(map[string]any)
			if !ok {
				return "", false
			}
			cur, ok = obj[step.Field]
			if !ok {
				return "", false
			}
		} else {
			arr, ok := cur.([]any)
			if !ok || step.Index >= len(arr) {
				return "", false
			}
			cur = arr[step.Index]
		}
	}
	if cur == nil {
		return "", false
	}
	switch v := cur.(type) {
	case string:
		return v, true
	case float64:
		// JSON numbers parse to float64; preserve integer shape when
		// the value has no fractional component.
		if v == float64(int64(v)) {
			return strconv.FormatInt(int64(v), 10), true
		}
		return strconv.FormatFloat(v, 'g', -1, 64), true
	case bool:
		return strconv.FormatBool(v), true
	default:
		return fmt.Sprintf("%v", v), true
	}
}

// FileValueReader holds the live value of a file-sourced dimension.
// The value is read at construction and re-read on any directory
// fsnotify event for the file's basename. Safe for concurrent use.
//
// Usage:
//
//	spec, _ := ParseFileValueSpec("/etc/weft/identity.json:.shard_id")
//	r := NewFileValueReader(ctx, spec)
//	row.Node = r.Current()  // empty when file/path missing or unreadable
type FileValueReader struct {
	spec    FileValueSpec
	current atomic.Pointer[string]
}

// NewFileValueReader constructs the reader, refreshing from disk and
// starting an fsnotify watch on the containing directory. Empty Path
// returns a reader that always reports "" (callers don't need to
// branch on a nil reader). All errors are best-effort: a missing
// file simply means an empty value until the file appears.
func NewFileValueReader(ctx context.Context, spec FileValueSpec) *FileValueReader {
	r := &FileValueReader{spec: spec}
	empty := ""
	r.current.Store(&empty)

	if spec.Path == "" {
		return r
	}

	r.refresh()

	w, err := fsnotify.NewWatcher()
	if err != nil {
		weftlog.Infof(ctx, "file value reader: fsnotify: %v", err)
		return r
	}
	dir, base := splitPath(spec.Path)
	if err := w.Add(dir); err != nil {
		weftlog.Infof(ctx, "file value reader: watch %s: %v", dir, err)
		w.Close()
		return r
	}

	go r.runWatch(ctx, w, base)
	return r
}

// Current returns the latest read value. Safe for any goroutine.
func (r *FileValueReader) Current() string {
	if v := r.current.Load(); v != nil {
		return *v
	}
	return ""
}

func (r *FileValueReader) refresh() {
	data, err := os.ReadFile(r.spec.Path)
	if err != nil {
		if !os.IsNotExist(err) {
			weftlog.Infof(context.Background(), "file value reader: %s: %v", r.spec.Path, err)
		}
		return
	}
	value := r.extract(data)
	r.current.Store(&value)
}

func (r *FileValueReader) extract(data []byte) string {
	if len(r.spec.JSONPath) == 0 {
		return strings.TrimSpace(string(data))
	}
	var parsed any
	if err := json.Unmarshal(data, &parsed); err != nil {
		weftlog.Infof(context.Background(), "file value reader: %s: parse json: %v", r.spec.Path, err)
		return ""
	}
	value, ok := applyJSONPath(parsed, r.spec.JSONPath)
	if !ok {
		return ""
	}
	return value
}

// splitPath splits a path into (dir, base) without importing
// path/filepath — same minimal helper the previous VersionReader used,
// kept here so file_value.go is self-contained.
func splitPath(p string) (dir, base string) {
	idx := strings.LastIndexByte(p, '/')
	if idx < 0 {
		return ".", p
	}
	if idx == 0 {
		return "/", p[1:]
	}
	return p[:idx], p[idx+1:]
}

func filepathBase(p string) string {
	idx := strings.LastIndexByte(p, '/')
	if idx < 0 {
		return p
	}
	return p[idx+1:]
}

func (r *FileValueReader) runWatch(ctx context.Context, w *fsnotify.Watcher, base string) {
	defer w.Close()

	for {
		select {
		case <-ctx.Done():
			return
		case ev, ok := <-w.Events:
			if !ok {
				return
			}
			if filepathBase(ev.Name) != base {
				continue
			}
			if ev.Op&(fsnotify.Create|fsnotify.Write|fsnotify.Rename|fsnotify.Remove) != 0 {
				r.refresh()
			}
		case err, ok := <-w.Errors:
			if !ok {
				return
			}
			weftlog.Errorf(ctx, "file value reader: watch error: %v", err)
		}
	}
}
