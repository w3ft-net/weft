package coordinator

import (
	"github.com/graybeardcorp/weft/planner"
	"github.com/graybeardcorp/weft/store"
)

// ExplainSummary is what `explain` says about pipeline, as plain text for an
// error message. It reads no logs: the analysis is the planner's, over the
// pipeline's own text, with the org's saved scripts inlined as a run does. It
// is empty when the pipeline can't be analyzed (not a log query, a parse
// error), since an error message has nothing useful to say then.
func (c *Coordinator) ExplainSummary(pipeline, cwd string, orgID store.OrgID) string {
	a, err := planner.Analyze(pipeline, cwd, watchBinResolver{store: c.Store, orgID: orgID})
	if err != nil {
		return ""
	}
	return a.Render(false)
}
