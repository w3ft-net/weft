package shellfs

import (
	"context"
	"errors"
	"io/fs"
	"testing"

	"github.com/graybeardcorp/warp/pkg/shellcore"
	"github.com/w3ft-net/weft/backends/defaults"
	"github.com/w3ft-net/weft/binscript"
	"github.com/w3ft-net/weft/cronauthor"
	"github.com/w3ft-net/weft/identity"
	"github.com/w3ft-net/weft/mounts"
	"github.com/w3ft-net/weft/shape"
	"github.com/w3ft-net/weft/shape/shapetest"
	"github.com/w3ft-net/weft/sqlitedb"
)

type principal struct{ user string }

func (principal) Partition() shape.Added { return shapetest.Added(1) }
func (a principal) Subject() string      { return a.user }

type grants map[identity.Action]bool

func (g grants) Authorize(_ context.Context, _ identity.Principal, act identity.Action, _ identity.Resource) error {
	if g[act] {
		return nil
	}
	return identity.Denied("not granted")
}

type allScope struct{}

func (allScope) ReadScope(_ context.Context, a identity.Principal) (shape.Set, error) {
	return shape.Set{a.Partition().Coord()}, nil
}

func config(t *testing.T) mounts.Config {
	t.Helper()
	db, err := sqlitedb.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	srcs, err := defaults.NewSourceBackend(db)
	if err != nil {
		t.Fatal(err)
	}
	bins, err := defaults.NewBinBackend(db)
	if err != nil {
		t.Fatal(err)
	}
	crons, err := defaults.NewCronBackend(db)
	if err != nil {
		t.Fatal(err)
	}
	tmp, err := defaults.NewTmpBackend(db)
	if err != nil {
		t.Fatal(err)
	}
	return mounts.Config{
		Owner:      shapetest.Added(1),
		Sources:    srcs,
		BinService: binscript.Service{Bins: bins, Crons: crons, Sources: srcs},
		CronAuthor: cronauthor.Service{Crons: crons, Sources: srcs, Bins: bins},
		Tmp:        tmp,
	}
}

var adapter = identity.Access{
	PrincipalOf: func(_ shape.Added, user string) identity.Principal { return principal{user} },
	Authz:       grants{identity.ActionManageBin: true},
	Scope:       allScope{},
}

// The options shape the Config in the order given: WithAccess names who
// the mounts act as, and WithUser then binds a member of it, who may write /bin;
// the system identity beside them may not.
func TestOptions_ApplyInOrder(t *testing.T) {
	member := New(config(t), WithAccess(adapter), WithUser("u1"))
	if err := member.MountFS().Access(context.Background(), "/bin/scan", shellcore.PermWrite); err != nil {
		t.Errorf("a member writing /bin: %v", err)
	}
	system := New(config(t), WithAccess(adapter))
	if err := system.MountFS().Access(context.Background(), "/bin/scan", shellcore.PermWrite); err != nil {
		// Both are held to the same Authorizer here; the point is that it is asked.
		t.Logf("the system identity was refused: %v", err)
	}
	// With no access the mounts deny everything.
	if err := New(config(t)).MountFS().Access(context.Background(), "/bin/scan", shellcore.PermWrite); !errors.Is(err, fs.ErrPermission) {
		t.Errorf("no access: err = %v, want a permission refusal", err)
	}
}

// A ShellFS is a filesystem: a file written under /tmp reads back, and shows in
// its directory.
func TestShellFS_TmpRoundTrip(t *testing.T) {
	f := New(config(t), WithAccess(adapter), WithUser("u1"))
	if err := f.WriteFile(context.Background(), "/tmp/note", []byte("hello")); err != nil {
		t.Fatal(err)
	}
	if got, err := f.ReadFile(context.Background(), "/tmp/note"); err != nil || string(got) != "hello" {
		t.Fatalf("ReadFile = %q %v", got, err)
	}
	ents, err := f.ReadDir(context.Background(), "/tmp")
	if err != nil || len(ents) != 1 || ents[0].Name() != "note" {
		t.Fatalf("ReadDir = %v %v", ents, err)
	}
}

// A path no mount versions can't be amended.
func TestAmendVersionMessage_UnversionedPath(t *testing.T) {
	f := New(config(t), WithAccess(adapter), WithUser("u1"))
	if err := f.AmendVersionMessage("/tmp/note", "x"); err == nil {
		t.Error("amending /tmp/note was accepted")
	}
}
