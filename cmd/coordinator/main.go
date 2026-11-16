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

// open opens the SQLite database at --db-dsn and the repository at DSN +
// ".history", and returns the open deployment over them. The returned func
// closes both. The credential is read here so a build without one does not
// start. The api opens the same files; the repository serialises their commits
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
	o := openDeployment{stores: stores, history: hist}
	return &server.Deps{
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
		Mounts:   o.mounts,
		Commands: o.commands,
	}, func() { hist.Close(); sdb.Close() }, nil
}

// openDeployment is what the open coordinator builds its mounts and shell
// commands over: the one database's stores and the git repository that records
// tracked files.
type openDeployment struct {
	stores  *defaults.Stores
	history *gitrepo.Store
}

func (o openDeployment) mounts(owner shape.Added, _ string, services []string) mounts.Config {
	s := o.stores
	return mounts.Config{
		Owner:       owner,
		LogServices: services,
		Sources:     s.Sources,
		BinService:  binscript.Service{Bins: s.Bins, Crons: s.Crons, Sources: s.Sources},
		Notify:      s.Notify,
		CronAuthor:  cronauthor.Service{Crons: s.Crons, Sources: s.Sources, Bins: s.Bins},
		Tmp:         s.Tmp,
		History:     o.history,
	}
}

func (o openDeployment) commands(interp *bash.Interpreter, owner shape.Added, _ string, cwd func() string, _ identity.Access, _ string, _, _ time.Time) {
	witrepo.RegisterCommand(interp, witrepo.Config{
		Mounts:  o.mounts(owner, "", nil),
		Source:  o.history,
		Journal: o.history,
		Cwd:     cwd,
	})
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
