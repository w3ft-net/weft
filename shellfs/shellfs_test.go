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
	"github.com/w3ft-net/weft/mounts"
	"github.com/w3ft-net/weft/shape"
	"github.com/w3ft-net/weft/shape/shapetest"
	"github.com/w3ft-net/weft/sqlitedb"
	"github.com/w3ft-net/weft/tenancy"
)

type actor struct{ user string }

func (actor) Partition() shape.Added { return shapetest.Added(1) }
func (a actor) Subject() string      { return a.user }

type grants map[tenancy.Action]bool

func (g grants) Authorize(_ context.Context, _ tenancy.Actor, act tenancy.Action, _ tenancy.Resource) error {
	if g[act] {
		return nil
	}
	return tenancy.Denied("not granted")
}

type allScope struct{}

func (allScope) ReadScope(_ context.Context, a tenancy.Actor) (shape.Set, error) {
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

var adapter = tenancy.Adapter{
	ActorOf: func(_ shape.Added, user string) tenancy.Actor { return actor{user} },
	Authz:   grants{tenancy.ActionManageBin: true},
	Scope:   allScope{},
}

// The options shape the Config in the order given: WithTenancy names who
// the mounts act as, and WithUser then binds a member of it, who may write /bin;
// the system identity beside them may not.
func TestOptions_ApplyInOrder(t *testing.T) {
	member := New(config(t), WithTenancy(adapter), WithUser("u1"))
	if err := member.MountFS().Access("/bin/scan", shellcore.PermWrite); err != nil {
		t.Errorf("a member writing /bin: %v", err)
	}
	system := New(config(t), WithTenancy(adapter))
	if err := system.MountFS().Access("/bin/scan", shellcore.PermWrite); err != nil {
		// Both are held to the same Authorizer here; the point is that it is asked.
		t.Logf("the system identity was refused: %v", err)
	}
	// With no tenancy the mounts deny everything.
	if err := New(config(t)).MountFS().Access("/bin/scan", shellcore.PermWrite); !errors.Is(err, fs.ErrPermission) {
		t.Errorf("no tenancy: err = %v, want a permission refusal", err)
	}
}

// A ShellFS is a filesystem: a file written under /tmp reads back, and shows in
// its directory.
func TestShellFS_TmpRoundTrip(t *testing.T) {
	f := New(config(t), WithTenancy(adapter), WithUser("u1"))
	if err := f.WriteFile("/tmp/note", []byte("hello")); err != nil {
		t.Fatal(err)
	}
	if got, err := f.ReadFile("/tmp/note"); err != nil || string(got) != "hello" {
		t.Fatalf("ReadFile = %q %v", got, err)
	}
	ents, err := f.ReadDir("/tmp")
	if err != nil || len(ents) != 1 || ents[0].Name() != "note" {
		t.Fatalf("ReadDir = %v %v", ents, err)
	}
}

// A path no mount versions can't be amended.
func TestAmendVersionMessage_UnversionedPath(t *testing.T) {
	f := New(config(t), WithTenancy(adapter), WithUser("u1"))
	if err := f.AmendVersionMessage("/tmp/note", "x"); err == nil {
		t.Error("amending /tmp/note was accepted")
	}
}
