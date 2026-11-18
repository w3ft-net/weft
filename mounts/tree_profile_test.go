package mounts

import (
	"context"
	"io/fs"
	"strings"
	"testing"
	"time"

	"github.com/graybeardcorp/warp/pkg/shellcore"
)

// countingLogMount wraps *LogMount and records every ReadDir/Walk the
// tree traversal performs, so we can profile the traversal shape
// without a live coordinator or DB. It is otherwise a transparent
// delegate. It promotes Walk so MountFS.Tree takes the WalkMount path.
type countingLogMount struct {
	inner     *LogMount
	rels      []string // every rel passed to ReadDir, in call order
	scans     int      // full LogServices slice scans implied by ReadDir
	walkCalls int      // Walk invocations
	walkScans int      // full LogServices slice scans implied by Walk
}

func (c *countingLogMount) Prefix() string            { return c.inner.Prefix() }
func (c *countingLogMount) Caps() shellcore.MountCaps { return c.inner.Caps() }
func (c *countingLogMount) ReadFile(ctx context.Context, r string) ([]byte, error) {
	return c.inner.ReadFile(context.Background(), r)
}
func (c *countingLogMount) Stat(ctx context.Context, r string) (fs.FileInfo, error) {
	return c.inner.Stat(context.Background(), r)
}
func (c *countingLogMount) WriteFile(ctx context.Context, r string, d []byte) error {
	return c.inner.WriteFile(context.Background(), r, d)
}
func (c *countingLogMount) Remove(ctx context.Context, r string) error {
	return c.inner.Remove(context.Background(), r)
}

func (c *countingLogMount) ReadDir(ctx context.Context, rel string) ([]fs.DirEntry, error) {
	c.rels = append(c.rels, rel)
	switch {
	case rel == "":
		c.scans++
	case strings.Count(rel, "/") == 0:
		c.scans += 2
	default:
		c.scans++
	}
	return c.inner.ReadDir(context.Background(), rel)
}

func (c *countingLogMount) Walk(ctx context.Context, rel string) ([]string, error) {
	c.walkCalls++
	c.walkScans++ // LogMount.Walk is a single pass over LogServices
	return c.inner.Walk(context.Background(), rel)
}

// buildLogServices fabricates a realistic slash-encoded app universe:
// nsCount namespaces × envCount envs × appCount apps per (ns,env).
func buildLogServices(nsCount, envCount, appCount int) []string {
	var apps []string
	for n := 0; n < nsCount; n++ {
		ns := "ns" + itoa(n)
		for e := 0; e < envCount; e++ {
			env := "env" + itoa(e)
			for a := 0; a < appCount; a++ {
				apps = append(apps, ns+"/"+env+"/svc"+itoa(a))
			}
		}
	}
	return apps
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var b []byte
	for i > 0 {
		b = append([]byte{byte('0' + i%10)}, b...)
		i /= 10
	}
	return string(b)
}

func newCountingTreeFS(t testing.TB, logServices []string) (*shellcore.MountFS, *countingLogMount) {
	cm := &countingLogMount{inner: &LogMount{cfg: ownerCfg(t, logServices)}}
	mfs := shellcore.NewMountFS()
	mfs.Register(cm)
	return mfs, cm
}

// projectedNonOwnerDBQueries returns how many uncached DB queries the
// non-owner path would incur for the recorded traversal. Each
// namespace permission check is FindOrCreateNamespace +
// GetNamespacePermission (2 queries). ReadDir checks once per
// namespace at root and once per non-root call; Walk memoises to one
// check per namespace total.
func projectedNonOwnerDBQueries(cm *countingLogMount, logServices []string) int {
	namespaces := map[string]bool{}
	for _, a := range logServices {
		if i := strings.Index(a, "/"); i >= 0 {
			namespaces[a[:i]] = true
		}
	}
	if cm.walkCalls > 0 {
		// Walk memoises: one check per namespace, total.
		return len(namespaces) * 2
	}
	permChecks := 0
	for _, rel := range cm.rels {
		if rel == "" {
			permChecks += len(namespaces)
		} else {
			permChecks++
		}
	}
	return permChecks * 2
}

