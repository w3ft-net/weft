package weftd

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"

	"github.com/fsnotify/fsnotify"
	"github.com/graybeardcorp/weft/internal/weftlog"
)

// ValueSpec describes a metavariable value read from a file on disk. Path
// is the file; JSONPath, when set, is a dot-path extracted from the file's
// JSON contents. An empty JSONPath means "use the whole file's contents,
// trimmed of surrounding whitespace."
type ValueSpec struct {
	Path     string
	JSONPath []ValueStep // nil = read raw file
}

// ValueStep is one segment of a JSON path. Either a struct-field
// name or an array index, but never both. Determined at parse time.
type ValueStep struct {
	Field string // non-empty for field access
	Index int    // valid when Field is empty
}

// ParseJSONSpec parses the `json:` form of a metavariable, a file path
// followed by a JSON path:
//
//	"/path:.foo"                  → top-level field `foo`
//	"/path:.foo.bar"              → nested fields
//	"/path:.items[0].name"        → array index then field
//
// The separator is `:.` (colon then dot) so that paths containing `:`
// (rare, but allowed on Unix) stay usable.
func ParseJSONSpec(s string) (ValueSpec, error) {
	s = strings.TrimSpace(s)
	idx := strings.Index(s, ":.")
	if idx == -1 {
		return ValueSpec{}, fmt.Errorf("json spec %q: want <path>:.<json.path>", s)
	}
	path := s[:idx]
	if path == "" {
		return ValueSpec{}, fmt.Errorf("json spec %q: empty path", s)
	}
	steps, err := parseJSONPath(s[idx+1:]) // include the leading "."
	if err != nil {
		return ValueSpec{}, fmt.Errorf("json spec %q: %w", s, err)
	}
	return ValueSpec{Path: path, JSONPath: steps}, nil
}

// parseJSONPath turns ".foo.bar[0].baz" into a step list. Accepts
// only field access and bare integer indexing — no filters, pipes,
// or computed indices.
func parseJSONPath(p string) ([]ValueStep, error) {
	if !strings.HasPrefix(p, ".") {
		return nil, fmt.Errorf("json path must start with '.': %q", p)
	}
	var steps []ValueStep
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
			steps = append(steps, ValueStep{Field: name})
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
			steps = append(steps, ValueStep{Index: n})
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
func applyJSONPath(data any, steps []ValueStep) (string, bool) {
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

// ValueReader holds the live value of a metavariable. A file-backed reader
// reads at construction and re-reads on any directory fsnotify event for the
// file's basename; a static reader (NewStaticValueReader) never changes.
// Safe for concurrent use.
//
// Usage:
//
//	spec, _ := ParseValueSpec("/etc/weft/identity.json:.shard_id")
//	r := NewValueReader(ctx, spec)
//	row.Node = r.Current()  // empty when file/path missing or unreadable
type ValueReader struct {
	spec    ValueSpec
	current atomic.Pointer[string]
}

// NewStaticValueReader returns a reader that always reports value. It backs
// `env:` metavariables, which are read once at startup.
func NewStaticValueReader(value string) *ValueReader {
	r := &ValueReader{}
	r.current.Store(&value)
	return r
}

// NewValueReader constructs the reader, refreshing from disk and
// starting an fsnotify watch on the containing directory. All errors are
// best-effort: a missing file simply means an empty value until the file
// appears.
func NewValueReader(ctx context.Context, spec ValueSpec) *ValueReader {
	r := &ValueReader{spec: spec}
	empty := ""
	r.current.Store(&empty)

	r.refresh()

	w, err := fsnotify.NewWatcher()
	if err != nil {
		weftlog.Infof(ctx, "metavariable reader: fsnotify: %v", err)
		return r
	}
	dir, base := filepath.Dir(spec.Path), filepath.Base(spec.Path)
	if err := w.Add(dir); err != nil {
		weftlog.Infof(ctx, "metavariable reader: watch %s: %v", dir, err)
		w.Close()
		return r
	}

	go r.runWatch(ctx, w, base)
	return r
}

// Current returns the latest read value. Safe for any goroutine.
func (r *ValueReader) Current() string {
	if v := r.current.Load(); v != nil {
		return *v
	}
	return ""
}

func (r *ValueReader) refresh() {
	data, err := os.ReadFile(r.spec.Path)
	if err != nil {
		if !os.IsNotExist(err) {
			weftlog.Infof(context.Background(), "metavariable reader: %s: %v", r.spec.Path, err)
		}
		return
	}
	value := r.extract(data)
	r.current.Store(&value)
}

func (r *ValueReader) extract(data []byte) string {
	if len(r.spec.JSONPath) == 0 {
		return strings.TrimSpace(string(data))
	}
	var parsed any
	if err := json.Unmarshal(data, &parsed); err != nil {
		weftlog.Infof(context.Background(), "metavariable reader: %s: parse json: %v", r.spec.Path, err)
		return ""
	}
	value, ok := applyJSONPath(parsed, r.spec.JSONPath)
	if !ok {
		return ""
	}
	return value
}

func (r *ValueReader) runWatch(ctx context.Context, w *fsnotify.Watcher, base string) {
	defer w.Close()

	for {
		select {
		case <-ctx.Done():
			return
		case ev, ok := <-w.Events:
			if !ok {
				return
			}
			if filepath.Base(ev.Name) != base {
				continue
			}
			if ev.Op&(fsnotify.Create|fsnotify.Write|fsnotify.Rename|fsnotify.Remove) != 0 {
				r.refresh()
			}
		case err, ok := <-w.Errors:
			if !ok {
				return
			}
			weftlog.Errorf(ctx, "metavariable reader: watch error: %v", err)
		}
	}
}
