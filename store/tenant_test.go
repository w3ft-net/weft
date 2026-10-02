package store

import (
	"context"
	"errors"
	"testing"
)

// tenantProbe is a throwaway tenant-scoped model used only to exercise
// the scoping callbacks against a real table without enrolling (and
// thereby constraining) a production model.
type tenantProbe struct {
	ID    uint  `gorm:"primaryKey"`
	OrgID OrgID `gorm:"not null"`
	Val   string
}

func (tenantProbe) TableName() string { return "tenant_probe" }

// TestTenantScoping_Enforced proves the structural multi-tenancy
// boundary: an enrolled model's reads/writes are auto-constrained to
// the context customer, an unscoped access fails closed instead of
// leaking, and the explicit exemption sees everything.
func TestTenantScoping_Enforced(t *testing.T) {
	db := testOpen(t) // returns a Store over a single-connection tx

	// A TEMP table lives on the tx's connection and is gone at rollback.
	if err := db.DB().Exec(
		`CREATE TEMP TABLE tenant_probe (id BIGSERIAL PRIMARY KEY, org_id BIGINT NOT NULL, val TEXT)`,
	).Error; err != nil {
		t.Fatalf("create probe table: %v", err)
	}
	db.tenant.enroll(db.DB(), &tenantProbe{})

	bg := context.Background()

	// Two org ids standing in for two tenants.
	const (
		orgA = 10
		orgB = 11
	)

	// Seed two customers' rows through the exemption.
	seed := db.DB().WithContext(WithoutTenantScope(bg))
	for _, p := range []tenantProbe{
		{OrgID: orgA, Val: "a1"}, {OrgID: orgA, Val: "a2"}, {OrgID: orgB, Val: "b1"},
	} {
		if err := seed.Create(&p).Error; err != nil {
			t.Fatalf("seed %+v: %v", p, err)
		}
	}

	// (1) A scoped read sees only that customer's rows.
	var mine []tenantProbe
	if err := db.DB().WithContext(WithOrg(bg, orgA)).Find(&mine).Error; err != nil {
		t.Fatalf("scoped find: %v", err)
	}
	if len(mine) != 2 {
		t.Errorf("org A saw %d rows, want 2", len(mine))
	}

	// (2) An unscoped read of an enrolled model fails closed.
	var leak []tenantProbe
	if err := db.DB().WithContext(bg).Find(&leak).Error; !errors.Is(err, ErrNoOrgInContext) {
		t.Errorf("unscoped find: err=%v (%d rows), want ErrNoOrgInContext", err, len(leak))
	}

	// (3) The explicit exemption sees everything.
	var all []tenantProbe
	if err := db.DB().WithContext(WithoutTenantScope(bg)).Find(&all).Error; err != nil {
		t.Fatalf("exempt find: %v", err)
	}
	if len(all) != 3 {
		t.Errorf("exempt find saw %d rows, want 3", len(all))
	}

	// (4) A scoped update cannot reach across the tenant boundary.
	if err := db.DB().WithContext(WithOrg(bg, orgA)).
		Model(&tenantProbe{}).Where("val = ?", "b1").
		Update("val", "hacked").Error; err != nil {
		t.Fatalf("scoped update: %v", err)
	}
	var b1 tenantProbe
	if err := db.DB().WithContext(WithoutTenantScope(bg)).
		Where("org_id = ?", orgB).First(&b1).Error; err != nil {
		t.Fatalf("reread b1: %v", err)
	}
	if b1.Val != "b1" {
		t.Errorf("org B's row was modified across the boundary: val=%q", b1.Val)
	}

	// (5) A create with no customer in context fails closed.
	if err := db.DB().WithContext(bg).Create(&tenantProbe{OrgID: orgA, Val: "x"}).Error; !errors.Is(err, ErrNoOrgInContext) {
		t.Errorf("unscoped create: err=%v, want ErrNoOrgInContext", err)
	}

	// (6) An *empty* org id is absent, not an org whose id is "". Under the
	// old uint key the zero value was 0 and equally meaningless; now that the
	// id is a string, a zero-value variable threaded in by mistake must still
	// fail closed rather than emit `WHERE org_id = ''`.
	var empty []tenantProbe
	if err := db.DB().WithContext(WithOrg(bg, 0)).Find(&empty).Error; !errors.Is(err, ErrNoOrgInContext) {
		t.Errorf("empty-org find: err=%v (%d rows), want ErrNoOrgInContext", err, len(empty))
	}
}
