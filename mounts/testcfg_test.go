package mounts

import (
	"context"
	"testing"

	"github.com/w3ft-net/weft/backends/defaults"
	"github.com/w3ft-net/weft/binscript"
	"github.com/w3ft-net/weft/cronauthor"
	"github.com/w3ft-net/weft/notifycfg"
	"github.com/w3ft-net/weft/shape"
	"github.com/w3ft-net/weft/shape/shapetest"
	"github.com/w3ft-net/weft/sqlitedb"
	"github.com/w3ft-net/weft/tenancy"
)

// ownerCfg returns a Config over SQLite stores holding one namespace per first
// path segment of services, acting as the system identity of tenant 1, which
// reads every namespace and writes nothing.
func ownerCfg(t testing.TB, services []string) Config {
	t.Helper()
	db, err := sqlitedb.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	bins, err := defaults.NewBinBackend(db)
	if err != nil {
		t.Fatal(err)
	}
	srcs, err := defaults.NewSourceBackend(db)
	if err != nil {
		t.Fatal(err)
	}
	crons, err := defaults.NewCronBackend(db)
	if err != nil {
		t.Fatal(err)
	}
	notify, err := defaults.NewNotifyBackend(db)
	if err != nil {
		t.Fatal(err)
	}
	tmp, err := defaults.NewTmpBackend(db)
	if err != nil {
		t.Fatal(err)
	}
	owner := shapetest.Added(1)
	seen := map[string]bool{}
	for _, a := range services {
		ns := a
		for i := 0; i < len(a); i++ {
			if a[i] == '/' {
				ns = a[:i]
				break
			}
		}
		if !seen[ns] {
			seen[ns] = true
			if _, err := srcs.FindOrCreateNamespace(owner, ns); err != nil {
				t.Fatal(err)
			}
		}
	}
	return Config{
		Owner:       owner,
		LogServices: services,
		Actor:       nobody{},
		Authz:       grants{},
		Scope:       allScope{},
		Sources:     srcs,
		BinService:  binscript.Service{Bins: bins, Crons: crons, Sources: srcs},
		Notify:      notifycfg.Service{Aliases: notify, Secrets: notify},
		CronAuthor:  cronauthor.Service{Crons: crons, Sources: srcs, Bins: bins},
		Tmp:         tmp,
	}
}

// allScope reads everything in the actor's tenant.
type allScope struct{}

func (allScope) ReadScope(_ context.Context, a tenancy.Actor) (shape.Set, error) {
	return shape.Set{a.Partition().Coord()}, nil
}

// grants is an Authorizer with a fixed set of allowed actions.
type grants map[tenancy.Action]bool

func (g grants) Authorize(_ context.Context, _ tenancy.Actor, act tenancy.Action, _ tenancy.Resource) error {
	if g[act] {
		return nil
	}
	return tenancy.Denied("not granted")
}

// nobody is an Actor with no identity of its own.
type nobody struct{}

func (nobody) Partition() shape.Added { return shapetest.Added(1) }
func (nobody) Subject() string        { return "" }

// tenantActor is an Actor with no identity of its own, in the given tenant.
type tenantActor int64

func (t tenantActor) Partition() shape.Added { return shapetest.Added(int64(t)) }
func (tenantActor) Subject() string          { return "" }
