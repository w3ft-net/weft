package mounts

import (
	"context"
	"errors"
	"io/fs"
	"testing"

	"github.com/graybeardcorp/warp/pkg/shellcore"
	"github.com/w3ft-net/weft/identity"
	"github.com/w3ft-net/weft/shape"
	"github.com/w3ft-net/weft/shape/shapetest"
)

type staticScope struct{ set shape.Set }

func (s staticScope) ReadScope(context.Context, identity.Principal) (shape.Set, error) {
	return s.set, nil
}

// The mounts take their read scope from whatever Scoper they are given, not
// from any one build's tables, and refuse a scope they cannot enforce.
func TestLogMount_NonHostedScope(t *testing.T) {
	cfg := ownerCfg(t, []string{"alpha/prod/api/app", "beta/prod/api/app"})
	alpha := func(c shape.Coord) Config {
		c.Namespace = shape.Value("alpha")
		cfg.Principal, cfg.Scope = groupPrincipal(1), staticScope{shape.Set{shapetest.In(1, c)}}
		return cfg
	}

	m := &LogMount{cfg: alpha(shape.Coord{})}
	if err := m.Authorize(context.Background(), "alpha/prod", shellcore.PermRead); err != nil {
		t.Errorf("granted namespace: %v", err)
	}
	if err := m.Authorize(context.Background(), "beta/prod", shellcore.PermRead); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("ungranted namespace: err = %v, want ErrNotExist", err)
	}

	m = &LogMount{cfg: alpha(shape.Coord{Env: shape.Value("prod")})}
	if err := m.Authorize(context.Background(), "alpha/prod", shellcore.PermRead); !errors.Is(err, shape.ErrFinerThanNamespace) {
		t.Errorf("env-restricted scope: err = %v, want a refusal", err)
	}
}

// Authorizing a read of /log/<ns> must never create <ns>: a typo leaves nothing
// behind, and a namespace the principal may not read does not exist to them.
func TestAuthorize_NeverCreatesNamespaces(t *testing.T) {
	cfg := ownerCfg(t, []string{"readable/prod/api/app", "private/prod/api/app"})
	cfg.Principal, cfg.Scope = groupPrincipal(1), staticScope{shape.Set{shapetest.In(1, shape.Coord{Namespace: shape.Value("readable")})}}
	m := &LogMount{cfg: cfg}

	if err := m.Authorize(context.Background(), "readable/prod/api", shellcore.PermRead); err != nil {
		t.Errorf("a member with read permission must read their namespace: %v", err)
	}
	if err := m.Authorize(context.Background(), "private", shellcore.PermRead); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("a namespace the principal may not read: err = %v, want ErrNotExist", err)
	}
	for _, typo := range []string{"typo", "readabel", "no such"} {
		if err := m.Authorize(context.Background(), typo, shellcore.PermRead); !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("%q does not exist: err = %v, want ErrNotExist", typo, err)
		}
		if _, found, _ := cfg.Sources.GetNamespace(cfg.Owner, typo); found {
			t.Errorf("probing /log/%s created a namespace", typo)
		}
	}
}

// /log lists the group's namespaces from the namespace table, filtered by the
// caller's read scope, not whatever happens to have registered a service. A
// readable namespace with no services yet must still show.
func TestLogReadDir_RootListsReadableNamespaces(t *testing.T) {
	cfg := ownerCfg(t, []string{"withsvc/prod/api/app", "private/prod/api/app", "empty/prod/x/y"})
	cfg.LogServices = []string{"withsvc/prod/api/app", "private/prod/api/app"}

	list := func(c Config) []string {
		ents, err := (&LogMount{cfg: c}).ReadDir(context.Background(), "")
		if err != nil {
			t.Fatal(err)
		}
		var out []string
		for _, e := range ents {
			out = append(out, e.Name())
		}
		return out
	}

	member := cfg
	member.Principal = groupPrincipal(1)
	member.Scope = staticScope{shape.Set{
		shapetest.In(1, shape.Coord{Namespace: shape.Value("withsvc")}),
		shapetest.In(1, shape.Coord{Namespace: shape.Value("empty")}),
	}}
	if got, want := list(member), []string{"empty", "withsvc"}; !equal(got, want) {
		t.Errorf("member /log = %v, want %v", got, want)
	}
	if got, want := list(cfg), []string{"empty", "private", "withsvc"}; !equal(got, want) {
		t.Errorf("owner /log = %v, want %v", got, want)
	}
}

func equal(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
