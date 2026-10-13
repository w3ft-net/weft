package mounts

import (
	"fmt"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/graybeardcorp/weft/store"
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
	return Config{Store: tx, OrgID: org.ID, IsOwner: true, LogServices: services}
}
