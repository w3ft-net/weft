package ingest

import (
	"context"
	"encoding/json"
	"fmt"
	weftsdk "github.com/w3ft-net/weft-go"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/bedrockruntime"
	brdoc "github.com/aws/aws-sdk-go-v2/service/bedrockruntime/document"
	brtypes "github.com/aws/aws-sdk-go-v2/service/bedrockruntime/types"
)

// ClassifierResult is one classification pass over a sample: the LLM's
// best guess for each open-vocabulary field. A nil member means "not
// present in this sample" (already validated against the real sample —
// see validateClassification — not a raw, untrusted model claim).
type ClassifierResult struct {
	Timestamp    *TimestampCandidate
	Level        *FieldCandidate
	TraceID      *FieldCandidate
	SpanName     *FieldCandidate
	SpanID       *FieldCandidate
	ParentSpanID *FieldCandidate
	DurationNs   *FieldCandidate
}

// Classifier classifies a sample of log lines into timestamp/level/trace/
// span candidates. det carries the sample plus the cheap, deterministic
// envelope/JSON detection already run on it (format, stripped lines) so
// the classifier never has to re-derive what Detect already knows for
// certain — see the "keep exact checks deterministic" scope decision in
// docs/plans (schema-configuration LLM classification plan).
type Classifier interface {
	Classify(ctx context.Context, det *Detection) (*ClassifierResult, error)
}

// BedrockClassifier classifies via Amazon Bedrock's Converse API with tool
// use, forcing structured JSON output instead of free-text parsing.
type BedrockClassifier struct {
	client  *bedrockruntime.Client
	modelID string
}

// NewBedrockClassifier builds a classifier against modelID (a Bedrock
// model ID or cross-region inference profile ID/ARN — e.g. Amazon Nova
// Lite) using cfg's credentials/region.
func NewBedrockClassifier(cfg aws.Config, modelID string) *BedrockClassifier {
	return &BedrockClassifier{
		client:  bedrockruntime.NewFromConfig(cfg),
		modelID: modelID,
	}
}

// NewBedrockClassifierFromEnv builds a BedrockClassifier from
// BEDROCK_CLASSIFIER_MODEL_ID and BEDROCK_REGION, shared by both
// cmd/api and cmd/coordinator's main.go (each constructs one Server that
// needs a Classifier) so the env-var convention and AWS config loading
// live in one place instead of two copies in unrelated main packages.
//
// Deliberately separate from AWS_REGION (used elsewhere for SSM/S3/EC2):
// Bedrock model availability is region-specific and Nova Lite may not be
// offered wherever the coordinator otherwise runs — see the infra
// prerequisite note in docs/plans (schema-configuration LLM classification
// plan). Returns (nil, nil) — not an error — when
// BEDROCK_CLASSIFIER_MODEL_ID is unset, so classification is simply
// disabled (ClassifyAsync already treats a nil Classifier as "skip") until
// an operator explicitly configures it, rather than the server failing to
// start.
func NewBedrockClassifierFromEnv(ctx context.Context) (Classifier, error) {
	modelID := os.Getenv("BEDROCK_CLASSIFIER_MODEL_ID")
	if modelID == "" {
		return nil, nil
	}
	region := os.Getenv("BEDROCK_REGION")
	if region == "" {
		return nil, fmt.Errorf("BEDROCK_CLASSIFIER_MODEL_ID is set but BEDROCK_REGION is not")
	}
	cfg, err := awsconfig.LoadDefaultConfig(ctx, awsconfig.WithRegion(region))
	if err != nil {
		return nil, fmt.Errorf("load AWS config for bedrock classifier: %w", err)
	}
	return NewBedrockClassifier(cfg, modelID), nil
}

const classifyToolName = "classify_log_fields"

// Classify sends the sample (already envelope-stripped by det) to the
// model and returns validated candidates. Every candidate returned here
// has already been checked against the real sample lines
// (validateClassification) — a model claim that doesn't hold up is
// dropped, never passed through as if confirmed.
func (c *BedrockClassifier) Classify(ctx context.Context, det *Detection) (*ClassifierResult, error) {
	out, lines, err := c.classifyRaw(ctx, det)
	if err != nil {
		return nil, err
	}
	if out == nil {
		return &ClassifierResult{}, nil
	}
	return validateClassification(ctx, out, lines), nil
}

// classifyRaw is the model call alone: its unverified proposals and the
// lines they were made against. Split from Classify so tests can see what the
// model said before verification decided what to keep.
func (c *BedrockClassifier) classifyRaw(ctx context.Context, det *Detection) (*classifyToolResponse, []string, error) {
	lines := det.Lines
	if len(lines) > 30 {
		lines = lines[:30]
	}
	if len(lines) == 0 {
		return nil, nil, nil
	}

	resp, err := c.client.Converse(ctx, classifyConverseInput(c.modelID, det, lines))
	if err != nil {
		return nil, nil, fmt.Errorf("bedrock converse: %w", err)
	}

	raw, err := extractToolUseInput(resp)
	if err != nil {
		return nil, nil, err
	}

	var out classifyToolResponse
	if err := decodeToolInput(raw, &out); err != nil {
		return nil, nil, fmt.Errorf("decode classifier tool response: %w", err)
	}

	return &out, lines, nil
}

