package mounts

import (
	"context"
	"testing"

	"github.com/w3ft-net/weft/backends/mem"
	"github.com/w3ft-net/weft/binscript"
	"github.com/w3ft-net/weft/cronauthor"
	"github.com/w3ft-net/weft/identity"
	"github.com/w3ft-net/weft/notifycfg"
	"github.com/w3ft-net/weft/shape"
	"github.com/w3ft-net/weft/shape/shapetest"
)

// ownerCfg returns a Config over SQLite stores holding one namespace per first
// path segment of services, acting as the system identity of group 1, which
// reads every namespace and writes nothing.
func ownerCfg(t testing.TB, services []string) Config {
	t.Helper()
	bins := mem.NewBins()
	srcs := mem.NewSources()
	crons := mem.NewCrons(srcs)
	notify := mem.NewNotify()
	tmp := mem.NewTmp()
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
		Principal:   nobody{},
		Authz:       grants{},
		Scope:       allScope{},
		Sources:     srcs,
		BinService:  binscript.Service{Bins: bins, Crons: crons, Sources: srcs},
		Notify:      notifycfg.Service{Aliases: notify, Secrets: notify},
		CronAuthor:  cronauthor.Service{Crons: crons, Sources: srcs, Bins: bins},
		Tmp:         tmp,
	}
}

// allScope reads everything in the principal's group.
type allScope struct{}

func (allScope) ReadScope(_ context.Context, a identity.Principal) (shape.Set, error) {
	return shape.Set{a.Partition().Coord()}, nil
}

// grants is an Authorizer with a fixed set of allowed actions.
type grants map[identity.Action]bool

func (g grants) Authorize(_ context.Context, _ identity.Principal, act identity.Action, _ identity.Resource) error {
	if g[act] {
		return nil
	}
	return identity.Denied("not granted")
}

// nobody is a Principal with no identity of its own.
type nobody struct{}

func (nobody) Partition() shape.Added { return shapetest.Added(1) }
func (nobody) Subject() string        { return "" }

// groupPrincipal is a Principal with no identity of its own, in the given group.
type groupPrincipal int64

func (t groupPrincipal) Partition() shape.Added { return shapetest.Added(int64(t)) }
func (groupPrincipal) Subject() string          { return "" }