// TestTreeLogUsesSingleWalk pins the fix contract: `tree /log` must
// resolve the whole namespace in exactly ONE Walk (one pass over
// LogServices, one permission check per namespace) and ZERO recursive
// ReadDir calls. Before LogMount.Walk existed this failed — the
// traversal made O(directories) ReadDir calls (301 for 7,500 apps).
func TestTreeLogUsesSingleWalk(t *testing.T) {
	apps := buildLogServices(50, 5, 30) // 7,500 apps
	mfs, cm := newCountingTreeFS(t, apps)

	if _, err := mfs.Tree(context.Background(), "/log"); err != nil {
		t.Fatalf("tree /log: %v", err)
	}

	if cm.walkCalls != 1 {
		t.Errorf("Walk calls = %d, want exactly 1 (single bulk listing)", cm.walkCalls)
	}
	if len(cm.rels) != 0 {
		t.Errorf("recursive ReadDir calls = %d, want 0 (Walk should replace them)", len(cm.rels))
	}
	if cm.walkScans != 1 {
		t.Errorf("full-slice scans = %d, want 1 (one pass over LogServices)", cm.walkScans)
	}
	nsCount := 50
	if db := projectedNonOwnerDBQueries(cm, apps); db != nsCount*2 {
		t.Errorf("projected non-owner DB queries = %d, want %d (one memoised check per namespace)", db, nsCount*2)
	}
}

// TestTreeWalkOutputParity guards the WalkMount doc promise that the
// Walk fast-path and the ReadDir fallback render byte-identical trees.
func TestTreeWalkOutputParity(t *testing.T) {
	apps := buildLogServices(8, 3, 4)
	// Include a bare namespace (no apps) and a namespace with apps
	// directly under it (no env) to exercise both LogMount branches.
	apps = append(apps, "bare", "direct/svcX", "direct/env9/svcY")

	cfg := ownerCfg(t, apps)

	// Walk path.
	walkFS := shellcore.NewMountFS()
	walkFS.Register(&LogMount{cfg: cfg})
	walkOut, err := walkFS.Tree(context.Background(), "/log")
	if err != nil {
		t.Fatalf("walk tree: %v", err)
	}

	// ReadDir-only path: a wrapper that does NOT expose Walk, forcing
	// MountFS.Tree's recursive ReadDir fallback.
	rdFS := shellcore.NewMountFS()
	rdFS.Register(readDirOnly{&LogMount{cfg: cfg}})
	rdOut, err := rdFS.Tree(context.Background(), "/log")
	if err != nil {
		t.Fatalf("readdir tree: %v", err)
	}

	if walkOut != rdOut {
		t.Errorf("tree output diverged between Walk and ReadDir paths\n--- walk ---\n%s\n--- readdir ---\n%s", walkOut, rdOut)
	}
}

// readDirOnly exposes only the shellcore.Mount surface (no Walk), so
// MountFS.Tree falls back to recursive ReadDir.
type readDirOnly struct{ lm *LogMount }

func (r readDirOnly) Prefix() string            { return r.lm.Prefix() }
func (r readDirOnly) Caps() shellcore.MountCaps { return r.lm.Caps() }
func (r readDirOnly) ReadDir(ctx context.Context, rel string) ([]fs.DirEntry, error) {
	return r.lm.ReadDir(context.Background(), rel)
}
func (r readDirOnly) ReadFile(ctx context.Context, rel string) ([]byte, error) {
	return r.lm.ReadFile(context.Background(), rel)
}
func (r readDirOnly) Stat(ctx context.Context, rel string) (fs.FileInfo, error) {
	return r.lm.Stat(context.Background(), rel)
}
func (r readDirOnly) WriteFile(ctx context.Context, rel string, d []byte) error {
	return r.lm.WriteFile(context.Background(), rel, d)
}
func (r readDirOnly) Remove(ctx context.Context, rel string) error {
	return r.lm.Remove(context.Background(), rel)
}

// TestProfileTreeLog is the informational profile retained from the
// original investigation. It logs the traversal shape across sizes;
// post-fix it should report walkCalls=1 and zero recursive ReadDir.
func TestProfileTreeLog(t *testing.T) {
	cases := []struct{ ns, env, app int }{
		{5, 3, 5},   // small: 75 apps
		{20, 4, 15}, // medium: 1200 apps
		{50, 5, 30}, // large: 7500 apps
	}
	for _, c := range cases {
		apps := buildLogServices(c.ns, c.env, c.app)
		mfs, cm := newCountingTreeFS(t, apps)
		start := time.Now()
		if _, err := mfs.Tree(context.Background(), "/log"); err != nil {
			t.Fatalf("tree /log: %v", err)
		}
		el := time.Since(start)
		t.Logf("ns=%d env=%d app=%d apps=%d | Walk calls=%d  ReadDir calls=%d  slice scans=%d  projected non-owner DB queries=%d  wall=%s",
			c.ns, c.env, c.app, len(apps), cm.walkCalls, len(cm.rels),
			cm.walkScans+cm.scans, projectedNonOwnerDBQueries(cm, apps), el)
	}
}

func BenchmarkTreeLog(b *testing.B) {
	apps := buildLogServices(50, 5, 30) // 7500 apps
	mfs := shellcore.NewMountFS()
	mfs.Register(&LogMount{cfg: ownerCfg(b, apps)})
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := mfs.Tree(context.Background(), "/log"); err != nil {
			b.Fatal(err)
		}
	}
}
