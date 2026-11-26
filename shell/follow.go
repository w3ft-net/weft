package shell

import "strings"

// isFollowPipeline reports whether pipeline is a `tail -f` stream, which runs
// until cancelled rather than finishing.
func isFollowPipeline(pipeline string) bool {
	parts := strings.Fields(pipeline)
	if len(parts) < 2 || parts[0] != "tail" {
		return false
	}
	for _, p := range parts {
		if p == "-f" {
			return true
		}
	}
	return false
}

// splitTerminalStage strips a trailing pager (`... | less`) from a pipeline,
// returning the head to ship and the pager's name ("" when there is none).
func splitTerminalStage(pipeline string) (head, term string) {
	idx := strings.LastIndex(pipeline, "|")
	if idx == -1 {
		return pipeline, ""
	}
	fields := strings.Fields(pipeline[idx+1:])
	if len(fields) == 0 || fields[0] != "less" {
		return pipeline, ""
	}
	return strings.TrimSpace(pipeline[:idx]), fields[0]
}
