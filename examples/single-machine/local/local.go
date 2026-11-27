// Package local is the open build's deployment: one group, one HTTP Basic
// credential, SQLite for every store, and git for the record of tracked files,
// all on this machine. It is one way to fill a deployment.Deployment, not the
// way: a deployment that keeps its data elsewhere builds its own.
package local

import (
	"fmt"
	"os"

	shellcore "github.com/graybeardcorp/warp/pkg/shell"
	"github.com/w3ft-net/weft/alerting"
	"github.com/w3ft-net/weft/backends/history/gitrepo"
	"github.com/w3ft-net/weft/binscript"
	"github.com/w3ft-net/weft/cronauthor"
	"github.com/w3ft-net/weft/dashboard"
	"github.com/w3ft-net/weft/deployment"
	"github.com/w3ft-net/weft/ext/backends/sqlite"
	"github.com/w3ft-net/weft/ext/backends/sqlite/sqlitedb"
	"github.com/w3ft-net/weft/ext/identity/basic"
	"github.com/w3ft-net/weft/ext/identity/magiclink"
	"github.com/w3ft-net/weft/identity/open"
	"github.com/w3ft-net/weft/mounts"
)

// Option adjusts Open.
type Option func(*options)

type options struct{ baseURL string }

// WithBaseURL is the web app's external origin: the sign-in links of the
// magic-link identity point at it. The api passes it; the coordinator, which
// only verifies sessions, does not.
func WithBaseURL(u string) Option { return func(o *options) { o.baseURL = u } }

// AuthEnv chooses the identity: "basic" (the default) or "magiclink".
const AuthEnv = "WEFT_AUTH"

type authn struct {
	deployment.Authn
	// build, when set, finishes Authn once the stores are open.
	build func(*sqlite.Stores) (deployment.Authn, error)
}

func authnFromEnv(o *options) (authn, error) {
	switch mode := os.Getenv(AuthEnv); mode {
	case "", "basic":
		id, err := basic.FromEnv()
		if err != nil {
			return authn{}, err
		}
		return authn{Authn: deployment.Authn{API: id, GRPC: id.Credential.Server()}}, nil
	case "magiclink":
		secret, err := magiclink.SecretFromEnv()
		if err != nil {
			return authn{}, err
		}
		ver, err := magiclink.Verifier(secret)
		if err != nil {
			return authn{}, err
		}
		if o.baseURL == "" {
			// The coordinator: sessions are verified, nobody signs in here.
			return authn{Authn: deployment.Authn{GRPC: ver}}, nil
		}
		return authn{build: func(s *sqlite.Stores) (deployment.Authn, error) {
			id, err := magiclink.New(magiclink.Config{
				Store: s.Login, Secret: secret, BaseURL: o.baseURL,
				Mail: magiclink.SMTPMailer{SMTP: alerting.SMTPFromEnv()},
			})
			if err != nil {
				return deployment.Authn{}, err
			}
			return deployment.Authn{API: id, GRPC: id.Server()}, nil
		}}, nil
	default:
		return authn{}, fmt.Errorf("%s=%q: want basic or magiclink", AuthEnv, mode)
	}
}

// DefaultPath is the SQLite database opened when no path is given.
const DefaultPath = "weft.db"

// Local is the open deployment, with the parts of it the coordinator's own
// backends and commands are built over.
type Local struct {
	*deployment.Deployment
	// Stores is every store the one database holds.
	Stores *sqlite.Stores
	// History is the git repository that records tracked files.
	History *gitrepo.Store
}

// Open opens the SQLite database at path (DefaultPath when empty) and the git
// repository at path + ".history", and returns the deployment over them. The
// returned func closes both. The credential is read first, so a build without
// one does not start. The api and the coordinator open the same files; the
// repository serialises their commits across processes.
func Open(path string, opts ...Option) (*Local, func(), error) {
	var o options
	for _, f := range opts {
		f(&o)
	}
	// The identity is chosen before anything is opened, so a build without a
	// credential or a session key does not start.
	auth, err := authnFromEnv(&o)
	if err != nil {
		return nil, nil, err
	}
	if path == "" {
		path = DefaultPath
	}
	sdb, err := sqlitedb.Open(path)
	if err != nil {
		return nil, nil, err
	}
	stores, err := sqlite.Open(sdb)
	if err != nil {
		sdb.Close()
		return nil, nil, err
	}
	hist, err := gitrepo.Open(gitrepo.Config{Dir: path + ".history"})
	if err != nil {
		sdb.Close()
		return nil, nil, err
	}
	if auth.build != nil {
		if auth.Authn, err = auth.build(stores); err != nil {
			sdb.Close()
			return nil, nil, err
		}
	}
	d := &deployment.Deployment{
		Authn:  auth.Authn,
		Access: open.Access(),
		Backends: deployment.Backends{
			Tokens:         stores.Tokens,
			Sources:        stores.Sources,
			Crons:          stores.Crons,
			Bins:           stores.Bins,
			Dashboards:     stores.Dashboards,
			Notify:         stores.Notify,
			IngestFailures: stores.Ingest,
		},
		Mounts: &mounts.Backends{
			Sources:    stores.Sources,
			BinService: binscript.Service{Bins: stores.Bins, Crons: stores.Crons, Sources: stores.Sources},
			Notify:     stores.Notify,
			CronAuthor: cronauthor.Service{Crons: stores.Crons, Sources: stores.Sources, Bins: stores.Bins,
				Readers: dashboard.Readers(stores.Dashboards)},
			Tmp:     stores.Tmp,
			History: hist,
			EtcExtensions: func(c mounts.Config) []shellcore.Mount {
				return []shellcore.Mount{mounts.Dashboards(c, stores.Dashboards)}
			},
			CronRenamed: mounts.DashboardCronRenamed(stores.Dashboards),
		},
		AlertTargets: []alerting.AlertTarget{alerting.EmailTarget{SMTP: alerting.SMTPFromEnv()}},
	}
	return &Local{Deployment: d, Stores: stores, History: hist}, func() { hist.Close(); sdb.Close() }, nil
}
