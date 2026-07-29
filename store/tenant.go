package store

import (
	"context"
	"errors"
	"fmt"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// Enforced multi-tenancy.
//
// Most tables in this store are owned by an org and carry an
// org_id column. Historically the only thing stopping a query
// from reading across tenants was the discipline of every store
// method threading an orgID into its WHERE clause. That's an
// *enforced* boundary only as long as nobody forgets — and "nobody
// forgets" is not a security model for a multi-tenant log service.
//
// This file makes the boundary structural. A model is *enrolled* as
// tenant-scoped; thereafter every Query/Row/Update/Delete against it
// issued through db.WithContext(ctx) is automatically constrained to
// the customer named by ctx, and a query whose ctx names no customer
// fails closed (ErrNoOrgInContext) rather than scanning the
// whole table. The deliberate cross-tenant cases (admin fleet views,
// billing sweeps, migrations) opt out explicitly via
// WithoutTenantScope.
//
// Enrollment is opt-in per model (see enrollTenantModels) so the
// boundary can be adopted one table at a time: until a model is
// enrolled its access is unchanged, and once enrolled every call site
// must pass a customer through context.

// tenantColumn is the column every enrolled model scopes on. Migration
// 053 (subscriptions → customer surrogate) made this uniform across
// all tenant tables.
const tenantColumn = "org_id"

// ErrNoOrgInContext is returned (fail-closed) when a query
// against an enrolled tenant model carries neither a customer
// (WithOrg) nor an explicit exemption (WithoutTenantScope). It
// means a code path tried to touch tenant-owned rows without
// declaring whose — surfaced as an error instead of a silent
// full-table read.
var ErrNoOrgInContext = errors.New("store: tenant-scoped query without a customer in context (use WithOrg or WithoutTenantScope)")

type orgCtxKey struct{}
type skipScopeCtxKey struct{}

// WithOrg returns a context that scopes any enrolled-model query
// issued through db.WithContext(ctx) to orgID. This is the only
// sanctioned way to read or write an enrolled model's rows.
func WithOrg(ctx context.Context, orgID string) context.Context {
	return context.WithValue(ctx, orgCtxKey{}, orgID)
}

// WithoutTenantScope marks a context exempt from automatic scoping —
// for the rare legitimate cross-tenant operations (admin fleet
// listings, billing sweeps, migrations/backfills). It is the
// deliberate hole in the boundary; reach for it consciously.
func WithoutTenantScope(ctx context.Context) context.Context {
	return context.WithValue(ctx, skipScopeCtxKey{}, true)
}

// orgFromContext reports the org a query is scoped to. An empty id is treated
// as *absent*, not as an org whose id happens to be "" — a zero-value string
// threaded in by mistake must fail closed like any other missing scope, rather
// than emitting `WHERE org_id = ''` and quietly matching nothing.
func orgFromContext(ctx context.Context) (string, bool) {
	if ctx == nil {
		return "", false
	}
	id, ok := ctx.Value(orgCtxKey{}).(string)
	if !ok || id == "" {
		return "", false
	}
	return id, true
}

func tenantScopeSkipped(ctx context.Context) bool {
	if ctx == nil {
		return false
	}
	skip, _ := ctx.Value(skipScopeCtxKey{}).(bool)
	return skip
}

// tenantScoping holds the set of enrolled tables and the GORM
// callbacks that enforce scoping. One instance is created per parent
// Store at Open and shared by pointer with the transaction-Stores
// BeginTest derives, so enrollment and enforcement read the same set.
type tenantScoping struct {
	tables map[string]bool // table name → enrolled
}

func newTenantScoping() *tenantScoping {
	return &tenantScoping{tables: map[string]bool{}}
}

// install registers the scoping callbacks on db. They run Before
// GORM's own query/row/update/delete/create processors so the
// injected WHERE (or the fail-closed error) is in place before the
// SQL is built. Callbacks registered on a parent DB are inherited by
// the transactions it spawns, so a single install at Open covers
// BeginTest transactions too.
func (ts *tenantScoping) install(db *gorm.DB) {
	_ = db.Callback().Query().Before("gorm:query").Register("tenant:scope_query", ts.scope)
	_ = db.Callback().Row().Before("gorm:row").Register("tenant:scope_row", ts.scope)
	_ = db.Callback().Update().Before("gorm:update").Register("tenant:scope_update", ts.scope)
	_ = db.Callback().Delete().Before("gorm:delete").Register("tenant:scope_delete", ts.scope)
	_ = db.Callback().Create().Before("gorm:create").Register("tenant:guard_create", ts.guardCreate)
}

// enroll marks each model's table tenant-scoped. Parsing the model is
// schema-only (no DB round-trip); the model must declare the
// tenantColumn or enrollment panics — a misconfiguration we want to
// surface at boot, not at query time.
func (ts *tenantScoping) enroll(db *gorm.DB, models ...any) {
	for _, m := range models {
		stmt := &gorm.Statement{DB: db}
		if err := stmt.Parse(m); err != nil {
			panic(fmt.Sprintf("store: enroll tenant model %T: parse: %v", m, err))
		}
		if _, ok := stmt.Schema.FieldsByDBName[tenantColumn]; !ok {
			panic(fmt.Sprintf("store: enroll tenant model %T: no %s column", m, tenantColumn))
		}
		ts.tables[stmt.Schema.Table] = true
	}
}

// scoped reports whether the statement targets an enrolled table.
// Raw SQL (no resolved schema) is always exempt — it is the
// developer's explicit responsibility.
func (ts *tenantScoping) scoped(stmt *gorm.Statement) bool {
	return stmt != nil && stmt.Schema != nil && ts.tables[stmt.Schema.Table]
}

// scope injects WHERE org_id = <ctx org> for reads, updates,
// and deletes against enrolled models, or fails closed if the context
// names no customer.
func (ts *tenantScoping) scope(db *gorm.DB) {
	if db.Error != nil || !ts.scoped(db.Statement) {
		return
	}
	ctx := db.Statement.Context
	if tenantScopeSkipped(ctx) {
		return
	}
	cid, ok := orgFromContext(ctx)
	if !ok {
		_ = db.AddError(ErrNoOrgInContext)
		return
	}
	db.Statement.AddClause(clause.Where{Exprs: []clause.Expression{
		clause.Eq{Column: clause.Column{Name: tenantColumn}, Value: cid},
	}})
}

// guardCreate fails an INSERT against an enrolled model closed if the
// context names no customer. It does not rewrite the row's
// org_id — callers set that from their own scope — but it
// refuses a blind, tenant-less create.
func (ts *tenantScoping) guardCreate(db *gorm.DB) {
	if db.Error != nil || !ts.scoped(db.Statement) {
		return
	}
	ctx := db.Statement.Context
	if tenantScopeSkipped(ctx) {
		return
	}
	if _, ok := orgFromContext(ctx); !ok {
		_ = db.AddError(ErrNoOrgInContext)
	}
}

// DB exposes the underlying *gorm.DB. Tenant-scoped (enrolled) models
// must be reached through DB().WithContext(WithOrg(ctx, id)) so
// the scoping callbacks can constrain the query — unscoped access to
// an enrolled model fails closed with ErrNoOrgInContext.
func (s *Store) DB() *gorm.DB { return s.db }

// scoped returns a session bound to orgID for enrolled-model
// access: the scoping callback constrains every query on it to that
// customer's rows. Shorthand for DB().WithContext(WithOrg(ctx,
// id)). Store methods use a background context for now — they don't
// yet thread a request context (a separate change); the customer is
// what the boundary needs.
func (s *Store) scoped(orgID string) *gorm.DB {
	return s.db.WithContext(WithOrg(context.Background(), orgID))
}

// crossTenant returns a session exempt from tenant scoping, for the
// legitimate non-scoped access paths against enrolled models (token
// auth by hash, Stripe webhooks by subscription id, admin fleet
// views). The deliberate hole — every use is a place we've decided a
// query may span customers.
func (s *Store) crossTenant() *gorm.DB {
	return s.db.WithContext(WithoutTenantScope(context.Background()))
}
