//go:build !defaultshape

package mounts

import (
	"context"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/graybeardcorp/weft/shape"
	"github.com/graybeardcorp/weft/shape/shapetest"
	"github.com/graybeardcorp/weft/store"
	"github.com/graybeardcorp/weft/tenancy"
	"github.com/graybeardcorp/weft/tenancy/hosted"
)

// ownerCfgSeq makes each ownerCfg org slug unique: a test may call it more
// than once inside one transaction-scoped DB, and the org slug is unique.
var ownerCfgSeq atomic.Int64

// ownerCfg returns an owner Config over a rolled-back test org holding one
// namespace per first path segment of services.
func ownerCfg(t testing.TB, services []string) Config {
	t.Helper()
	db, err := store.Open(store.TestDSN(t))
	if err != nil {
		t.Skipf("postgres unavailable: %v", err)
	}
	tx, rollback := db.BeginTest()
	t.Cleanup(func() {
		rollback()
		db.Close()
	})
	org, err := tx.FindOrCreateOrg(fmt.Sprintf("mounts-owner-cfg-%d", ownerCfgSeq.Add(1)))
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for _, a := range services {
		ns, _, _ := strings.Cut(a, "/")
		if seen[ns] {
			continue
		}
		seen[ns] = true
		if _, err := tx.FindOrCreateNamespace(org.ID, ns); err != nil {
			t.Fatal(err)
		}
	}
	// The system identity reads every namespace and writes nothing.
	return asHosted(Config{Store: tx, OrgID: org.ID, LogServices: services}, "")
}

// asHosted gives cfg the hosted adapter over its store, acting as user. The
// empty user is the system identity.
func asHosted(cfg Config, user string) Config {
	cfg.UserID = user
	cfg.Actor = hosted.Actor{Org: cfg.OrgID, User: user}
	cfg.Authz = hosted.Authorizer{Store: cfg.Store}
	cfg.Scope = hosted.Scoper{Store: cfg.Store}
	return cfg
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

// tenantActor is an Actor with no identity of its own, in the given org.
type tenantActor int64

func (t tenantActor) Partition() shape.Added { return shapetest.Added(int64(t)) }
func (tenantActor) Subject() string          { return "" }
