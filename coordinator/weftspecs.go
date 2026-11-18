package coordinator

import (
	"io"

	"github.com/graybeardcorp/warp/pkg/bash"
)

// weftRun is the body of a weft:: command: the operands, then stdio.
type weftRun = func(args []string, stdin io.Reader, stdout, stderr io.Writer) int

// regSpec registers a weft:: command declared by spec. The interpreter parses
// the command line, so a wrong operand count is a usage error and --help works
// like every other command's. Flags the body reads positionally (--average,
// --above, --below) are put back after the operands, where it expects them.
func regSpec(interp *bash.Interpreter, spec bash.Spec, run weftRun) {
	spec.Strategy = bash.StageBarrier
	spec.NumericOperands = true // a threshold may be negative
	interp.RegisterSpec(spec, func(in *bash.Inv) int {
		args := in.Operands()
		for _, f := range spec.Flags {
			if in.Bool(f.Name) {
				args = append(args, "--"+f.Name)
			}
		}
		return run(args, in.Stdin, in.Stdout, in.Stderr)
	})
}

// cronOps declares operands, the first being a cron name.
func cronOps(min, max int, names ...string) bash.OperandSpec {
	return bash.Operands(names...).Between(min, max).Complete(bash.NoCompletion)
}
