package api

import (
	"encoding/json"
	"io"
	"log"
	"net/http"
	"strings"

	"github.com/kevlarcon/weft/auth"
	"github.com/kevlarcon/weft/mounts"
	pb "github.com/kevlarcon/weft/proto/weftpb"
	"github.com/kevlarcon/weft/shellfs"
)

// sessionPerms projects a Session's role + org-permission flags into
// the shape shellfs.WithUser expects. One helper, four call sites.
func sessionPerms(s *auth.Session) mounts.OrgPerms {
	return mounts.OrgPerms{
		IsOwner:            s.IsOwner,
		CanManageFunctions: s.CanManageFunctions,
		CanManageWatches:   s.CanManageWatches,
		CanManageEtc:       s.CanManageEtc,
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

	var logApps []string
	if strings.HasPrefix(path, "/log") {
		logApps = s.fetchLogApps(r)
	}

	opts := []shellfs.Option{shellfs.WithUser(session.UserID, sessionPerms(session))}
	fs := shellfs.New(s.store, session.ActiveCustomerID, logApps, opts...)

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
	fs := shellfs.New(s.store, session.ActiveCustomerID, nil, opts...)
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
	fs := shellfs.New(s.store, session.ActiveCustomerID, nil, opts...)
	if err := fs.WriteFile(req.Path, []byte(req.Content)); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
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
	fs := shellfs.New(s.store, session.ActiveCustomerID, nil, opts...)
	if err := fs.Remove(path); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) fetchLogApps(r *http.Request) []string {
	resp, err := s.coordClient.ListApps(s.coordCtx(r), &pb.ListAppsRequest{})
	if err != nil {
		log.Printf("fetchLogApps: coordinator ListApps: %v", err)
		return nil
	}
	return resp.Apps
}
