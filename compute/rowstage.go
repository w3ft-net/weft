package compute

import (
	"bytes"
	"fmt"
	"io"
	"strconv"
	"strings"
	"sync"

	"github.com/graybeardcorp/warp/pkg/bash"
	"github.com/graybeardcorp/warp/pkg/jq"
	"github.com/graybeardcorp/weft/planner"
	pb "github.com/graybeardcorp/weft/proto/weftpb"
)

// RowStages runs planner.QueryPlan.RowStages: stages that pass rows
// through unchanged, run one row at a time so the rows reach the next
// stage whole, payload and meta-variables included. A jq select chain
// reads the row's payload; grep, sed and awk read its raw line (awk
// with its meta-variables); head/tail -n N count rows.
type RowStages struct {
	stages []rowStage
}

type rowStage struct {
	jq     jq.Expr          // a select chain, over the payload
	meta   planner.MetaRead // the meta-variables the stage names
	env    *JqEnvelope      // for a jq stage that reads some
	script *bash.Script     // a filter over the raw line
	interp *sync.Pool       // of *rowInterp, for script: built once, reused per row
	head   int              // head -n N: keep the first N (when > -1)
	tail   int              // tail -n N: keep the last N (when > -1)
}

// NewRowStages parses each stage once for every row.
func NewRowStages(stages []string) (*RowStages, error) {
	rs := &RowStages{}
	for _, src := range stages {
		st, err := parseRowStage(src)
		if err != nil {
			return nil, fmt.Errorf("row stage %q: %w", src, err)
		}
		rs.stages = append(rs.stages, st)
	}
	return rs, nil
}

func parseRowStage(src string) (rowStage, error) {
	st := rowStage{head: -1, tail: -1}
	script, err := bash.Parse(src)
	if err != nil {
		return st, err
	}
	cmd, ok := script.Commands[0].First.Commands[0].(*bash.SimpleCommand)
	if !ok {
		return st, fmt.Errorf("not a simple command")
	}
	var argv []string
	for _, w := range cmd.Words {
		argv = append(argv, w.Literal())
	}
	switch argv[0] {
	case "jq":
		for _, a := range argv[1:] {
			if !strings.HasPrefix(a, "-") {
				st.jq, err = jq.Parse(a)
				st.meta = planner.StageMetaRead(src)
				st.env = NewJqEnvelope(st.meta)
				return st, err
			}
		}
		return st, fmt.Errorf("no jq program")
	case "head", "tail":
		n, err := lineCount(argv[1:])
		if err != nil {
			return st, err
		}
		if argv[0] == "head" {
			st.head = n
		} else {
			st.tail = n
		}
		return st, nil
	}
	st.script = script
	st.meta = planner.StageMetaRead(src)
	binder := newAwkBinder(st.meta)
	st.interp = &sync.Pool{New: func() any { return newRowInterp(binder) }}
	return st, nil
}

// rowInterp is an interpreter with the tool commands registered, kept
// between rows: building one costs about as much as running a filter on a
// row. A stage's script is one simple command (grep, sed or awk), so no
// state it could leave behind reaches the next row. RegisterToolCommands
// captures AwkPerRecordVars once, so it reads the current row's
// meta-variables through the struct.
type rowInterp struct {
	interp *bash.Interpreter
	meta   map[string]string
	in     strings.Reader
	out    bytes.Buffer
}

func newRowInterp(binder awkBinder) *rowInterp {
	ri := &rowInterp{interp: bash.NewInterpreter()}
	ri.interp.AwkMaxSteps = 0
	ri.interp.SedMaxSteps = 0
	ri.interp.AwkPerRecordVars = func(string) map[string]string { return binder.vars(ri.meta) }
	ri.interp.SetStdout(&ri.out)
	ri.interp.SetStderr(io.Discard)
	bash.RegisterToolCommands(ri.interp)
	return ri
}

// lineCount reads head/tail's `-n N` or `-N`; no argument is 10.
func lineCount(args []string) (int, error) {
	switch {
	case len(args) == 0:
		return 10, nil
	case len(args) == 2 && args[0] == "-n":
		return strconv.Atoi(args[1])
	case len(args) == 1 && strings.HasPrefix(args[0], "-n"):
		return strconv.Atoi(args[0][2:])
	case len(args) == 1 && strings.HasPrefix(args[0], "-"):
		return strconv.Atoi(args[0][1:])
	}
	return 0, fmt.Errorf("unsupported line count %q", args)
}

// ReadsMeta reports whether any stage names a meta-variable. A caller building
// a row's meta-variables for Keep needs to only if it does.
func (rs *RowStages) ReadsMeta() bool {
	for _, st := range rs.stages {
		if st.meta.Reads() {
			return true
		}
	}
	return false
}

// Keep reports whether one row survives every filter, ignoring head and
// tail. The live path calls it per row, where there's no stream to
// count.
func (rs *RowStages) Keep(raw, payload string, meta map[string]string) bool {
	for _, st := range rs.stages {
		if st.head < 0 && st.tail < 0 && !st.keep(raw, payload, meta) {
			return false
		}
	}
	return true
}

func (st *rowStage) keep(raw, payload string, meta map[string]string) bool {
	if st.jq != nil {
		if payload == "" {
			payload = "{}"
		}
		v, err := jq.ParseValue(payload)
		if err != nil {
			return false
		}
		var opts jq.Options
		if st.meta.Reads() {
			opts.Vars = st.env.Vars(meta)
		}
		out, err := jq.EvalAllWithOptions(st.jq, v, opts)
		return err == nil && len(out) > 0
	}
	ri := st.interp.Get().(*rowInterp)
	defer st.interp.Put(ri)
	ri.meta = meta
	ri.out.Reset()
	ri.in.Reset(raw + "\n")
	ri.interp.SetStdin(&ri.in)
	ri.interp.ExecScript(st.script)
	return ri.out.Len() > 0
}

// rowRunner streams merged rows through the row stages into sink.
type rowRunner struct {
	stages []rowStage
	seen   []int             // per head stage: rows passed so far
	buf    [][]*pb.ResultRow // per tail stage: the last N rows
	sink   func(*pb.ResultRow) bool
}

func (rs *RowStages) runner(sink func(*pb.ResultRow) bool) *rowRunner {
	return &rowRunner{
		stages: rs.stages,
		seen:   make([]int, len(rs.stages)),
		buf:    make([][]*pb.ResultRow, len(rs.stages)),
		sink:   sink,
	}
}

// push runs one row from stage i on; false means the sink wants no more.
func (r *rowRunner) push(i int, row *pb.ResultRow) bool {
	for ; i < len(r.stages); i++ {
		st := &r.stages[i]
		switch {
		case st.head >= 0:
			if r.seen[i] >= st.head {
				return true
			}
			r.seen[i]++
		case st.tail >= 0:
			if st.tail == 0 {
				return true
			}
			if len(r.buf[i]) == st.tail {
				r.buf[i] = r.buf[i][1:]
			}
			r.buf[i] = append(r.buf[i], row)
			return true
		default:
			var meta map[string]string
			if st.meta.Reads() {
				meta = rowMetaVars(row, st.meta)
			}
			if !st.keep(row.Raw, row.MetaVars[planner.PayloadColumn], meta) {
				return true
			}
		}
	}
	return r.sink(row)
}

// flush releases what tail stages hold, in stage order, once the input
// has ended.
func (r *rowRunner) flush() {
	for i, st := range r.stages {
		if st.tail < 0 {
			continue
		}
		rows := r.buf[i]
		r.buf[i] = nil
		for _, row := range rows {
			if !r.push(i+1, row) {
				return
			}
		}
	}
}
