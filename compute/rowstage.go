package compute

import (
	"bytes"
	"fmt"
	"io"
	"strconv"
	"strings"

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
	jq     jq.Expr      // a select chain, over the payload
	meta   bool         // the select chain names a meta-variable
	script *bash.Script // a filter over the raw line
	head   int          // head -n N: keep the first N (when > -1)
	tail   int          // tail -n N: keep the last N (when > -1)
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
				st.meta = planner.StageReadsMetaVars(src)
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
	return st, nil
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
		if st.meta {
			payload = MergeMetaVars(payload, meta)
		}
		v, err := jq.ParseValue(payload)
		if err != nil {
			return false
		}
		out, err := jq.EvalAll(st.jq, v)
		return err == nil && len(out) > 0
	}
	interp := bash.NewInterpreter()
	interp.AwkMaxSteps = 0
	interp.SedMaxSteps = 0
	interp.AwkPerRecordVars = func(string) map[string]string { return awkVarsFromMeta(meta) }
	interp.SetStdin(strings.NewReader(raw + "\n"))
	var out bytes.Buffer
	interp.SetStdout(&out)
	interp.SetStderr(io.Discard)
	bash.RegisterToolCommands(interp)
	interp.ExecScript(st.script)
	return out.Len() > 0
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
			if !st.keep(row.Raw, row.MetaVars[planner.PayloadColumn], rowMetaVars(row)) {
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