// extractToolUseInput pulls the tool_use content block's Input document
// out of a Converse response. ToolChoiceMemberAny forces the model to use
// a tool, so a well-behaved model always returns exactly this shape; if
// it doesn't, that's a hard error rather than something worth guessing at.
func extractToolUseInput(resp *bedrockruntime.ConverseOutput) (brdoc.Interface, error) {
	msg, ok := resp.Output.(*brtypes.ConverseOutputMemberMessage)
	if !ok {
		return nil, fmt.Errorf("unexpected converse output type %T", resp.Output)
	}
	for _, block := range msg.Value.Content {
		if tu, ok := block.(*brtypes.ContentBlockMemberToolUse); ok {
			return tu.Value.Input, nil
		}
	}
	return nil, fmt.Errorf("model response had no tool_use block (stop reason %s)", resp.StopReason)
}

// decodeToolInput decodes a tool-use input document into out through its JSON
// form. The document's own UnmarshalSmithyDocument is not used: it ignores
// `json` struct tags, so a multi-word key like field_index never reached its
// field and every proposal came back with nothing to locate it by.
func decodeToolInput(raw interface{ MarshalSmithyDocument() ([]byte, error) }, out any) error {
	b, err := raw.MarshalSmithyDocument()
	if err != nil {
		return err
	}
	return json.Unmarshal(b, out)
}

// classifyToolResponse mirrors classifyToolSpec's JSON schema.
type classifyToolResponse struct {
	Timestamp    *timestampField `json:"timestamp"`
	Level        *tagField       `json:"level"`
	TraceID      *tagField       `json:"trace_id"`
	SpanName     *tagField       `json:"span_name"`
	SpanID       *tagField       `json:"span_id"`
	ParentSpanID *tagField       `json:"parent_span_id"`
	DurationNs   *tagField       `json:"duration_ns"`
}

type timestampField struct {
	Present     bool   `json:"present"`
	FieldIndex  int    `json:"field_index"`
	SpanFields  int    `json:"span_fields"`
	JSONPath    string `json:"json_path"`
	Strftime    string `json:"strftime"`
	SampleValue string `json:"sample_value"`
}

type tagField struct {
	Present        bool   `json:"present"`
	FieldIndex     int    `json:"field_index"`
	JSONPath       string `json:"json_path"`
	ExtractPattern string `json:"extract_pattern"`
	SampleValue    string `json:"sample_value"`
	// DurationUnit, for a duration only: see Schema.DurationUnit.
	DurationUnit string `json:"duration_unit"`
}

// tagFieldRoles lists every ClassifierResult field sharing tagField's
// shape, paired with the classifyToolResponse field carrying the model's
// proposal for it. Iterating this instead of writing out level/trace/span
// x4 by hand keeps validateClassification and buildClassifyPrompt from
// having six near-identical blocks.
type tagFieldRole struct {
	name string // matches the tool schema's JSON key, for logging
	src  *tagField
	dst  **FieldCandidate
}

func (r *ClassifierResult) tagRoles(t *classifyToolResponse) []tagFieldRole {
	return []tagFieldRole{
		{"level", t.Level, &r.Level},
		{"trace_id", t.TraceID, &r.TraceID},
		{"span_name", t.SpanName, &r.SpanName},
		{"span_id", t.SpanID, &r.SpanID},
		{"parent_span_id", t.ParentSpanID, &r.ParentSpanID},
		{"duration_ns", t.DurationNs, &r.DurationNs},
	}
}

// validateClassification re-checks every candidate the model proposed
// against the real sample lines before trusting it — a model claim about
// a field/pattern is worthless until it's shown to actually extract
// something on the data it was looking at. Anything that doesn't verify
// is silently dropped (present: false), exactly like every heuristic
// candidate in detect.go already behaves for a non-matching line.
func validateClassification(ctx context.Context, t *classifyToolResponse, lines []string) *ClassifierResult {
	result := &ClassifierResult{}

	if t.Timestamp != nil && t.Timestamp.Present {
		if c := verifyTimestampProposal(ctx, t.Timestamp, lines); c != nil {
			result.Timestamp = c
		}
	}

	for _, role := range result.tagRoles(t) {
		if role.src == nil || !role.src.Present {
			continue
		}
		if c := verifyTagProposal(role.src, ProbeField(role.name), lines); c != nil {
			*role.dst = c
		} else {
			weftsdk.Infof(ctx, "classify: dropping unverified %s candidate: %+v", role.name, role.src)
		}
	}

	return result
}

// minVerifyHits/minVerifyRatio: a candidate must resolve on at least this
// many lines, and (for timestamp and level; see tagMinRatio) on this fraction
// of the lines it was even eligible to match (positional: line has enough fields; JSON path: line parses as
// JSON; whole-line search: always eligible). Mirrors the heuristic
// detectors' own thresholds (detect.go's >50% timestamp / >1/3 level).
const (
	minVerifyHits  = 2
	minVerifyRatio = 0.5
)

