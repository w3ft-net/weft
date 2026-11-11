package api

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	"github.com/graybeardcorp/weft/auth"
	"github.com/graybeardcorp/weft/planner"
	pb "github.com/graybeardcorp/weft/proto/weftpb"
)

// explainCountBudget bounds the line count behind explain (a var for the test), which is advice
// and must not turn into the slow query it is explaining.
var explainCountBudget = 5 * time.Second

const explainUsage = "usage: explain <pipeline>\n  say how much of the pipeline is optimized and what would make more of it so\n"

// maybeHandleExplain answers `explain <pipeline>`: how much of a
// one-liner is optimized and what would make more of it so. It
// takes the line as typed, so a pipe belongs to the line explained and not
// to explain, as with a shell's `time`. It never runs a stage that isn't
// optimized retrieval: the one query it makes counts the lines the optimized
// stages return (countRetrieved), as the caller's own query, so a source the
// caller can't read answers as it does for a run. Returns handled=false for
// any other line.
func (s *Server) maybeHandleExplain(r *http.Request, session *auth.Session, line, cwd string, dirStack []string, start, end time.Time) (handled bool, out string, err error) {
	rest, ok := strings.CutPrefix(strings.TrimSpace(line), "explain")
	if !ok || (rest != "" && rest[0] != ' ' && rest[0] != '\t') {
		return false, "", nil
	}
	rest = strings.TrimSpace(rest)
	if rest == "" {
		return true, explainUsage, nil
	}
	if first := strings.Fields(rest)[0]; strings.HasPrefix(first, "-") {
		return true, "", fmt.Errorf("unknown option %s", first)
	}
	a, err := planner.Analyze(rest, cwd, storeBinResolver{store: s.store, orgID: session.Org()})
	if err != nil {
		return true, "", err
	}
	if a.RetrievalCount != "" && s.coordClient != nil {
		if rows, err := s.countRetrieved(r, a.RetrievalCount, dirStack, start, end); err != nil {
			a.RowsNote = err.Error()
		} else {
			a.Retrieved(rows)
		}
	}
	return true, a.Render(true), nil
}

// deadlineGrace is how long endedByDeadline waits for the deadline to pass.
const deadlineGrace = 100 * time.Millisecond

// endedByDeadline is whether err, which ended a stream, was the deadline. The
// coordinator enforces the deadline it was sent as well as we do, and the two
// timers race for it, so its side can end the stream a moment before our context
// has expired, as a cancellation or an expired deadline. Anything else that
// ended the stream (a refusal, an unavailable coordinator) is not the deadline.
// For those two codes the deadline is the usual cause, so it is waited for, for
// at most deadlineGrace: if it has not passed by then, this was a cancellation
// for some other reason, and it keeps its own message.
func endedByDeadline(ctx context.Context, err error) bool {
	switch status.Code(err) {
	case codes.DeadlineExceeded, codes.Canceled:
	default:
		return false
	}
	grace := time.NewTimer(deadlineGrace)
	defer grace.Stop()
	select {
	case <-ctx.Done():
		return true
	case <-grace.C:
		return false
	}
}

// countRetrieved runs a pipeline that prints one number, through the
// coordinator as the caller would, within explainCountBudget. The error's
// text is for the person reading explain: the coordinator's own message,
// without the rpc envelope.
func (s *Server) countRetrieved(r *http.Request, pipeline string, dirStack []string, start, end time.Time) (int64, error) {
	ctx, cancel := context.WithTimeout(s.coordCtx(r), explainCountBudget)
	defer cancel()
	for _, d := range dirStack {
		ctx = metadata.AppendToOutgoingContext(ctx, "weft-dirstack", d)
	}
	stream, err := s.coordClient.Execute(ctx, &pb.CoordinatorExecuteRequest{
		Pipeline:    pipeline,
		TimeStartNs: start.UnixNano(),
		TimeEndNs:   end.UnixNano(),
	})
	var lines []string
	for err == nil {
		var resp *pb.CoordinatorExecuteResponse
		if resp, err = stream.Recv(); err == nil {
			for _, l := range resp.GetLines() {
				lines = append(lines, l.GetRaw())
			}
		}
	}
	if errors.Is(err, io.EOF) {
		err = nil
	}
	switch {
	case ctx.Err() != nil, err != nil && endedByDeadline(ctx, err):
		return 0, fmt.Errorf("took longer than %s", explainCountBudget)
	case err != nil:
		msg := err.Error()
		if st, ok := status.FromError(err); ok {
			msg = st.Message()
		}
		return 0, errors.New(msg)
	case len(lines) != 1:
		return 0, errors.New("no count came back")
	}
	n, perr := strconv.ParseInt(strings.TrimSpace(lines[0]), 10, 64)
	if perr != nil {
		return 0, errors.New("no count came back")
	}
	return n, nil
}
