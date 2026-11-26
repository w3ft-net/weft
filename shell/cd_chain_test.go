package shell

import (
	"bytes"
	"testing"
)

// ExecNonInteractive (the `weft -c "..."` one-shot path) does not rewrite
// relative paths client-side. `cd`, `pushd` and `popd` are the shell's own, so
// they run on the client (checked against its filesystem), and the commands
// between them ship to the server, a real pkg/bash interpreter, each with the
// directory stack as the cd left it. These tests pin what is shipped and with
// which stack; the *resolution* of a relative path against that stack is
// verified server-side in the coordinator's dirstack courier tests
// (TestCourier_* in weft/coordinator).

type shipRecord struct {
	line  string
	stack []string
}

func newCaptureShell() (*Shell, *string) {
	s, recs := newRecordingShell()
	var captured string
	q := s.Query
	s.Query = func(pipeline, rng string, stack []string) (*QueryResult, error) {
		captured = pipeline
		return q(pipeline, rng, stack)
	}
	_ = recs
	return s, &captured
}

func newRecordingShell() (*Shell, *[]shipRecord) {
	var recs []shipRecord
	s := &Shell{
		FS: testMountFS(nil),
		Query: func(pipeline, _ string, stack []string) (*QueryResult, error) {
			recs = append(recs, shipRecord{pipeline, stack})
			return &QueryResult{Text: ""}, nil
		},
	}
	return s, &recs
}

func checkShipped(t *testing.T, recs []shipRecord, want ...shipRecord) {
	t.Helper()
	if len(recs) != len(want) {
		t.Fatalf("shipped %d segments %+v, want %d %+v", len(recs), recs, len(want), want)
	}
	for i, w := range want {
		g := recs[i]
		if g.line != w.line || len(g.stack) == 0 || g.stack[0] != w.stack[0] {
			t.Errorf("segment %d: shipped %q with stack %v, want %q with cwd %q", i, g.line, g.stack, w.line, w.stack[0])
		}
	}
}

func TestExecNonInteractiveCdAndCatShipsTheServersPart(t *testing.T) {
	s, recs := newRecordingShell()
	var buf bytes.Buffer
	if err := s.ExecNonInteractive("cd /log/acme && cat api.log | head -1", &buf); err != nil {
		t.Fatalf("ExecNonInteractive: %v", err)
	}
	checkShipped(t, *recs, shipRecord{"cat api.log | head -1", []string{"/log/acme"}})
}

func TestExecNonInteractiveCdSemicolonShipsTheServersPart(t *testing.T) {
	s, recs := newRecordingShell()
	var buf bytes.Buffer
	if err := s.ExecNonInteractive("cd /log/acme; cat api.log", &buf); err != nil {
		t.Fatalf("ExecNonInteractive: %v", err)
	}
	checkShipped(t, *recs, shipRecord{"cat api.log", []string{"/log/acme"}})
}

// Stacked relative cds accumulate on the client.
func TestExecNonInteractiveRelativeCdChainAccumulates(t *testing.T) {
	s, recs := newRecordingShell()
	var buf bytes.Buffer
	if err := s.ExecNonInteractive("cd /log && cd acme && cat api.log", &buf); err != nil {
		t.Fatalf("ExecNonInteractive: %v", err)
	}
	checkShipped(t, *recs, shipRecord{"cat api.log", []string{"/log/acme"}})
}

// pushd and popd move the client's stack between the two cats, and each cat
// ships with the stack at that moment.
func TestExecNonInteractivePushdPopdCarriesTheStack(t *testing.T) {
	s, recs := newRecordingShell()
	var buf bytes.Buffer
	const line = "pushd /log/acme && cat api.log && popd && cat /log/other/x.log"
	if err := s.ExecNonInteractive(line, &buf); err != nil {
		t.Fatalf("ExecNonInteractive: %v", err)
	}
	checkShipped(t, *recs,
		shipRecord{"cat api.log", []string{"/log/acme"}},
		shipRecord{"cat /log/other/x.log", []string{"/"}})
}