// verifyTimestampProposal checks a model's timestamp proposal by running it
// through the real parser (evalExtractor) over the sample, exactly as the
// probe checks a user-built rule: a proposal is only as good as what the
// parser extracts with it. Returns nil if it doesn't verify.
func verifyTimestampProposal(ctx context.Context, f *timestampField, lines []string) *TimestampCandidate {
	if f.Strftime == "" {
		return nil
	}
	layout, err := strftimeToGoLayout(f.Strftime)
	if err != nil {
		weftsdk.Infof(ctx, "classify: dropping timestamp candidate, bad strftime %q: %v", f.Strftime, err)
		return nil
	}

	ex := ProbeExtractor{TimestampFormat: layout}
	cand := &TimestampCandidate{Format: layout, Sample: f.SampleValue}
	switch {
	case f.JSONPath != "":
		ex.Field = f.JSONPath
		cand.JSONPath = f.JSONPath
	case f.FieldIndex >= 1:
		spans := max(f.SpanFields, 1)
		ex.Field, ex.TimestampFields = strconv.Itoa(f.FieldIndex), spans
		ex.Spec = derivedFrame(lines, f.FieldIndex, spans)
		cand.FieldIndex, cand.SpanFields = f.FieldIndex, spans
	default:
		return nil
	}
	if !proposalVerifies(ProbeTimestamp, lines, ex, minVerifyRatio) {
		return nil
	}
	return cand
}

// verifyTagProposal is verifyTimestampProposal for level, trace and span
// fields. A JSON-path proposal's pattern is dropped: the parser never
// applies a pattern to a JSON path, so keeping it would describe an
// extraction ingest doesn't perform.
func verifyTagProposal(f *tagField, field ProbeField, lines []string) *FieldCandidate {
	ex, ok := tagExtractor(lines, f.FieldIndex, f.JSONPath, f.ExtractPattern)
	if !ok {
		return nil
	}
	if field == ProbeDurationNs {
		if _, ok := durationUnitMode(f.DurationUnit); !ok {
			return nil
		}
		found := false
		for _, u := range unitVariants(f.DurationUnit) {
			ex.DurationUnit = u
			if proposalVerifies(field, lines, ex, tagMinRatio(string(field))) {
				found = true
				break
			}
		}
		if !found {
			return nil
		}
	} else if !proposalVerifies(field, lines, ex, tagMinRatio(string(field))) {
		return nil
	}
	// The candidate mirrors the extractor: a JSON-path proposal carries no
	// pattern (the parser never applies one to a JSON path).
	cand := &FieldCandidate{Sample: f.SampleValue, JSONPath: f.JSONPath, Pattern: ex.Pattern, Unit: ex.DurationUnit}
	if ex.Field != "none" && f.JSONPath == "" {
		cand.FieldIndex = f.FieldIndex
	}
	return cand
}

// tagExtractor turns a model's (field index | JSON path | whole-line pattern)
// proposal for a level/trace/span value into the extractor it describes, in
// the schema's own vocabulary. It is the one place that mapping lives, for
// detection-time proposals and for retry proposals alike. A JSON path drops
// any pattern: the parser never applies one to a JSON path, so keeping it
// would describe an extraction ingest does not perform.
func tagExtractor(lines []string, fieldIndex int, jsonPath, pattern string) (ProbeExtractor, bool) {
	switch {
	case jsonPath != "":
		return ProbeExtractor{Field: jsonPath}, true
	case fieldIndex != 0:
		return ProbeExtractor{Field: strconv.Itoa(fieldIndex), Pattern: pattern, Spec: derivedFrame(lines, fieldIndex, 1)}, true
	case pattern != "":
		// Whole-line search: no field to anchor on (schema.go's TraceIDPattern convention).
		return ProbeExtractor{Field: "none", Pattern: pattern}, true
	}
	return ProbeExtractor{}, false
}

// proposalVerifies applies ex with the real parser (strict about level
// patterns) and reports whether it extracts a value on enough of the lines.
func proposalVerifies(f ProbeField, lines []string, ex ProbeExtractor, minRatio float64) bool {
	vals, err := evalExtractor(f, ProbeBase{}, lines, ex, true)
	if err != nil {
		return false // bad regex or JSON path
	}
	var found []string
	for _, v := range vals {
		if v != "" {
			found = append(found, v)
		}
	}
	// A trace or span ID verifies on any hit count, so a model that cannot find
	// one may point at the timestamp, which every line has. A date or time is
	// never an identifier.
	if isIDField(f) && len(found) > 0 && (len(inferLayouts(found)) > 0 || allMatch(timeOfDayRe, found)) {
		return false
	}
	return verifies(len(found), len(lines), minRatio)
}

var timeOfDayRe = regexp.MustCompile(`^\d{1,2}:\d\d(:\d\d([.,]\d+)?)?$`)

func allMatch(re *regexp.Regexp, vals []string) bool {
	for _, v := range vals {
		if !re.MatchString(v) {
			return false
		}
	}
	return true
}

func isIDField(f ProbeField) bool {
	return f == ProbeTraceID || f == ProbeSpanID || f == ProbeParentSpanID
}

func verifies(hits, eligible int, minRatio float64) bool {
	return hits >= minVerifyHits && eligible > 0 && float64(hits)/float64(eligible) >= minRatio
}

