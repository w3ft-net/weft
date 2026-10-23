package coordinator

import (
	"bufio"
	"fmt"
	"strconv"
	"strings"

	"github.com/graybeardcorp/warp/pkg/shellcore"
	"github.com/graybeardcorp/weft/store"
	"github.com/tidwall/gjson"
)

// cronSchema is how a cron's stdout becomes stored rows: the value and tag
// fields from alert -v/-t or the `# value:`/`# tag:` headers, and whether
// the cron alerts. A zero value field means the default text rule.
type cronSchema struct {
	value, tag shellcore.FieldSpec
	alerts     bool
}

func schemaForWatch(w store.CronWatch) (cronSchema, error) {
	s := cronSchema{alerts: w.HasAlert}
	var err error
	if w.ValueField != nil {
		if s.value, err = shellcore.ParseFieldSpec(*w.ValueField); err != nil {
			return s, fmt.Errorf("value_field %q: %w", *w.ValueField, err)
		}
	}
	if w.TagField != nil {
		if s.tag, err = shellcore.ParseFieldSpec(*w.TagField); err != nil {
			return s, fmt.Errorf("tag_field %q: %w", *w.TagField, err)
		}
	}
	return s, nil
}

// declaredBy names where the schema came from, for strike messages.
func (s cronSchema) declaredBy() string {
	if s.alerts {
		return "alert -v " + s.value.String()
	}
	return "# value: " + s.value.String()
}

// cronPoint is one stored point of a run: the line it came from, its value
// and its tag ("" for untagged).
type cronPoint struct {
	line  string
	value float64
	tag   string
}

// parseCronOutput splits one run's stdout into points under the output
// contract. A non-nil error is a strike: the output breaks the contract and
// the run stores none of it. Empty output is legal and yields no points.
func parseCronOutput(s cronSchema, stdout string) ([]cronPoint, error) {
	var lines []string
	scanner := bufio.NewScanner(strings.NewReader(stdout))
	scanner.Buffer(make([]byte, 64*1024), 1024*1024)
	for scanner.Scan() {
		if line := strings.TrimSpace(scanner.Text()); line != "" {
			lines = append(lines, line)
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("reading output: %w", err)
	}

	points := make([]cronPoint, 0, len(lines))
	for i, line := range lines {
		n := i + 1
		var p cronPoint
		var err error
		switch {
		case s.value.IsJSON():
			p, err = jsonPoint(s, n, line)
		case !s.value.IsZero():
			p, err = positionalPoint(s, n, line)
		case s.alerts:
			if len(lines) > 1 {
				return nil, fmt.Errorf("output has %d lines; an alert without -v reads one value, so give alert -v 1 -t 2 for one line per tag", len(lines))
			}
			p, err = flaglessAlertPoint(n, line)
		default:
			p, err = textPoint(n, line)
		}
		if err != nil {
			return nil, err
		}
		points = append(points, p)
	}
	return points, nil
}

func isJSONObject(line string) bool {
	return strings.HasPrefix(line, "{") && gjson.Valid(line)
}

func parseValue(n int, v string) (float64, error) {
	f, err := strconv.ParseFloat(v, 64)
	if err != nil {
		return 0, fmt.Errorf("line %d: value %q is not a number", n, v)
	}
	return f, nil
}

// textPoint applies the default rule: `<value> [<tag>]`.
func textPoint(n int, line string) (cronPoint, error) {
	if isJSONObject(line) {
		return cronPoint{}, fmt.Errorf("output is JSON; add # value: (and # tag:) headers, or alert -v")
	}
	fields := strings.Fields(line)
	if len(fields) > 2 {
		return cronPoint{}, fmt.Errorf("line %d has %d fields; text output is <value> [<tag>]", n, len(fields))
	}
	v, err := parseValue(n, fields[0])
	if err != nil {
		return cronPoint{}, err
	}
	p := cronPoint{line: line, value: v}
	if len(fields) == 2 {
		p.tag = fields[1]
	}
	return p, nil
}

// flaglessAlertPoint reads the one value an alert without -v evaluates.
func flaglessAlertPoint(n int, line string) (cronPoint, error) {
	if isJSONObject(line) {
		return cronPoint{}, fmt.Errorf("output is JSON; give alert -v <path>")
	}
	if fields := strings.Fields(line); len(fields) > 1 {
		if len(fields) == 2 {
			return cronPoint{}, fmt.Errorf("line %d has a tag; give alert -v 1 -t 2", n)
		}
		return cronPoint{}, fmt.Errorf("line %d has %d fields; an alert without -v reads one number, so give alert -v <field>", n, len(fields))
	}
	v, err := parseValue(n, line)
	if err != nil {
		return cronPoint{}, err
	}
	return cronPoint{line: line, value: v}, nil
}

// positionalPoint reads alert -v N [-t M] columns. The two-field limit of
// the default rule doesn't apply: the flags say which columns matter.
func positionalPoint(s cronSchema, n int, line string) (cronPoint, error) {
	raw, _ := shellcore.ResolveField(s.value, line)
	if raw == "" {
		return cronPoint{}, fmt.Errorf("line %d has no field %s for alert -v", n, s.value)
	}
	v, err := parseValue(n, raw)
	if err != nil {
		return cronPoint{}, err
	}
	p := cronPoint{line: line, value: v}
	if !s.tag.IsZero() {
		p.tag, _ = shellcore.ResolveField(s.tag, line)
	}
	return p, nil
}

// jsonPoint reads one JSON row by the declared paths. A missing or null tag
// leaves the row untagged; a missing value is a strike.
func jsonPoint(s cronSchema, n int, line string) (cronPoint, error) {
	if !isJSONObject(line) {
		return cronPoint{}, fmt.Errorf("line %d is not JSON, but the cron declares %s", n, s.declaredBy())
	}
	val := gjson.Get(line, s.value.Path)
	if !val.Exists() || val.Type == gjson.Null {
		return cronPoint{}, fmt.Errorf("line %d has no %s", n, s.value)
	}
	if val.Type != gjson.Number {
		return cronPoint{}, fmt.Errorf("line %d: value %q is not a number", n, val.String())
	}
	p := cronPoint{line: line, value: val.Float()}
	if !s.tag.IsZero() {
		if t := gjson.Get(line, s.tag.Path); t.Exists() && t.Type != gjson.Null {
			p.tag = t.String()
		}
	}
	return p, nil
}

// checkTagCap is check 5: the number of distinct tags one run may print.
func checkTagCap(points []cronPoint, limit int) error {
	tags := map[string]struct{}{}
	for _, p := range points {
		if p.tag != "" {
			tags[p.tag] = struct{}{}
		}
	}
	if len(tags) > limit {
		return fmt.Errorf("run printed %d tags; your plan allows %d per run", len(tags), limit)
	}
	return nil
}
