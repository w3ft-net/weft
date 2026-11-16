// Command coordinator is the open-source weft coordinator: one group, one
// HTTP Basic credential, SQLite for every store, local coordination and git for
// the record of tracked files. The process itself is package server
// (cmd/coordinator/server); this main supplies the open adapter.
package main

import (
	"log"
	"time"

	"github.com/graybeardcorp/warp/pkg/bash"
	"github.com/w3ft-net/weft/alerting"
	"github.com/w3ft-net/weft/backends/defaults"
	"github.com/w3ft-net/weft/backends/history/gitrepo"
	"github.com/w3ft-net/weft/binscript"
	"github.com/w3ft-net/weft/cmd/coordinator/server"
	"github.com/w3ft-net/weft/coordinator"
	"github.com/w3ft-net/weft/cronauthor"
	"github.com/w3ft-net/weft/identity"
	"github.com/w3ft-net/weft/mounts"
	"github.com/w3ft-net/weft/shape"
	"github.com/w3ft-net/weft/sqlitedb"
	"github.com/w3ft-net/weft/witrepo"
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
	// The api opens the same files; the repository serialises their commits
	// across processes.
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
	backends := &mounts.Backends{
		Sources:    stores.Sources,
		BinService: binscript.Service{Bins: stores.Bins, Crons: stores.Crons, Sources: stores.Sources},
		Notify:     stores.Notify,
		CronAuthor: cronauthor.Service{Crons: stores.Crons, Sources: stores.Sources, Bins: stores.Bins},
		Tmp:        stores.Tmp,
		History:    hist,
	}
	server.Run(fl, &server.Deps{
		Identity: server.Identity{
			Authn: basic.Server(),
			Orgs:  defaults.OpenOrgs{},
			Scope: defaults.OpenScoper{},
			Authz: defaults.OpenAuthorizer{},
		},
		Backends: server.Backends{
			Tokens:         stores.Tokens,
			Sources:        stores.Sources,
			Notices:        stores.Notices,
			Crons:          stores.Crons,
			Bins:           stores.Bins,
			IngestFailures: stores.Ingest,
			Coordination:   stores.Coord,
			Shards:         stores.Shards,
			Services:       stores.Sources,
			DeadLetters:    stores.DLQ,
		},
		Notifier: coordinator.NewNotifier(stores.Notify, alerting.EmailTarget{SMTP: alerting.SMTPFromEnv()}),
		Mounts:   backends,
		Commands: func(interp *bash.Interpreter, owner shape.Added, _ string, cwd func() string, _ identity.Access, _ string, _, _ time.Time) {
			witrepo.RegisterCommand(interp, witrepo.Config{
				Mounts:  backends.Config(mounts.Session{Owner: owner}),
				Source:  hist,
				Journal: hist,
				Cwd:     cwd,
			})
		},
	})
}
