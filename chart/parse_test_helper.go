package chart

// parsePipeArgs is a test-only helper that wraps ParsePipeArgs with
// a typed return — the public ParsePipeArgs returns `any` to match
// shellcore's PipeStageParseFunc signature, but tests want the
// concrete PipeArgs type for direct field access.
func parsePipeArgs(args []string) (PipeArgs, string) {
	parsed, errMsg := ParsePipeArgs(args)
	if errMsg != "" {
		return PipeArgs{}, errMsg
	}
	return parsed.(PipeArgs), ""
}
