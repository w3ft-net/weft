// Command api is the open-source weft HTTP api: one tenant, one HTTP Basic
// credential, SQLite stores. The process itself is package server
// (cmd/api/server); this main supplies the open adapter. It imports no
// coordinator, so the binary is built without cgo.
package main

import (
	weftapi "github.com/w3ft-net/weft/api"
	"github.com/w3ft-net/weft/backends/bin"
	"github.com/w3ft-net/weft/backends/cron"
	"github.com/w3ft-net/weft/backends/defaults"
	"github.com/w3ft-net/weft/backends/history/gitrepo"
	"github.com/w3ft-net/weft/backends/ingestfail"
	"github.com/w3ft-net/weft/backends/source"
	"github.com/w3ft-net/weft/backends/token"
	"github.com/w3ft-net/weft/binscript"
	"github.com/w3ft-net/weft/cmd/api/server"
	"github.com/w3ft-net/weft/cronauthor"
	"github.com/w3ft-net/weft/mounts"
	"github.com/w3ft-net/weft/notifycfg"
	"github.com/w3ft-net/weft/sqlitedb"
	"github.com/w3ft-net/weft/tenancy"
)

// defaultDBPath is the SQLite database opened when --db-dsn and DB_DSN are both
// empty.
const defaultDBPath = "weft.db"

// db is what the open api opens: the stores over one SQLite database, the git
// repository that records tracked files, and the one credential. The
// coordinator opens the same files; the repository serialises their commits
// across processes.
type db struct {
	*defaults.Stores
	history *gitrepo.Store
	basic   defaults.Basic
}

// open opens the SQLite database at path and the repository at path +
// ".history". The returned func closes both. The credential is read here so a
// build without one does not start. The Redis settings are the hosted schema
// cache's and are ignored.
func open(path, _ string, _ bool) (*db, func(), error) {
	basic, err := defaults.BasicFromEnv()
	if err != nil {
		return nil, nil, err
	}
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
	return &db{Stores: stores, history: hist, basic: basic}, func() { hist.Close(); sdb.Close() }, nil
}

func adapter() server.Adapter[*db] {
	return server.Adapter[*db]{
		Open: open,
		Identity: func(d *db) (weftapi.Identity, error) {
			return &Identity{Basic: d.basic}, nil
		},
		Tenancy:        func(*db) tenancy.Adapter { return defaults.OpenTenancy() },
		Tokens:         func(d *db) token.Store { return d.Tokens },
		Sources:        func(d *db) source.Store { return d.Sources },
		Crons:          func(d *db) cron.Store { return d.Crons },
		Bins:           func(d *db) bin.Store { return d.Bins },
		Notify:         func(d *db) notifycfg.Service { return d.Notify },
		IngestFailures: func(d *db) ingestfail.Store { return d.Ingest },
		Mounts: func(d *db) func(weftapi.Session, []string) mounts.Config {
			return func(session weftapi.Session, services []string) mounts.Config {
				owner := session.Actor().Partition()
				cfg := mounts.Config{
					Owner:       owner,
					LogServices: services,
					Sources:     d.Sources,
					BinService:  binscript.Service{Bins: d.Bins, Crons: d.Crons, Sources: d.Sources},
					Notify:      d.Notify,
					CronAuthor:  cronauthor.Service{Crons: d.Crons, Sources: d.Sources, Bins: d.Bins},
					Tmp:         d.Tmp,
				}
				cfg.History = d.history
				return cfg
			}
		},
	}
}

func main() { server.Run(adapter()) }
