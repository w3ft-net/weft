// Command coordinator is the open-source weft coordinator: one tenant, one
// HTTP Basic credential, SQLite for every store, local coordination and git for
// the record of tracked files. The process itself is package server
// (cmd/coordinator/server); this main supplies the open adapter.
package main

import (
	"time"

	"github.com/graybeardcorp/warp/pkg/bash"
	"github.com/w3ft-net/weft/alerting"
	"github.com/w3ft-net/weft/backends/bin"
	"github.com/w3ft-net/weft/backends/coordination"
	"github.com/w3ft-net/weft/backends/cron"
	"github.com/w3ft-net/weft/backends/defaults"
	"github.com/w3ft-net/weft/backends/dlq"
	"github.com/w3ft-net/weft/backends/history/gitrepo"
	"github.com/w3ft-net/weft/backends/ingestfail"
	"github.com/w3ft-net/weft/backends/notice"
	"github.com/w3ft-net/weft/backends/source"
	"github.com/w3ft-net/weft/backends/token"
	"github.com/w3ft-net/weft/binscript"
	"github.com/w3ft-net/weft/cmd/coordinator/server"
	"github.com/w3ft-net/weft/coordinator"
	"github.com/w3ft-net/weft/cronauthor"
	"github.com/w3ft-net/weft/identity"
	"github.com/w3ft-net/weft/logdb"
	"github.com/w3ft-net/weft/mounts"
	"github.com/w3ft-net/weft/shape"
	"github.com/w3ft-net/weft/sqlitedb"
	"github.com/w3ft-net/weft/tenancy"
	"github.com/w3ft-net/weft/witrepo"
)

// defaultDBPath is the SQLite database opened when --db-dsn and DB_DSN are both
// empty.
const defaultDBPath = "weft.db"

// db is what the open coordinator opens: the stores over one SQLite database,
// the git repository that records tracked files, and the one credential. The
// api opens the same files; the repository serialises their commits across
// processes.
type db struct {
	*defaults.Stores
	history *gitrepo.Store
	basic   defaults.Basic
}

// open opens the SQLite database at path and the repository at path +
// ".history". The returned func closes both. The credential is read here so a
// build without one does not start.
func open(path string) (*db, func(), error) {
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
		Coordinator: server.Tenancy[*db]{
			Orgs:       defaults.OpenOrgs{},
			Scoper:     func(*db) tenancy.Scoper { return defaults.OpenScoper{} },
			Authorizer: func(*db) tenancy.Authorizer { return defaults.OpenAuthorizer{} },
			Mounts:     mountsOver,
			Commands:   commandsOver,
		},
		Identity:       func(d *db) identity.Server { return d.basic.Server() },
		Coordination:   func(d *db) coordination.Coordination { return d.Coord },
		Shards:         func(d *db) logdb.ShardStore { return d.Shards },
		Services:       func(d *db) logdb.ServiceDirectory { return d.Sources },
		Tokens:         func(d *db) token.Store { return d.Tokens },
		Sources:        func(d *db) source.Store { return d.Sources },
		Notices:        func(d *db) notice.Store { return d.Notices },
		IngestFailures: func(d *db) ingestfail.Store { return d.Ingest },
		DeadLetters:    func(d *db, _ string) dlq.Store { return d.DLQ },
		Crons:          func(d *db) cron.Store { return d.Crons },
		Bins:           func(d *db) bin.Store { return d.Bins },
		Notifier: func(d *db) *coordinator.Notifier {
			return coordinator.NewNotifier(d.Notify, alerting.EmailTarget{SMTP: alerting.SMTPFromEnv()})
		},
	}
}

func mountsOver(d *db) coordinator.MountsFunc {
	return func(owner shape.Added, _ string, services []string) mounts.Config {
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
}

func commandsOver(d *db) coordinator.CommandsFunc {
	return func(interp *bash.Interpreter, owner shape.Added, _ string, cwd func() string, _ tenancy.Adapter, _ string, _, _ time.Time) {
		witrepo.RegisterCommand(interp, witrepo.Config{
			Mounts:  mountsOver(d)(owner, "", nil),
			Source:  d.history,
			Journal: d.history,
			Cwd:     cwd,
		})
	}
}

func main() { server.Run(adapter()) }
