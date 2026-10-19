package api

import (
	"strings"

	"github.com/graybeardcorp/weft/auth"
	"github.com/graybeardcorp/weft/planner"
)

const explainUsage = "usage: explain <pipeline>\n  say how much of the pipeline is optimized and what would make more of it so\n"

// maybeHandleExplain answers `explain <pipeline>`: how much of a
// one-liner is optimized and what would make more of it so. It
// takes the line as typed, so a pipe belongs to the line explained and not
// to explain, as with a shell's `time`. It runs nothing and reads no log
// data, and it names no source to check access to, so it can't be used to
// probe what exists. Returns handled=false for any other line.
func (s *Server) maybeHandleExplain(session *auth.Session, line, cwd string) (handled bool, out string, err error) {
	rest, ok := strings.CutPrefix(strings.TrimSpace(line), "explain")
	if !ok || (rest != "" && rest[0] != ' ' && rest[0] != '\t') {
		return false, "", nil
	}
	rest = strings.TrimSpace(rest)
	if rest == "" {
		return true, explainUsage, nil
	}
	a, err := planner.Analyze(rest, cwd, storeBinResolver{store: s.store, orgID: session.ActiveOrgID})
	if err != nil {
		return true, "", err
	}
	return true, a.Render(true), nil
}