// tagMinRatio is the fraction of eligible lines a candidate must resolve on.
// Every line has a level, so a level that shows up on a minority of lines is
// a coincidence. Trace and span fields are different: they appear only on the
// lines that belong to a request or span (health checks and startup chatter
// carry none), so a handful of hits is what a real one looks like, and the
// user reviews the result (with its hit count) before accepting it.
func tagMinRatio(role string) float64 {
	if role == "level" {
		return minVerifyRatio
	}
	return 0
}

// classifySystemPrompt explains the task once; buildClassifyPrompt adds
// the per-sample specifics (format, JSON examples, raw lines).
const classifySystemPrompt = `You classify a sample of application/server log lines to help configure a log parser. For each of seven fields (timestamp, level, trace_id, span_name, span_id, parent_span_id, duration_ns), decide whether it's present in the sample and, if so, how to extract it. A field need not be on every line: trace_id and the span fields typically appear only on request or span lines, not on health checks or startup output, so propose one whenever at least two lines carry it, even if most lines don't. The level, by contrast, is on essentially every line.

Two ways to locate a field:
  - Positional: field_index is the 1-based index of the whitespace-delimited field.
  - JSON path: json_path like ".context.trace_id" or ".tags[0].value" — use this whenever the line (or its JSON tail) is JSON and the value lives inside it, including nested objects and arrays. Leave field_index at 0 when using json_path.

For trace_id/span_name/span_id/parent_span_id/duration_ns, if the value isn't cleanly at one field index or JSON path but appears as a trailing "key=value" tag whose position varies line to line (e.g. "... key=abc123" at the end of a variable-length line), leave field_index at 0 and json_path empty, and give extract_pattern as a regex with exactly one capture group that finds it anywhere in the line (e.g. "key=(\\S+)"). extract_pattern may also be used to refine a specific field_index/json_path (e.g. a field holding "VALUE/other" where the pattern "^(\\w+)/" pulls out just "VALUE").

Every regex is Go (RE2) syntax with exactly ONE capturing group, around the value; any other grouping, alternation included, must be non-capturing, written (?:...).

What each value is, whatever the format of the log:
  - trace_id: a token that is the same on every line belonging to one request or operation, so those lines can be correlated. It may be a field of its own, a key=value tag, or a nested JSON field.
  - span fields: the name, id, parent id and duration of a unit of work, if the lines describe such units.
  - level: the severity of the message, in whatever words or numbers this log uses. The field may hold more than the level (the level, a delimiter, then unrelated text): then give extract_pattern, a regex with one capture group that captures only the level.

For duration_ns (stored as nanoseconds whatever it is written in), also give duration_unit: ns, us, ms or s when the extracted text is a bare number counted in that unit, or text when the text carries its own unit (5.01s, 340ms, 1m2s). If the same duration appears in more than one spelling on a line, prefer the most precise one.

For timestamp, give field_index or json_path the same way, plus a strftime pattern (e.g. "%Y-%m-%d %H:%M:%S,%f", "%Y-%m-%dT%H:%M:%S%z") describing the value's format. Use %f for a fractional-second field written as exactly 6 digits (microseconds); use %3f for exactly 3 digits (milliseconds, the common case for comma-separated fractional seconds like "10:15:03,102"); use %9f for 9 digits (nanoseconds). sample_value should be the literal value you found for every present field, for traceability.

Only claim present: true when you actually see the field in the sample — never guess. Respond only by calling the classify_log_fields tool.`

func classifyToolSpec() brtypes.ToolSpecification {
	return brtypes.ToolSpecification{
		Name:        aws.String(classifyToolName),
		Description: aws.String("Report the detected timestamp/level/trace/span field classifications for the log sample."),
		InputSchema: &brtypes.ToolInputSchemaMemberJson{
			Value: brdoc.NewLazyDocument(classifyJSONSchema),
		},
	}
}

var tagFieldSchema = map[string]any{
	"type": "object",
	"properties": map[string]any{
		"present":         map[string]any{"type": "boolean", "description": "true if this value appears on at least two lines of the sample"},
		"field_index":     map[string]any{"type": "integer", "description": "1-based index of the whitespace-delimited field that holds the value; 0 when the value is located by json_path or by extract_pattern across the whole line"},
		"json_path":       map[string]any{"type": "string", "description": "JSON path such as .a.b.c when the line (or its JSON tail) is JSON; empty otherwise"},
		"extract_pattern": map[string]any{"type": "string", "description": "Go (RE2) regex whose ONLY capturing group yields the value, e.g. key=(\\S+) (other groups must be non-capturing, (?:...)); empty if the whole field is the value"},
		"sample_value":    map[string]any{"type": "string", "description": "the actual value found on one sample line"},
	},
	"required": []string{"present", "field_index", "json_path", "extract_pattern", "sample_value"},
}

// durationFieldSchema is tagFieldSchema plus the unit the duration text is
// written in, the way the timestamp schema carries its strftime pattern.
var durationFieldSchema = func() map[string]any {
	props := map[string]any{}
	for k, v := range tagFieldSchema["properties"].(map[string]any) {
		props[k] = v
	}
	props["duration_unit"] = durationUnitSchema
	return map[string]any{
		"type":       "object",
		"properties": props,
		"required":   []string{"present", "field_index", "json_path", "extract_pattern", "sample_value", "duration_unit"},
	}
}()

