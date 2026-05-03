package api

import (
	"encoding/json"
	"net/http"

	"github.com/kevlarcon/weft/shellfs"
)

// FSEntry is one row in a directory listing — either a file or a
// nested directory. The frontend uses IsDir to decide whether
// clicking expands the row in place (directory) or stuffs `cat
// <path>` into the prompt (file).
type FSEntry struct {
	Name  string `json:"name"`
	IsDir bool   `json:"is_dir"`
}

// handleListFS returns the immediate children of a path under the
// caller's active customer. Used by the shell-chrome /tmp + /etc
// tree sidebars; `cat` and the rest of the shell drive their own
// output via the gRPC stream.
//
// path is the URL-encoded absolute path (e.g. /tmp, /etc,
// /etc/dashboards). Anything not rooted at one of the user-visible
// virtual filesystems gets the same 400 ReadDir would emit — we
// don't impose a separate allowlist here because shellfs already
// gates everything through the customer scope.
func (s *Server) handleListFS(w http.ResponseWriter, r *http.Request) {
	session := GetSession(r)
	path := r.URL.Query().Get("path")
	if path == "" {
		http.Error(w, "path query parameter required", http.StatusBadRequest)
		return
	}

	// shellfs is cheap to construct per-request; nil logApps because
	// the tree sidebars only target /tmp and /etc, neither of which
	// uses the log-app list. (If a future caller wants to list
	// /log/ via this endpoint, we'll thread the apps in then.)
	fs := shellfs.New(s.store, session.ActiveCustomerID, nil)
	entries, err := fs.ReadDir(path)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	out := make([]FSEntry, 0, len(entries))
	for _, e := range entries {
		out = append(out, FSEntry{Name: e.Name(), IsDir: e.IsDir()})
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"entries": out})
}
