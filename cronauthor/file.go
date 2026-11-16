package cronauthor

import (
	"fmt"
	"strings"
)

// Serialize is a cron as the shell shows it under /cron.d: a `# schedule:`
// header, `# enabled: false` when paused, then the pipeline.
func Serialize(schedule, pipeline string, enabled bool) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# schedule: %s\n", schedule)
	if !enabled {
		b.WriteString("# enabled: false\n")
	}
	b.WriteString(pipeline)
	if !strings.HasSuffix(pipeline, "\n") {
		b.WriteByte('\n')
	}
	return b.String()
}

// Parse splits a /cron.d file into its parts. Enabled is true unless the file
// says `# enabled: false`: dropping the comment is how a user turns a benched or
// paused cron back on. The `# value:`/`# tag:`/`# type:` headers stay in the
// pipeline, where Compile reads them.
func Parse(content string) (schedule, pipeline string, enabled bool, err error) {
	enabled = true
	var pipelineLines []string
	hasCommand := false
	for _, line := range strings.Split(content, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "# value:") || strings.HasPrefix(trimmed, "# tag:") || strings.HasPrefix(trimmed, "# type:") {
			pipelineLines = append(pipelineLines, trimmed)
		} else if strings.HasPrefix(trimmed, "# schedule:") {
			schedule = strings.TrimSpace(strings.TrimPrefix(trimmed, "# schedule:"))
		} else if strings.HasPrefix(trimmed, "# enabled:") {
			switch v := strings.TrimSpace(strings.TrimPrefix(trimmed, "# enabled:")); v {
			case "true":
			case "false":
				enabled = false
			default:
				return "", "", false, fmt.Errorf("'# enabled:' must be true or false, got %q", v)
			}
		} else if trimmed != "" && !strings.HasPrefix(trimmed, "#") {
			pipelineLines = append(pipelineLines, line)
			hasCommand = true
		}
	}
	pipeline = strings.Join(pipelineLines, "\n")
	if schedule == "" {
		err = fmt.Errorf("missing '# schedule:' directive")
	}
	if !hasCommand {
		err = fmt.Errorf("missing pipeline")
	}
	return
}