var durationUnitSchema = map[string]any{
	"type":        "string",
	"enum":        []string{"ns", "us", "ms", "s", "text"},
	"description": "the unit of the text your pattern captures. Look at the captured text itself: if it is only digits (optionally with a decimal point), choose the unit those numbers are counted in: ns, us, ms or s (a bare 5015996546 is ns; a bare 12.5 next to a total of 340 is most likely ms). Choose text ONLY when the captured text itself ends in a unit, such as 5.01s, 340ms or 1m2s",
}

var classifyJSONSchema = map[string]any{
	"type": "object",
	"properties": map[string]any{
		"timestamp": map[string]any{
			"type": "object",
			"properties": map[string]any{
				"present":      map[string]any{"type": "boolean", "description": "true if the lines carry a timestamp"},
				"field_index":  map[string]any{"type": "integer", "description": "1-based index of the whitespace-delimited field where the timestamp starts; 0 when json_path is used"},
				"span_fields":  map[string]any{"type": "integer", "description": "how many whitespace-delimited fields the timestamp covers (2 for \"2026-10-15 12:00:01\")"},
				"json_path":    map[string]any{"type": "string", "description": "JSON path such as .ts when the line is JSON; empty otherwise"},
				"strftime":     map[string]any{"type": "string", "description": "strftime pattern of the timestamp, e.g. %Y-%m-%d %H:%M:%S,%3f"},
				"sample_value": map[string]any{"type": "string", "description": "the actual timestamp text from one line"},
			},
			"required": []string{"present", "field_index", "span_fields", "json_path", "strftime", "sample_value"},
		},
		"level":          tagFieldSchema,
		"trace_id":       tagFieldSchema,
		"span_name":      tagFieldSchema,
		"span_id":        tagFieldSchema,
		"parent_span_id": tagFieldSchema,
		"duration_ns":    durationFieldSchema,
	},
	"required": []string{"timestamp", "level", "trace_id", "span_name", "span_id", "parent_span_id", "duration_ns"},
}

// buildClassifyPrompt describes the sample: raw lines always, plus parsed
// example JSON objects when the sample is JSON-shaped (det's own
// deterministic format detection — never re-derived by the model).
func buildClassifyPrompt(det *Detection, lines []string) string {
	var b strings.Builder
	format := det.SuggestFormat()
	fmt.Fprintf(&b, "Sample format (already determined, not yours to re-derive): %s\n\n", format)

	if format == "json" || format == "prefix_json" {
		b.WriteString("Parsed example objects from the sample:\n")
		shown := 0
		for _, line := range lines {
			if shown >= 3 {
				break
			}
			body, ok := parseJSONBody(line)
			if !ok {
				continue
			}
			pretty, err := json.MarshalIndent(body, "", "  ")
			if err != nil {
				continue
			}
			b.Write(pretty)
			b.WriteString("\n\n")
			shown++
		}
	}

	if format != "json" {
		b.WriteString("Field numbering for the first lines (whitespace-delimited, 1-based; use these numbers for field_index and span_fields):\n")
		for i, line := range lines {
			if i == 3 {
				break
			}
			b.WriteString(fieldTable(line) + "\n")
		}
		b.WriteString("\n")
	}

	b.WriteString("Raw sample lines:\n")
	for _, line := range lines {
		if len(line) > 300 {
			line = line[:300] + "..."
		}
		b.WriteString(line)
		b.WriteString("\n")
	}
	return b.String()
}

// fieldTable numbers a line's whitespace-delimited fields ("1=a 2=b ..."), so
// the model reads indexes off the page instead of counting them.
func fieldTable(line string) string {
	var parts []string
	for i, f := range strings.Fields(line) {
		if len(f) > 40 {
			f = f[:37] + "..."
		}
		parts = append(parts, fmt.Sprintf("%d=%s", i+1, f))
	}
	return strings.Join(parts, " ")
}

