package coordinator

import (
	"strings"
	"testing"

	"github.com/graybeardcorp/warp/pkg/bash"
)

// The weft:: commands are declared with specs, so the interpreter rejects a
// wrong operand count, answers --help, and does not read a negative threshold
// as a flag.
func TestWeftCommandsAreSpecs(t *testing.T) {
	interp := bash.NewInterpreter()
	wf := &WeftFuncs{}
	RegisterWeftCommands(interp, wf)
	RegisterWeftPredicates(interp, wf)

	for _, c := range []struct {
		name string
		args []string
		want string // "" = parses
		exit int
	}{
		{"weft::is_above", []string{"x"}, "missing operand", 2},
		{"weft::is_above", []string{"x", "1", "2", "3"}, "extra operand '3'", 2},
		{"weft::is_above", []string{"x", "-5"}, "", 0},
		{"weft::is_above", []string{"x", "1", "5m", "--average"}, "", 0},
		{"weft::avg", []string{"x"}, "missing operand", 1},
		{"weft::latest", nil, "missing operand", 1},
		{"weft::is_outlier", []string{"x", "3", "5m", "--above"}, "", 0},
		{"weft::is_outlier", []string{"x", "3", "5m", "--sideways"}, "unrecognized option '--sideways'", 2},
		{"weft::percentile", []string{"x", "5m"}, "missing operand", 1},
		{"weft::is_gt", []string{"-3"}, "", 0},
	} {
		sp, ok := interp.LookupSpec(c.name)
		if !ok {
			t.Fatalf("%s has no spec", c.name)
		}
		_, err := sp.Parse(c.args)
		switch {
		case c.want == "" && err != nil:
			t.Errorf("%s %q: %v", c.name, c.args, err)
		case c.want != "" && (err == nil || !strings.Contains(err.Error(), c.want)):
			t.Errorf("%s %q: err %v, want %q", c.name, c.args, err, c.want)
		}
		if c.want != "" && sp.ExitCode() != c.exit {
			t.Errorf("%s: usage exit %d, want %d", c.name, sp.ExitCode(), c.exit)
		}
	}
	// --help comes with the spec, and the TypeDoc survives.
	sp, _ := interp.LookupSpec("weft::is_above")
	if p, err := sp.Parse([]string{"--help"}); err != nil || !p.Help {
		t.Errorf("--help: %v %v", p, err)
	}
	if !strings.Contains(sp.Help(), "usage: weft::is_above") || sp.TypeDoc == "" {
		t.Errorf("help %q, typedoc %q", sp.Help(), sp.TypeDoc)
	}
}
