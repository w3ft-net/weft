package coordinator

import (
	"github.com/w3ft-net/weft/planner"
	"github.com/w3ft-net/weft/shape"
)

// ExplainSummary is what `explain` says about pipeline, as plain text for an
// error message. It reads no logs: the analysis is the planner's, over the
// pipeline's own text, with the org's saved scripts inlined as a run does. It
// is empty when the pipeline can't be analyzed (not a log query, a parse
// error), since an error message has nothing useful to say then.
func (c *Coordinator) ExplainSummary(pipeline, cwd string, owner shape.Added) string {
	a, err := planner.Analyze(pipeline, cwd, watchBinResolver{bins: c.Bins, owner: owner})
	if err != nil {
		return ""
	}
	return a.Render(false)
}