// strftimeToGoLayout translates a (near-)POSIX strftime pattern into a Go
// time.Parse reference layout. Models are asked for strftime rather than
// Go's idiosyncratic reference-time syntax because strftime is the
// well-known, widely-understood convention — this is the deterministic,
// testable half of that choice. Unsupported directives (notably %j, day
// of year — Go's reference layout has no token for it) return an error
// rather than silently producing a layout that parses the wrong thing.
func strftimeToGoLayout(pattern string) (string, error) {
	var b strings.Builder
	for i := 0; i < len(pattern); i++ {
		c := pattern[i]
		if c != '%' {
			b.WriteByte(c)
			continue
		}
		if i == len(pattern)-1 {
			return "", fmt.Errorf("strftime %q: trailing '%%' with no directive", pattern)
		}
		i++
		switch pattern[i] {
		case 'Y':
			b.WriteString("2006")
		case 'y':
			b.WriteString("06")
		case 'm':
			b.WriteString("01")
		case 'd':
			b.WriteString("02")
		case 'e':
			b.WriteString("_2")
		case 'H':
			b.WriteString("15")
		case 'I':
			b.WriteString("03")
		case 'M':
			b.WriteString("04")
		case 'S':
			b.WriteString("05")
		case 'f':
			b.WriteString("000000")
		case '3':
			// non-standard but common shorthand for millisecond
			// precision; must be followed by 'f'.
			if i+1 >= len(pattern) || pattern[i+1] != 'f' {
				return "", fmt.Errorf("strftime %q: %%3 must be followed by f", pattern)
			}
			i++
			b.WriteString("000")
		case '6':
			if i+1 >= len(pattern) || pattern[i+1] != 'f' {
				return "", fmt.Errorf("strftime %q: %%6 must be followed by f", pattern)
			}
			i++
			b.WriteString("000000")
		case '9':
			if i+1 >= len(pattern) || pattern[i+1] != 'f' {
				return "", fmt.Errorf("strftime %q: %%9 must be followed by f", pattern)
			}
			i++
			b.WriteString("000000000")
		case 'z':
			b.WriteString("-0700")
		case 'Z':
			b.WriteString("MST")
		case 'p':
			b.WriteString("PM")
		case 'a':
			b.WriteString("Mon")
		case 'A':
			b.WriteString("Monday")
		case 'b':
			b.WriteString("Jan")
		case 'B':
			b.WriteString("January")
		case '%':
			b.WriteByte('%')
		default:
			return "", fmt.Errorf("strftime %q: unsupported directive %%%c", pattern, pattern[i])
		}
	}
	return b.String(), nil
}

// ClassifyAsync computes the cheap, deterministic part of Detection
// (envelope + JSON coverage — see Detect) synchronously and returns
// immediately. If final, it also starts the LLM classification pass in a
// background goroutine bounded by a timeout, calling onComplete with the
// fully-populated Detection once it finishes — success, or the heuristic
// text-mode fallback if the classifier errors out. Never blocks the
// caller: this is meant to be called from an ingest request/RPC handler,
// which must not wait on Bedrock's availability/latency. ctx supplies
// the trace ID only; the goroutine outlives the request and keeps its own
// 10s timeout.
func ClassifyAsync(ctx context.Context, classifier Classifier, lines []string, final bool, onComplete func(*Detection)) *Detection {
	det := Detect(lines)
	if !final || classifier == nil {
		return det
	}

	go func() {
		// The request ends before this finishes, so cancellation is
		// detached; the values (trace ID) are kept for the logs below.
		ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		defer cancel()

		result, err := classifier.Classify(ctx, det)
		if err != nil {
			weftsdk.Errorf(ctx, "classify: bedrock classification failed, falling back to heuristic text detection: %v", err)
			// Heuristic fallback covers timestamp/level only — the only
			// two fields the pattern-matching detector has ever known
			// how to find. Envelope/JSON detection is already in det;
			// running detectTimestamps/detectLevels again fills in what
			// the LLM would have replaced.
			det.detectTimestamps(det.Lines)
			det.detectCategorical(det.Lines)
			onComplete(det)
			return
		}

		applyClassification(det, result)
		onComplete(det)
	}()

	return det
}

func applyClassification(det *Detection, r *ClassifierResult) {
	if r.Timestamp != nil {
		det.TimestampCandidates = []TimestampCandidate{*r.Timestamp}
	}
	if r.Level != nil {
		det.LevelCandidates = []FieldCandidate{*r.Level}
	}
	if r.TraceID != nil {
		det.TraceIDCandidates = []FieldCandidate{*r.TraceID}
	}
	if r.SpanName != nil {
		det.SpanNameCandidates = []FieldCandidate{*r.SpanName}
	}
	if r.SpanID != nil {
		det.SpanIDCandidates = []FieldCandidate{*r.SpanID}
	}
	if r.ParentSpanID != nil {
		det.ParentSpanIDCandidates = []FieldCandidate{*r.ParentSpanID}
	}
	if r.DurationNs != nil {
		det.DurationNsCandidates = []FieldCandidate{*r.DurationNs}
	}
}

// --- retry proposals --------------------------------------------------------

// maxProposals caps how many candidates one retry call may return; each is
// verified in turn, and the first that fits the user's marks wins.
const maxProposals = 3

// proposedExtractor is one candidate in the model's reply: a union of the
// shapes tagField and timestampField use for detection.
type proposedExtractor struct {
	FieldIndex     int    `json:"field_index"`
	SpanFields     int    `json:"span_fields"`
	JSONPath       string `json:"json_path"`
	ExtractPattern string `json:"extract_pattern"`
	Strftime       string `json:"strftime"`
	DurationUnit   string `json:"duration_unit"`
}

type proposeToolResponse struct {
	Candidates []proposedExtractor `json:"candidates"`
}

