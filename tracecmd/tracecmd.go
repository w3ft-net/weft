// Package tracecmd is the grammar of the `trace` command, shared by every
// layer that has to agree on it: the planner (which runs it), the shell and
// spanview (which pick a view for it) and the browser.
//
//	trace [<ns>[/<svc>]]                      recent traces
//	trace --id <id> [<ns>[/<svc>]]            one request's log lines
//	trace --spans [<ns>[/<svc>]]              operations across requests
//	trace --spans --id <id> [<ns>[/<svc>]]    one request's breakdown
//	trace --spans --name <span> [--samples]   one operation
package tracecmd

import (
	"errors"
	"fmt"
	"strings"
)

// Mode says which of the five shapes a command is.
type Mode int

const (
	ModeList      Mode = iota // recent traces
	ModeLines                 // one request's log lines
	ModeBreakdown             // one request's spans, with self time
	ModeSpans                 // operations across requests
	ModeSamples               // one operation's slowest occurrences
)

// Cmd is a parsed `trace` command.
type Cmd struct {
	ID        string // --id: the trace to show
	Spans     bool   // --spans: the operations view
	Name      string // --name: one span name
	Samples   bool   // --samples: the slowest occurrences of Name
	Namespace string // positional scope
	Service   string // positional scope, after the slash
}

// Parse reads the arguments after the word `trace`.
func Parse(args []string) (*Cmd, error) {
	c := &Cmd{}
	var haveID, haveName, haveScope bool
	value := func(i int, flag, what string) (string, error) {
		if i+1 >= len(args) || args[i+1] == "" || strings.HasPrefix(args[i+1], "-") {
			return "", fmt.Errorf("trace: %s needs %s", flag, what)
		}
		return args[i+1], nil
	}
	for i := 0; i < len(args); i++ {
		switch a := args[i]; {
		case a == "--id":
			v, err := value(i, "--id", "a trace id")
			if err != nil {
				return nil, err
			}
			if haveID {
				return nil, errors.New("trace: --id given more than once")
			}
			c.ID, haveID = v, true
			i++
		case a == "--name":
			v, err := value(i, "--name", "a span name")
			if err != nil {
				return nil, err
			}
			if haveName {
				return nil, errors.New("trace: --name given more than once")
			}
			c.Name, haveName = v, true
			i++
		case a == "--spans":
			c.Spans = true
		case a == "--samples":
			c.Samples = true
		case strings.HasPrefix(a, "-"):
			return nil, fmt.Errorf("trace: unknown option %s", a)
		default:
			if haveScope {
				return nil, errors.New("trace: expected at most one <namespace>[/<service>] argument")
			}
			ns, svc, err := parseScope(a)
			if err != nil {
				return nil, err
			}
			c.Namespace, c.Service, haveScope = ns, svc, true
		}
	}
	switch {
	case c.Name != "" && !c.Spans:
		return nil, errors.New("trace: --name needs --spans")
	case c.Samples && (!c.Spans || c.Name == ""):
		return nil, errors.New("trace: --samples needs --spans --name <span>")
	case c.Name != "" && c.ID != "":
		return nil, errors.New("trace: --name looks across requests, so it can't be combined with --id")
	}
	return c, nil
}

func parseScope(a string) (ns, svc string, err error) {
	if strings.HasPrefix(a, "/log/") || strings.HasPrefix(a, "/") && strings.Count(a, "/") > 1 {
		return "", "", errors.New("trace: a scope is <namespace>[/<service>], not a path")
	}
	if strings.ContainsAny(a, " \t|&;<>()$`'\"\\*?[]{}!#~") {
		return "", "", fmt.Errorf("trace: a scope is <namespace>[/<service>], got %q", a)
	}
	ns, svc, hasSlash := strings.Cut(a, "/")
	if ns == "" || (hasSlash && (svc == "" || strings.Contains(svc, "/"))) {
		return "", "", fmt.Errorf("trace: a scope is <namespace>[/<service>], got %q", a)
	}
	return ns, svc, nil
}

