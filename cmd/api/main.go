// Command api is the open-source weft HTTP api: one group, one HTTP Basic
// credential, SQLite stores. The process itself is package server
// (cmd/api/server); this main supplies the open adapter. It imports no
// coordinator, so the binary is built without cgo.
package main

import (
	"log"

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

func main() {
	fl := server.ParseFlags()
	// The credential is read first so a build without one does not start.
	basic, err := defaults.BasicFromEnv()
	if err != nil {
		log.Fatalf("open database: %v", err)
	}
	// The coordinator opens the same files; the repository serialises their
	// commits across processes.
	path := fl.DBDSN
	if path == "" {
		path = defaultDBPath
	}
	sdb, err := sqlitedb.Open(path)
	if err != nil {
		log.Fatalf("open database: %v", err)
	}
	defer sdb.Close()
	stores, err := defaults.Open(sdb)
	if err != nil {
		log.Fatalf("open database: %v", err)
	}
	hist, err := gitrepo.Open(gitrepo.Config{Dir: path + ".history"})
	if err != nil {
		log.Fatalf("open database: %v", err)
	}
	defer hist.Close()
	server.Run(fl, &server.Deps{
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
		Mounts: &mounts.Backends{
			Sources:    stores.Sources,
			BinService: binscript.Service{Bins: stores.Bins, Crons: stores.Crons, Sources: stores.Sources},
			Notify:     stores.Notify,
			CronAuthor: cronauthor.Service{Crons: stores.Crons, Sources: stores.Sources, Bins: stores.Bins},
			Tmp:        stores.Tmp,
			History:    hist,
		},
	})
}