// proposalsToExtractors converts the model's candidates into extractors for
// f, dropping any that are unusable (nothing to locate the value by, or an
// unsupported timestamp format) and capping the list. They are still
// unverified; Probe checks each against the user's marks.
func proposalsToExtractors(lines []string, resp *proposeToolResponse, f ProbeField) []ProbeExtractor {
	var out []ProbeExtractor
	for i, c := range resp.Candidates {
		if i == maxProposals {
			break
		}
		if f == ProbeTimestamp {
			layout, err := strftimeToGoLayout(c.Strftime)
			if err != nil || c.Strftime == "" {
				continue
			}
			switch {
			case c.JSONPath != "":
				out = append(out, ProbeExtractor{Field: c.JSONPath, TimestampFormat: layout})
			case c.FieldIndex >= 1:
				out = append(out, ProbeExtractor{Field: strconv.Itoa(c.FieldIndex), TimestampFormat: layout, TimestampFields: max(c.SpanFields, 1), Spec: derivedFrame(lines, c.FieldIndex, max(c.SpanFields, 1))})
			}
			continue
		}
		if ex, ok := tagExtractor(lines, c.FieldIndex, c.JSONPath, c.ExtractPattern); ok {
			if f == ProbeDurationNs {
				if _, ok := durationUnitMode(c.DurationUnit); !ok {
					continue
				}
				for _, u := range unitVariants(c.DurationUnit) {
					ex.DurationUnit = u
					out = append(out, ex)
				}
				continue
			}
			out = append(out, ex)
		}
	}
	return out
}

const proposeToolName = "propose_extractors"

var proposeJSONSchema = map[string]any{
	"type": "object",
	"properties": map[string]any{
		"candidates": map[string]any{
			"type":     "array",
			"maxItems": maxProposals,
			"items": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"field_index":     map[string]any{"type": "integer", "description": "1-based index of the whitespace-delimited field holding the value; 0 when json_path or a whole-line extract_pattern locates it"},
					"span_fields":     map[string]any{"type": "integer", "description": "timestamps only: how many whitespace-delimited fields it covers"},
					"json_path":       map[string]any{"type": "string", "description": "JSON path such as .a.b.c; empty if the line is not JSON"},
					"extract_pattern": map[string]any{"type": "string", "description": "Go (RE2) regex whose ONLY capturing group yields the value (other groups must be non-capturing, (?:...)); may use alternation or optional parts; empty if not needed"},
					"strftime":        map[string]any{"type": "string", "description": "timestamps only: strftime pattern; empty otherwise"},
				},
				"required": []string{"field_index", "span_fields", "json_path", "extract_pattern", "strftime"},
			},
		},
	},
	"required": []string{"candidates"},
}

const proposeSystemPrompt = `You help configure a log parser. A person has marked, on a sample of log lines, where one value is (and on which lines it is absent). A simple rule did not fit all their marks. Propose up to 3 different extraction rules, best first.

A rule locates the value one of three ways:
  - field_index: the 1-based index of the whitespace-delimited field, optionally refined by extract_pattern, a Go (RE2) regex with exactly one capture group applied to that field's text. Use this for levels, which cannot be searched for across the line.
  - json_path: like ".context.trace.id", when the line (or its JSON tail) is JSON.
  - extract_pattern alone (field_index 0): a Go (RE2) regex with exactly one capture group, searched across the whole line. Use alternation or optional parts when the same value is written in more than one way on different lines (for example key=abc on some lines and KEY: abc on others). The value must be in the ONLY capturing group: any other grouping, alternation included, must be non-capturing, written (?:...). RE2 has no lookahead or backreferences.

For a timestamp, also give strftime (e.g. "%Y-%m-%d %H:%M:%S,%3f"; %3f is milliseconds) and span_fields when it covers more than one whitespace-delimited field.

Every rule must give exactly the marked value on every line marked with one, and nothing on every line marked as having none. Prefer simple, readable rules.`

func proposeSystemPromptFor(f ProbeField) string {
	if f == ProbeDurationNs {
		return proposeSystemPrompt + proposeDurationNote
	}
	return proposeSystemPrompt
}

// proposeDurationNote is added to the retry prompt for a duration only, so
// the unit instructions do not perturb the other fields' proposals.
const proposeDurationNote = `

This value is a duration. Also give duration_unit: ns, us, ms or s when the extracted text is a bare number (a fraction is fine) counted in that unit, or text when the text carries its own unit, such as 5.01s, 340ms or 1m2s. Choose the unit that makes the numbers plausible durations for the work described.`

// proposeSchemaFor is the tool schema for f: the shared schema, plus the unit
// property for a duration.
func proposeSchemaFor(f ProbeField) map[string]any {
	if f != ProbeDurationNs {
		return proposeJSONSchema
	}
	items := map[string]any{}
	for k, v := range proposeJSONSchema["properties"].(map[string]any)["candidates"].(map[string]any)["items"].(map[string]any) {
		items[k] = v
	}
	props := map[string]any{}
	for k, v := range items["properties"].(map[string]any) {
		props[k] = v
	}
	props["duration_unit"] = durationUnitSchema
	items["properties"] = props
	candidates := map[string]any{}
	for k, v := range proposeJSONSchema["properties"].(map[string]any)["candidates"].(map[string]any) {
		candidates[k] = v
	}
	items["required"] = append(append([]string{}, proposeJSONSchema["properties"].(map[string]any)["candidates"].(map[string]any)["items"].(map[string]any)["required"].([]string)...), "duration_unit")
	candidates["items"] = items
	return map[string]any{
		"type":       "object",
		"properties": map[string]any{"candidates": candidates},
		"required":   []string{"candidates"},
	}
}