// Mode reports which shape the command is.
func (c *Cmd) Mode() Mode {
	switch {
	case c.Spans && c.ID != "":
		return ModeBreakdown
	case c.Spans && c.Samples:
		return ModeSamples
	case c.Spans:
		return ModeSpans
	case c.ID != "":
		return ModeLines
	}
	return ModeList
}

// scope is the positional argument, or "".
func (c *Cmd) scope() string {
	if c.Service != "" {
		return c.Namespace + "/" + c.Service
	}
	return c.Namespace
}

// String is the command as a pipeline that parses back to the same Cmd.
func (c *Cmd) String() string {
	parts := []string{"trace"}
	if c.Spans {
		parts = append(parts, "--spans")
	}
	if c.ID != "" {
		parts = append(parts, "--id", quote(c.ID))
	}
	if c.Name != "" {
		parts = append(parts, "--name", quote(c.Name))
	}
	if c.Samples {
		parts = append(parts, "--samples")
	}
	if s := c.scope(); s != "" {
		parts = append(parts, s)
	}
	return strings.Join(parts, " ")
}

// Title is the short text a view shows for the command.
func (c *Cmd) Title() string {
	in := ""
	if s := c.scope(); s != "" {
		in = " in " + s
	}
	switch c.Mode() {
	case ModeList:
		return "recent traces" + in
	case ModeLines, ModeBreakdown:
		return c.ID
	}
	if c.Name != "" {
		return c.Name
	}
	return "all spans" + in
}

// quote single-quotes s when the shell would otherwise split or reinterpret it.
func quote(s string) string {
	if !strings.ContainsAny(s, " \t\"'\\$`|&;<>()*?[]{}#~!") {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// ParseLine parses a whole command line, as the client sees it before
// sending it. It reports ok=false for anything that is not exactly one
// `trace` command: another command, a pipe or redirect, or a grammar error.
func ParseLine(line string) (*Cmd, bool) {
	words, ok := splitWords(line)
	if !ok || len(words) == 0 || words[0] != "trace" {
		return nil, false
	}
	c, err := Parse(words[1:])
	if err != nil {
		return nil, false
	}
	return c, true
}

// splitWords splits a line the way the shell would for the cases String
// produces: whitespace separates words, single and double quotes group, and
// a backslash escapes the next character. A pipe or redirect outside quotes
// makes ok false.
func splitWords(line string) (words []string, ok bool) {
	var cur strings.Builder
	inWord := false
	var quote rune
	escaped := false
	for _, r := range line {
		switch {
		case escaped:
			cur.WriteRune(r)
			escaped = false
		case quote != 0:
			if r == quote {
				quote = 0
			} else {
				cur.WriteRune(r)
			}
		case r == '\\':
			escaped, inWord = true, true
		case r == '\'' || r == '"':
			quote, inWord = r, true
		case r == ' ' || r == '\t':
			if inWord {
				words = append(words, cur.String())
				cur.Reset()
				inWord = false
			}
		case r == '|' || r == '>' || r == '<' || r == '&' || r == ';':
			return nil, false
		default:
			cur.WriteRune(r)
			inWord = true
		}
	}
	if quote != 0 || escaped {
		return nil, false
	}
	if inWord {
		words = append(words, cur.String())
	}
	return words, true
}

// SamplesPipeline is the slowest-occurrences query for one row of an
// aggregate (`trace --spans ...`): the aggregate's own scope, narrowed to the
// row's service. A row is one span name in one service, whose label is
// "namespace/service" (empty when the aggregate wasn't grouped).
func SamplesPipeline(aggregate, name, label string) string {
	c, ok := ParseLine(aggregate)
	if !ok {
		c = &Cmd{}
	}
	c.Spans, c.ID, c.Name, c.Samples = true, "", name, true
	if label != "" {
		c.Namespace, c.Service, _ = strings.Cut(label, "/")
	}
	return c.String()
}

// IDPipeline is the command that shows one request's lines.
func IDPipeline(id string) string { return (&Cmd{ID: id}).String() }
