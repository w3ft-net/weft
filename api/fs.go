package api

import (
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"strings"

	"github.com/graybeardcorp/weft/auth"
	"github.com/graybeardcorp/weft/mounts"
	pb "github.com/graybeardcorp/weft/proto/weftpb"
	"github.com/graybeardcorp/weft/shellfs"
	"github.com/graybeardcorp/weft/store"
)

// sessionPerms projects a Session's role + org-permission flags into
// the shape shellfs.WithUser expects. One helper, four call sites.
func sessionPerms(s *auth.Session) mounts.OrgPerms {
	return mounts.OrgPerms{
		IsOwner:             s.IsOwner,
		CanManageFunctions:  s.CanManageFunctions,
		CanManageWatches:    s.CanManageWatches,
		CanManageEtc:        s.CanManageEtc,
		CanCreateNamespaces: s.CanCreateNamespaces,
	}
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

	opts := []shellfs.Option{shellfs.WithUser(session.UserID, sessionPerms(session))}
	fs := shellfs.New(s.store, session.ActiveOrgID, logServices, opts...)

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

	opts := []shellfs.Option{shellfs.WithUser(session.UserID, sessionPerms(session))}
	fs := shellfs.New(s.store, session.ActiveOrgID, nil, opts...)
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

	opts := []shellfs.Option{shellfs.WithUser(session.UserID, sessionPerms(session))}
	fs := shellfs.New(s.store, session.ActiveOrgID, nil, opts...)
	// WriteFile's error may be a non-blocking store.Warning (e.g. a
	// /cron.d save tripping the truncation lint) — the write already
	// happened. This generic multi-path editor endpoint has no field
	// to carry warning text today (204 has no body), so it drops the
	// message rather than reporting a successful write as a failed
	// one; the dedicated /api/watches endpoint is where a cron save's
	// warnings actually reach the user.
	blocking, _ := store.Split(fs.WriteFile(req.Path, []byte(req.Content)))
	if len(blocking) > 0 {
		http.Error(w, errors.Join(blocking...).Error(), http.StatusBadRequest)
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

	opts := []shellfs.Option{shellfs.WithUser(session.UserID, sessionPerms(session))}
	fs := shellfs.New(s.store, session.ActiveOrgID, nil, opts...)
	if err := fs.Remove(path); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) fetchLogServices(r *http.Request) []string {
	resp, err := s.coordClient.ListServices(s.coordCtx(r), &pb.ListServicesRequest{})
	if err != nil {
		log.Printf("fetchLogServices: coordinator ListApps: %v", err)
		return nil
	}
	return resp.Services
}
