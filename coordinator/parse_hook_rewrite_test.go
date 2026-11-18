package coordinator

import (
	"context"
	"testing"

	"github.com/graybeardcorp/warp/pkg/bash"
	"github.com/w3ft-net/weft/shape/shapetest"
)

// captureAfterRewrite runs script on an interpreter carrying the same rewrite
// hook the coordinator installs, and returns the post-rewrite trees warp saw for each origin
// kind. A later hook records the tree and then empties it, so the callbacks
// (which need a live Coordinator) never run.
func captureAfterRewrite(t *testing.T, fs *bash.MemFS, script string) map[bash.ParseKind]*bash.Script {
	t.Helper()
	interp := bash.NewInterpreter()
	interp.SetFS(fs)
	interp.UseParse(func(_ bash.ParseOrigin, s *bash.Script) {
		rewriteQueryPipelines(context.Background(), s, nil, TimeRange{}, shapetest.Added(0), "", nil, nil)
	})
	got := map[bash.ParseKind]*bash.Script{}
	interp.UseParse(func(o bash.ParseOrigin, s *bash.Script) {
		if o.Kind == bash.ParseRoot {
			return
		}
		snap := *s
		got[o.Kind] = &snap
		*s = bash.Script{}
	})
	ast, err := bash.Parse(script)
	if err != nil {
		t.Fatal(err)
	}
	interp.ExecScript(ast)
	return got
}

// eval previously bypassed BinScriptTransform, so a query typed through eval
// reached warp's own cat instead of runQuery.
func TestParseHook_EvalBodyIsRewritten(t *testing.T) {
	got := captureAfterRewrite(t, bash.NewMemFS(), `eval "cat /log/foo.log"`)
	s := got[bash.ParseEval]
	if s == nil {
		t.Fatal("eval body never reached the parse hook chain")
	}
	singleCallbackInScript(t, s)
}

func TestParseHook_SourcedFileIsRewritten(t *testing.T) {
	fs := bash.NewMemFS()
	fs.WriteFile(context.Background(), "/lib.sh", []byte("cat /log/foo.log"))
	got := captureAfterRewrite(t, fs, `source /lib.sh`)
	s := got[bash.ParseSource]
	if s == nil {
		t.Fatal("sourced file never reached the parse hook chain")
	}
	singleCallbackInScript(t, s)
}

func TestParseHook_BinScriptIsRewritten(t *testing.T) {
	fs := bash.NewMemFS()
	fs.WriteFile(context.Background(), "/bin/errs", []byte("cat /log/foo.log"))
	got := captureAfterRewrite(t, fs, `errs`)
	s := got[bash.ParseExecutable]
	if s == nil {
		t.Fatal("/bin script never reached the parse hook chain")
	}
	singleCallbackInScript(t, s)
}
