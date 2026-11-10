package api

import (
	"encoding/json"
	"errors"
	"io"
	iofs "io/fs"
	"net/http"
	"path"
	"strings"

	weftsdk "github.com/w3ft-net/weft-go"

	"github.com/graybeardcorp/weft/auth"
	"github.com/graybeardcorp/weft/mounts"
	pb "github.com/graybeardcorp/weft/proto/weftpb"
	"github.com/graybeardcorp/weft/shellfs"
	"github.com/graybeardcorp/weft/store"
)

// shellFS is the filesystem the shell writes through, bound to this request's
// session: the same mounts, the same group checks, the same plan limits. Every
// route that changes /bin, /cron.d or /etc goes through it instead of calling
// the store, so there is one implementation of who may write what.
func (s *Server) shellFS(r *http.Request, session *auth.Session, logServices []string) *shellfs.ShellFS {
	return shellfs.New(s.store, session.ActiveOrgID, logServices,
		shellfs.WithUser(session.UserID),
		shellfs.WithTier(session.Tier),
		shellfs.WithContext(r.Context()))
}

// mountPath joins a mount directory and a name from the URL, refusing a name
// (".." and the like) that would resolve outside the directory.
func mountPath(dir, name string) (string, bool) {
	p := dir + "/" + name
	return p, path.Clean(p) == p
}

// writeFSError answers a failed write through the shell filesystem: a missing
// group or an exhausted plan is 403, anything else (bad input, a rejected
// save) is 400.
func writeFSError(w http.ResponseWriter, err error) {
	var limit *mounts.PlanLimitError
	if errors.Is(err, iofs.ErrPermission) || errors.As(err, &limit) {
		http.Error(w, err.Error(), http.StatusForbidden)
		return
	}
	http.Error(w, err.Error(), http.StatusBadRequest)
}

// FSEntry is one row in a directory listing.
type FSEntry struct {
	Name  string `json:"name"`
	IsDir bool   `json:"is_dir"`
}

func (s *Server) handleListFS(w http.ResponseWriter, r *http.Request) {
	session := GetSession(r)
	path := r.URL.Query().Get("path")
	if path == "" {
		http.Error(w, "path query parameter required", http.StatusBadRequest)
		return
	}

	var logServices []string
	if strings.HasPrefix(path, "/log") {
		logServices = s.fetchLogServices(r)
	}

	fs := s.shellFS(r, session, logServices)

	if r.URL.Query().Get("walk") == "1" {
		paths, err := fs.Walk(path)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"paths": paths})
		return
	}

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

func (s *Server) handleReadFS(w http.ResponseWriter, r *http.Request) {
	session := GetSession(r)
	path := r.URL.Query().Get("path")
	if path == "" {
		http.Error(w, "path query parameter required", http.StatusBadRequest)
		return
	}

	fs := s.shellFS(r, session, nil)
	data, err := fs.ReadFile(path)
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	_, _ = w.Write(data)
}

func (s *Server) handleWriteFS(w http.ResponseWriter, r *http.Request) {
	session := GetSession(r)

	var req struct {
		Path    string `json:"path"`
		Content string `json:"content"`
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		http.Error(w, "failed to read body", http.StatusBadRequest)
		return
	}
	if err := json.Unmarshal(body, &req); err != nil {
		http.Error(w, "invalid JSON", http.StatusBadRequest)
		return
	}
	if req.Path == "" {
		http.Error(w, "path required", http.StatusBadRequest)
		return
	}

	fs := s.shellFS(r, session, nil)
	// WriteFile's error may be a non-blocking store.Warning (e.g. a
	// /cron.d save tripping the truncation lint) — the write already
	// happened. This generic multi-path editor endpoint has no field
	// to carry warning text today (204 has no body), so it drops the
	// message rather than reporting a successful write as a failed
	// one; the dedicated /api/crons endpoint is where a cron save's
	// warnings actually reach the user.
	blocking, _ := store.Split(fs.WriteFile(req.Path, []byte(req.Content)))
	if len(blocking) > 0 {
		writeFSError(w, errors.Join(blocking...))
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleRemoveFS(w http.ResponseWriter, r *http.Request) {
	session := GetSession(r)
	path := r.URL.Query().Get("path")
	if path == "" {
		http.Error(w, "path query parameter required", http.StatusBadRequest)
		return
	}

	fs := s.shellFS(r, session, nil)
	if err := fs.Remove(path); err != nil {
		writeFSError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) fetchLogServices(r *http.Request) []string {
	resp, err := s.coordClient.ListServices(s.coordCtx(r), &pb.ListServicesRequest{})
	if err != nil {
		weftsdk.Infof(r.Context(), "fetchLogServices: coordinator ListApps: %v", err)
		return nil
	}
	return resp.Services
}
