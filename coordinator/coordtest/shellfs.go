//go:build test

package coordtest

import (
	"context"
	"reflect"
	"strings"
	"testing"

	"github.com/w3ft-net/weft/coordinator"
)

// ShellFS runs the shell's navigation over the virtual filesystem on the
// server: the dirstack a delegated client ships with each line and reads back,
// ls and tree resolved against that cwd, relative sources, and the refusals for
// a glob that spans sources and a follow that is redirected.
func ShellFS(t *testing.T, newSubject func(t *testing.T) Subject) {
	for _, c := range []struct {
		name string
		run  func(*testing.T, *queryFixture)
	}{
		{"CourierSeedReadback", courierSeedReadback},
		{"CourierCdMovesCwd", courierCdMovesCwd},
		{"CourierRelativeCdResolvesAgainstSeed", courierRelativeCd},
		{"CourierPushdReturnsFullStack", courierPushd},
		{"CourierNilSeedDefaultsToRoot", courierNilSeed},
		{"LsResolvesCwdAndListsNamespace", shellLs},
		{"TreeListsNamespaceFromSeededCwd", shellTree},
		{"TailRelativeSourceResolvesAgainstCwd", shellTailRelative},
		{"GlobNoMatchIsAnError", globNoMatch},
		{"GlobCrossSourceIsDisallowed", globCrossSource},
		{"FollowWithStdoutRedirectIsRefused", followRedirectRefused},
	} {
		t.Run(c.name, func(t *testing.T) { c.run(t, newBareFixture(t, newSubject(t))) })
	}
}

// at runs line with the dirstack seed as the owner and returns the drained
// result with the result itself, for its FinalDirStack. An error from Execute
// is returned as the drained stderr's stand-in so a refusal reads the same
// whichever way it surfaces.
func (f *queryFixture) at(t *testing.T, line string, seed []string) (*coordinator.ExecuteResult, *coordinator.DrainedResult, error) {
	t.Helper()
	res, err := f.c.ExecuteWithDir(context.Background(), line, coordinator.TimeRange{}, f.s.ActorOf(""), seed)
	if err != nil {
		return nil, nil, err
	}
	d, err := res.Drain(context.Background())
	if err != nil {
		t.Fatalf("drain %q: %v", line, err)
	}
	return res, d, nil
}

func (f *queryFixture) mustAt(t *testing.T, line string, seed []string) (*coordinator.ExecuteResult, *coordinator.DrainedResult) {
	t.Helper()
	res, d, err := f.at(t, line, seed)
	if err != nil {
		t.Fatalf("ExecuteWithDir %q: %v", line, err)
	}
	return res, d
}

func (f *queryFixture) acmeNamespace(t *testing.T) {
	t.Helper()
	if _, err := f.s.Sources.FindOrCreateNamespace(f.s.Owner, "acme"); err != nil {
		t.Fatal(err)
	}
}

// seedAcme creates namespace acme and registers two services under it: one
// env-less (acme/api, the file "api.log") and one env-bearing (acme/worker in
// prod, the "prod/" directory), so a listing shows both arms.
func (f *queryFixture) seedAcme(t *testing.T) {
	t.Helper()
	f.acmeNamespace(t)
	f.c.SeenServices.Add(f.s.Owner, "acme/api", "")
	f.c.SeenServices.Add(f.s.Owner, "acme/worker", "prod")
}

// A seeded cwd is the interpreter's cwd for the line: pwd prints it and the
// final stack echoes it back when the line does not move.
func courierSeedReadback(t *testing.T, f *queryFixture) {
	res, d := f.mustAt(t, "pwd", []string{"/log/acme"})
	if got := strings.TrimSpace(d.Text); got != "/log/acme" {
		t.Errorf("pwd = %q, want /log/acme (stderr=%q)", got, d.Stderr)
	}
	if want := []string{"/log/acme"}; !reflect.DeepEqual(res.FinalDirStack, want) {
		t.Errorf("FinalDirStack = %v, want %v", res.FinalDirStack, want)
	}
}

// A cd in the shipped line moves the cwd and the new one comes back: the server
// owns cd semantics.
func courierCdMovesCwd(t *testing.T, f *queryFixture) {
	res, d := f.mustAt(t, "cd /bin && pwd", []string{"/tmp"})
	if got := strings.TrimSpace(d.Text); got != "/bin" {
		t.Errorf("cd /bin && pwd = %q, want /bin (stderr=%q)", got, d.Stderr)
	}
	if want := []string{"/bin"}; !reflect.DeepEqual(res.FinalDirStack, want) {
		t.Errorf("FinalDirStack = %v, want %v", res.FinalDirStack, want)
	}
}

// A relative cd resolves against the seeded cwd, not "/".
func courierRelativeCd(t *testing.T, f *queryFixture) {
	res, d := f.mustAt(t, "cd sub && pwd", []string{"/log/acme"})
	if got := strings.TrimSpace(d.Text); got != "/log/acme/sub" {
		t.Errorf("cd sub && pwd = %q, want /log/acme/sub (stderr=%q)", got, d.Stderr)
	}
	if want := []string{"/log/acme/sub"}; !reflect.DeepEqual(res.FinalDirStack, want) {
		t.Errorf("FinalDirStack = %v, want %v", res.FinalDirStack, want)
	}
}

