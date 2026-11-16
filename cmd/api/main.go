// Command api is the open-source weft HTTP api: one group, one HTTP Basic
// credential, SQLite stores. The process itself is package server
// (cmd/api/server); this main supplies the open adapter. It imports no
// coordinator, so the binary is built without cgo.
package main

import (
	"log"

	weftapi "github.com/w3ft-net/weft/api"
	"github.com/w3ft-net/weft/backends/defaults"
	"github.com/w3ft-net/weft/backends/history/gitrepo"
	"github.com/w3ft-net/weft/binscript"
	"github.com/w3ft-net/weft/cmd/api/server"
	"github.com/w3ft-net/weft/cronauthor"
	"github.com/w3ft-net/weft/mounts"
	"github.com/w3ft-net/weft/sqlitedb"
)

// defaultDBPath is the SQLite database opened when --db-dsn and DB_DSN are both
// empty.
const defaultDBPath = "weft.db"

// open opens the SQLite database at --db-dsn and the repository at that path +
// ".history", and returns the open deployment over them. The returned func
// closes both. The credential is read here so a build without one does not
// start. The
// coordinator opens the same files; the repository serialises their commits
// across processes.
func open(fl *server.Flags) (*server.Deps, func(), error) {
	basic, err := defaults.BasicFromEnv()
	if err != nil {
		return nil, nil, err
	}
	path := fl.DBDSN
	if path == "" {
		path = defaultDBPath
	}
	sdb, err := sqlitedb.Open(path)
	if err != nil {
		return nil, nil, err
	}
	stores, err := defaults.Open(sdb)
	if err != nil {
		sdb.Close()
		return nil, nil, err
	}
	hist, err := gitrepo.Open(gitrepo.Config{Dir: path + ".history"})
	if err != nil {
		sdb.Close()
		return nil, nil, err
	}
	return &server.Deps{
		Identity: server.Identity{
			Authn:  &Identity{Basic: basic},
			Access: defaults.OpenAccess(),
		},
		Backends: server.Backends{
			Tokens:         stores.Tokens,
			Sources:        stores.Sources,
			Crons:          stores.Crons,
			Bins:           stores.Bins,
			Notify:         stores.Notify,
			IngestFailures: stores.Ingest,
		},
		Mounts: openMounts{stores: stores, history: hist},
	}, func() { hist.Close(); sdb.Close() }, nil
}

// openMounts is the mounts the open api builds a session's filesystem over:
// the one database's stores and the git repository that records tracked files.
type openMounts struct {
	stores  *defaults.Stores
	history *gitrepo.Store
}

func (m openMounts) Config(session weftapi.Session, services []string) mounts.Config {
	s := m.stores
	return mounts.Config{
		Owner:       session.Principal().Partition(),
		LogServices: services,
		Sources:     s.Sources,
		BinService:  binscript.Service{Bins: s.Bins, Crons: s.Crons, Sources: s.Sources},
		Notify:      s.Notify,
		CronAuthor:  cronauthor.Service{Crons: s.Crons, Sources: s.Sources, Bins: s.Bins},
		Tmp:         s.Tmp,
		History:     m.history,
	}
}

func main() {
	fl := server.ParseFlags()
	deps, closeDB, err := open(fl)
	if err != nil {
		log.Fatalf("open database: %v", err)
	}
	defer closeDB()
	server.Run(fl, deps)
}