// buildProposePrompt describes the sample and the user's marks.
func buildProposePrompt(in ProposeInput) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Value to extract: %s.\n\nSample lines (numbered from 1):\n", probeFieldLabels[in.Field])
	for i, line := range in.Lines {
		if len(line) > 300 {
			line = line[:300] + "..."
		}
		fmt.Fprintf(&b, "%d: %s\n", i+1, line)
	}
	if len(in.Examples) == 0 && len(in.NoValueLines) == 0 {
		fmt.Fprintf(&b, "\nThe person has not marked anything yet. Propose rules only for a value you can actually see in these lines. If the sample has no %s, return an empty candidates list: the person will then say whether it appears at all and, if so, select an example for you. Never offer a timestamp, a counter or other ordinary text as a stand-in.\n", probeFieldLabels[in.Field])
	} else {
		b.WriteString("\nWhat the person marked:\n")
	}
	for _, m := range in.Examples {
		fmt.Fprintf(&b, "  line %d: the value is %q\n", m.Line+1, m.Text)
	}
	for _, l := range in.NoValueLines {
		fmt.Fprintf(&b, "  line %d has no value\n", l+1)
	}
	if in.Previous != nil {
		fmt.Fprintf(&b, "\nThe rule tried before, which did not fit: field=%q pattern=`%s`\n", in.Previous.Field, in.Previous.Pattern)
	}
	return b.String()
}

var probeFieldLabels = map[ProbeField]string{
	ProbeTimestamp: "timestamp", ProbeLevel: "log level", ProbeTraceID: "trace ID",
	ProbeSpanName: "span name", ProbeSpanID: "span ID", ProbeParentSpanID: "parent span ID",
	ProbeDurationNs: "span duration (how long the unit of work took)",
}

// ProposeExtractors asks the model for candidate extractors that fit the
// user's marks. Its answer is untrusted; Probe verifies every candidate.
func (c *BedrockClassifier) ProposeExtractors(ctx context.Context, in ProposeInput) ([]ProbeExtractor, error) {
	out, err := c.proposeRaw(ctx, in)
	if err != nil {
		return nil, err
	}
	return proposalsToExtractors(in.Lines, out, in.Field), nil
}

// proposeRaw is the model call alone (see classifyRaw).
func (c *BedrockClassifier) proposeRaw(ctx context.Context, in ProposeInput) (*proposeToolResponse, error) {
	lines := in.Lines
	if len(lines) > 50 {
		lines = lines[:50]
	}
	in.Lines = lines
	resp, err := c.client.Converse(ctx, &bedrockruntime.ConverseInput{
		ModelId: aws.String(c.modelID),
		Messages: []brtypes.Message{{
			Role:    brtypes.ConversationRoleUser,
			Content: []brtypes.ContentBlock{&brtypes.ContentBlockMemberText{Value: buildProposePrompt(in)}},
		}},
		System: []brtypes.SystemContentBlock{&brtypes.SystemContentBlockMemberText{Value: proposeSystemPromptFor(in.Field)}},
		ToolConfig: &brtypes.ToolConfiguration{
			Tools: []brtypes.Tool{&brtypes.ToolMemberToolSpec{Value: brtypes.ToolSpecification{
				Name:        aws.String(proposeToolName),
				Description: aws.String("Report candidate extraction rules for the marked value."),
				InputSchema: &brtypes.ToolInputSchemaMemberJson{Value: brdoc.NewLazyDocument(proposeSchemaFor(in.Field))},
			}}},
			ToolChoice: &brtypes.ToolChoiceMemberAny{},
		},
		InferenceConfig: &brtypes.InferenceConfiguration{Temperature: aws.Float32(0), MaxTokens: aws.Int32(1024)},
	})
	if err != nil {
		return nil, fmt.Errorf("bedrock converse: %w", err)
	}
	raw, err := extractToolUseInput(resp)
	if err != nil {
		return nil, err
	}
	var out proposeToolResponse
	if err := decodeToolInput(raw, &out); err != nil {
		return nil, fmt.Errorf("decode proposer tool response: %w", err)
	}
	return &out, nil
}

// classifyConverseInput is the classification request for a sample.
func classifyConverseInput(modelID string, det *Detection, lines []string) *bedrockruntime.ConverseInput {
	return &bedrockruntime.ConverseInput{
		ModelId: aws.String(modelID),
		Messages: []brtypes.Message{
			{
				Role:    brtypes.ConversationRoleUser,
				Content: []brtypes.ContentBlock{&brtypes.ContentBlockMemberText{Value: buildClassifyPrompt(det, lines)}},
			},
		},
		System: []brtypes.SystemContentBlock{
			&brtypes.SystemContentBlockMemberText{Value: classifySystemPrompt},
		},
		ToolConfig: &brtypes.ToolConfiguration{
			Tools:      []brtypes.Tool{&brtypes.ToolMemberToolSpec{Value: classifyToolSpec()}},
			ToolChoice: &brtypes.ToolChoiceMemberAny{},
		},
		InferenceConfig: &brtypes.InferenceConfiguration{
			Temperature: aws.Float32(0),
			MaxTokens:   aws.Int32(2048),
		},
	}
}
