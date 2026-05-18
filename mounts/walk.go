package mounts

import (
	"path"
	"strings"
)

// Walk returns the flat subtree under p as mount-relative paths
// ("/"-separated, directories suffixed with "/"). It is the
// server-side backend for the CLI's `tree` (ProxyMount.Walk) and
// collapses what would be O(directories) ReadDir round-trips into one
// call. /log takes LogMount.Walk's single-pass, memoised-permission
// fast path; the other (small, local) mounts fall back to a recursive
// ReadDir over the composed MountFS.
func Walk(cfg Config, p string) ([]string, error) {
	p = "/" + strings.Trim(path.Clean("/"+p), "/")
	if p == "/log" || strings.HasPrefix(p, "/log/") {
		lm := &LogMount{cfg: cfg}
		rel := strings.TrimPrefix(strings.TrimPrefix(p, "/log"), "/")
		return lm.Walk(rel)
	}

	mfs := New(cfg)
	prefix := "/" + firstSeg(strings.TrimPrefix(p, "/"))
	var out []string
	var rec func(abs string)
	rec = func(abs string) {
		names, err := mfs.ReadDir(abs)
		if err != nil {
			return
		}
		for _, de := range names {
			clean := de.Name()
			childAbs := strings.TrimSuffix(abs, "/") + "/" + clean
			rel := strings.TrimPrefix(strings.TrimPrefix(childAbs, prefix), "/")
			if de.IsDir() {
				out = append(out, rel+"/")
				rec(childAbs)
			} else {
				out = append(out, rel)
			}
		}
	}
	rec(p)
	return out, nil
}

func firstSeg(s string) string {
	if i := strings.Index(s, "/"); i >= 0 {
		return s[:i]
	}
	return s
}
