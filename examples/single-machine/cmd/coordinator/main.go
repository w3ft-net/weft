// Command coordinator is the open-source weft coordinator: one group, one
// HTTP Basic credential, SQLite for every store, local coordination and git for
// the record of tracked files. The process itself is package server
// (cmd/coordinator/server); this main supplies the open deployment.
package main

import (
	"context"
	"log"
	"time"

	"github.com/graybeardcorp/warp/pkg/bash"
	"github.com/w3ft-net/weft/backends/defaults"
	"github.com/w3ft-net/weft/cmd/coordinator/server"
	"github.com/w3ft-net/weft/deployment/local"
	"github.com/w3ft-net/weft/identity"
	"github.com/w3ft-net/weft/ext/launcher/docker"
	"github.com/w3ft-net/weft/logdb"
	"github.com/w3ft-net/weft/mounts"
	"github.com/w3ft-net/weft/shape"
	"github.com/w3ft-net/weft/witrepo"
)

func main() {
	fl := server.ParseFlags()
	open, closeOpen, err := local.Open(fl.DBDSN)
	if err != nil {
		log.Fatalf("open database: %v", err)
	}
	defer closeOpen()
	stores := open.Stores
	server.Run(fl, open.Deployment, &server.Own{
		Platform: func(_ context.Context, _, shardSecret string) (logdb.Platform, error) {
			return docker.FromEnv(shardSecret), nil
		},
		Orgs: open.Orgs{},
		Backends: server.Backends{
			Notices:      stores.Notices,
			Coordination: stores.Coord,
			Shards:       stores.Shards,
			Services:     stores.Sources,
			DeadLetters:  stores.DLQ,
		},
		Commands: func(interp *bash.Interpreter, owner shape.Added, _ string, cwd func() string, _ identity.Access, _ string, _, _ time.Time) {
			witrepo.RegisterCommand(interp, witrepo.Config{
				Mounts:  open.Mounts.Config(mounts.Session{Owner: owner}),
				Source:  open.History,
				Journal: open.History,
				Cwd:     cwd,
			})
		},
	})
}