// pushd grows the stack and the whole of it comes back, index 0 being the cwd.
func courierPushd(t *testing.T, f *queryFixture) {
	res, _ := f.mustAt(t, "pushd /bin", []string{"/tmp"})
	if want := []string{"/bin", "/tmp"}; !reflect.DeepEqual(res.FinalDirStack, want) {
		t.Errorf("FinalDirStack after pushd = %v, want %v", res.FinalDirStack, want)
	}
}

// Plain Execute with no seed starts at "/" and returns a one-entry stack.
func courierNilSeed(t *testing.T, f *queryFixture) {
	res, err := f.c.Execute(context.Background(), "pwd", coordinator.TimeRange{}, f.s.ActorOf(""))
	if err != nil {
		t.Fatal(err)
	}
	d, err := res.Drain(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(d.Text); got != "/" {
		t.Errorf("pwd (no seed) = %q, want / (stderr=%q)", got, d.Stderr)
	}
	if want := []string{"/"}; !reflect.DeepEqual(res.FinalDirStack, want) {
		t.Errorf("FinalDirStack (no seed) = %v, want %v", res.FinalDirStack, want)
	}
}

// ls with no args inside /log/acme lists acme's children, not the root mounts,
// and the services the coordinator has seen are what it lists.
func shellLs(t *testing.T, f *queryFixture) {
	f.seedAcme(t)
	_, d := f.mustAt(t, "ls", []string{"/log/acme"})
	for _, root := range []string{"bin/", "cron.d/", "etc/", "tmp/"} {
		if strings.Contains(d.Text, root) {
			t.Fatalf("ls in /log/acme leaked root mount %q\noutput:\n%s\nstderr: %s", root, d.Text, d.Stderr)
		}
	}
	if !strings.Contains(d.Text, "api.log") {
		t.Errorf("ls did not list service api.log\noutput:\n%s\nstderr: %s", d.Text, d.Stderr)
	}
	if !strings.Contains(d.Text, "prod") {
		t.Errorf("ls did not list env directory prod\noutput:\n%s\nstderr: %s", d.Text, d.Stderr)
	}
}

// tree with no args inside /log/acme renders acme's subtree.
func shellTree(t *testing.T, f *queryFixture) {
	f.seedAcme(t)
	_, d := f.mustAt(t, "tree", []string{"/log/acme"})
	for _, want := range []string{"api", "worker"} {
		if !strings.Contains(d.Text, want) {
			t.Errorf("tree did not include service %s\noutput:\n%s\nstderr: %s", want, d.Text, d.Stderr)
		}
	}
}

// From /log/vimgym/api, `tail gamestate.log` resolves against the cwd to
// /log/vimgym/api/gamestate.log rather than reading as the namespace
// "gamestate". There are no shards, so the result is empty.
func shellTailRelative(t *testing.T, f *queryFixture) {
	_, d := f.mustAt(t, "tail gamestate.log", []string{"/log/vimgym/api"})
	if strings.Contains(d.Stderr, "unknown source") {
		t.Fatalf("tail gamestate.log from /log/vimgym/api was not resolved against cwd: %s", d.Stderr)
	}
}

// A glob in a source command that is not the first of the line (a `&&` or `;`
// chain, a /bin script body) expands as the first would; unexpanded, `*` reached
// the planner as a literal kind and the query silently returned nothing.
func globNoMatch(t *testing.T, f *queryFixture) {
	f.acmeNamespace(t)
	f.c.SeenServices.Add(f.s.Owner, "acme/api/main", "prod")
	for _, line := range []string{
		`cd /log/acme/prod/api && tail -n 1 *.nomatch`,
		`cd /log/acme/prod/api; tail -n 1 *.nomatch`,
		`tail -n 1 /log/acme/prod/api/*.nomatch`,
	} {
		got := f.refusal(t, line)
		if !strings.Contains(got, "no pseudofiles match") {
			t.Errorf("%q: unmatched glob was silent or wrong: %q", line, got)
		}
	}
}

// A glob that would span services or namespaces is refused wherever it sits.
func globCrossSource(t *testing.T, f *queryFixture) {
	f.acmeNamespace(t)
	f.c.SeenServices.Add(f.s.Owner, "acme/api/main", "prod")
	for _, line := range []string{
		`cat *.log | grep x`,
		`cd /log/acme && cat *.log | grep x`,
		`cd /log/acme/prod; tail -n 1 *.log`,
		`cat /log/acme/prod/*.log`,
	} {
		got := f.refusal(t, line)
		if !strings.Contains(got, "not allowed") || !strings.Contains(got, "across sources") {
			t.Errorf("%q: want an explicit cross-source refusal, got %q", line, got)
		}
	}
}

// `tail -f ... > /tmp/x` is refused on the real execution path: the rewriter
// lifts redirects off the pipeline before it is planned, so without the check
// here the follow ran as a finite read and left a truncated file, exit 0.
func followRedirectRefused(t *testing.T, f *queryFixture) {
	f.acmeNamespace(t)
	for _, line := range []string{
		`tail -f /log/acme/prod/api/main.log > /tmp/stream`,
		`tail -f /log/acme/prod/api/main.log | grep ERROR >> /tmp/stream`,
	} {
		got := f.refusal(t, line)
		if !strings.Contains(got, "tail -f") || !strings.Contains(got, "can't be redirected") {
			t.Errorf("%q: want a refusal naming tail -f, got %q", line, got)
		}
	}
}

// refusal is what line says when it is refused: the error, or the stderr of a
// result that carried it.
func (f *queryFixture) refusal(t *testing.T, line string) string {
	t.Helper()
	_, d, err := f.at(t, line, nil)
	if err != nil {
		return err.Error()
	}
	return d.Stderr
}
