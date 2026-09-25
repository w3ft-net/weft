package ingest

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"regexp"
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
	lines := det.StrippedLines
	if len(lines) == 0 {
		lines = det.Lines
	}
	if len(lines) > 30 {
		lines = lines[:30]
	}
	if len(lines) == 0 {
		return &ClassifierResult{}, nil
	}

	resp, err := c.client.Converse(ctx, &bedrockruntime.ConverseInput{
		ModelId: aws.String(c.modelID),
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
	})
	if err != nil {
		return nil, fmt.Errorf("bedrock converse: %w", err)
	}

	raw, err := extractToolUseInput(resp)
	if err != nil {
		return nil, err
	}

	var out classifyToolResponse
	if err := raw.UnmarshalSmithyDocument(&out); err != nil {
		return nil, fmt.Errorf("decode classifier tool response: %w", err)
	}

	return validateClassification(&out, lines), nil
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
func validateClassification(t *classifyToolResponse, lines []string) *ClassifierResult {
	result := &ClassifierResult{}

	if t.Timestamp != nil && t.Timestamp.Present {
		if c := validateTimestampField(t.Timestamp, lines); c != nil {
			result.Timestamp = c
		}
	}

	for _, role := range result.tagRoles(t) {
		if role.src == nil || !role.src.Present {
			continue
		}
		if c := validateTagField(role.src, lines); c != nil {
			*role.dst = c
		} else {
			log.Printf("classify: dropping unverified %s candidate: %+v", role.name, role.src)
		}
	}

	return result
}

// minVerifyHits/minVerifyRatio: a candidate must resolve on at least this
// many lines, and on this fraction of the lines it was even eligible to
// match (positional: line has enough fields; JSON path: line parses as
// JSON; whole-line search: always eligible). Mirrors the heuristic
// detectors' own thresholds (detect.go's >50% timestamp / >1/3 level).
const (
	minVerifyHits  = 2
	minVerifyRatio = 0.5
)

func validateTimestampField(f *timestampField, lines []string) *TimestampCandidate {
	if f.Strftime == "" {
		return nil
	}
	layout, err := strftimeToGoLayout(f.Strftime)
	if err != nil {
		log.Printf("classify: dropping timestamp candidate, bad strftime %q: %v", f.Strftime, err)
		return nil
	}

	if f.JSONPath != "" {
		segs, err := compileJSONPath(f.JSONPath)
		if err != nil {
			log.Printf("classify: dropping timestamp candidate, bad json_path %q: %v", f.JSONPath, err)
			return nil
		}
		hits, eligible := 0, 0
		for _, line := range lines {
			body, ok := parseJSONBody(line)
			if !ok {
				continue
			}
			eligible++
			val, ok := evalJSONPath(body, segs)
			if !ok {
				continue
			}
			str, ok := val.(string)
			if !ok {
				continue
			}
			if _, err := time.Parse(layout, str); err == nil {
				hits++
			}
		}
		if !verifies(hits, eligible) {
			return nil
		}
		return &TimestampCandidate{JSONPath: f.JSONPath, Format: layout, Sample: f.SampleValue}
	}

	if f.FieldIndex < 1 {
		return nil
	}
	spans := f.SpanFields
	if spans < 1 {
		spans = 1
	}
	hits, eligible := 0, 0
	for _, line := range lines {
		fields := strings.Fields(line)
		if f.FieldIndex-1+spans > len(fields) {
			continue
		}
		eligible++
		val := strings.Join(fields[f.FieldIndex-1:f.FieldIndex-1+spans], " ")
		val = strings.TrimPrefix(val, "[")
		val = strings.TrimRight(val, "]:")
		if _, err := time.Parse(layout, val); err == nil {
			hits++
		}
	}
	if !verifies(hits, eligible) {
		return nil
	}
	return &TimestampCandidate{FieldIndex: f.FieldIndex, SpanFields: spans, Format: layout, Sample: f.SampleValue}
}

func validateTagField(f *tagField, lines []string) *FieldCandidate {
	var pat *regexp.Regexp
	if f.ExtractPattern != "" {
		re, err := regexp.Compile(f.ExtractPattern)
		if err != nil {
			log.Printf("classify: dropping candidate, bad extract_pattern %q: %v", f.ExtractPattern, err)
			return nil
		}
		pat = re
	}

	switch {
	case f.JSONPath != "":
		segs, err := compileJSONPath(f.JSONPath)
		if err != nil {
			log.Printf("classify: dropping candidate, bad json_path %q: %v", f.JSONPath, err)
			return nil
		}
		hits, eligible := 0, 0
		for _, line := range lines {
			body, ok := parseJSONBody(line)
			if !ok {
				continue
			}
			eligible++
			if str, ok := jsonStringAt(body, true, segs); ok && matchesPattern(pat, str) {
				hits++
			}
		}
		if !verifies(hits, eligible) {
			return nil
		}
		return &FieldCandidate{JSONPath: f.JSONPath, Pattern: f.ExtractPattern, Sample: f.SampleValue}

	case f.FieldIndex >= 1:
		hits, eligible := 0, 0
		for _, line := range lines {
			fields := strings.Fields(line)
			if f.FieldIndex > len(fields) {
				continue
			}
			eligible++
			val := strings.Trim(fields[f.FieldIndex-1], "[]():")
			if matchesPattern(pat, val) {
				hits++
			}
		}
		if !verifies(hits, eligible) {
			return nil
		}
		return &FieldCandidate{FieldIndex: f.FieldIndex, Pattern: f.ExtractPattern, Sample: f.SampleValue}

	case pat != nil:
		// Whole-line search: no field to anchor on, the pattern alone
		// has to find the value somewhere in the line — schema.go's
		// documented TraceIDPattern convention.
		hits := 0
		for _, line := range lines {
			if m := pat.FindStringSubmatch(line); len(m) > 1 {
				hits++
			}
		}
		if !verifies(hits, len(lines)) {
			return nil
		}
		return &FieldCandidate{Pattern: f.ExtractPattern, Sample: f.SampleValue}
	}

	return nil
}

func matchesPattern(pat *regexp.Regexp, val string) bool {
	if pat == nil {
		return val != ""
	}
	m := pat.FindStringSubmatch(val)
	return len(m) > 1
}

func verifies(hits, eligible int) bool {
	return hits >= minVerifyHits && eligible > 0 && float64(hits)/float64(eligible) >= minVerifyRatio
}

// classifySystemPrompt explains the task once; buildClassifyPrompt adds
// the per-sample specifics (format, JSON examples, raw lines).
const classifySystemPrompt = `You classify a sample of application/server log lines to help configure a log parser. For each of seven fields (timestamp, level, trace_id, span_name, span_id, parent_span_id, duration_ns), decide whether it's present in the sample and, if so, how to extract it.

Two ways to locate a field:
  - Positional: field_index is the 1-based index of the whitespace-delimited field (after any leading envelope has already been stripped for you).
  - JSON path: json_path like ".context.trace_id" or ".tags[0].value" — use this whenever the line (or its JSON tail) is JSON and the value lives inside it, including nested objects and arrays. Leave field_index at 0 when using json_path.

For trace_id/span_name/span_id/parent_span_id/duration_ns, if the value isn't cleanly at one field index or JSON path but appears as a trailing "key=value" tag whose position varies line to line (e.g. "... trace_id=abc123" at the end of a variable-length line), leave field_index at 0 and json_path empty, and give extract_pattern as a regex with exactly one capture group that finds it anywhere in the line (e.g. "trace_id=(\\S+)"). extract_pattern may also be used to refine a specific field_index/json_path (e.g. a Celery-style "INFO/MainProcess" field where the pattern "^(\\w+)/" pulls out just "INFO").

These are examples of conventions you might see, not the only ones — look at the actual sample:
  - trace/correlation IDs: trace_id=, X-Request-Id, request_id, otel.trace_id, nested context.trace.id
  - span fields: span_name=/operation=, span_id=, parent_span_id=/parent_id=, duration_ns=/duration_ms=/took_ms=
  - levels: not just DEBUG/INFO/WARN/ERROR — also SEVERE, NOTICE, numeric severities, framework-specific values like "INFO/MainProcess"

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
		"present":         map[string]any{"type": "boolean"},
		"field_index":     map[string]any{"type": "integer"},
		"json_path":       map[string]any{"type": "string"},
		"extract_pattern": map[string]any{"type": "string"},
		"sample_value":    map[string]any{"type": "string"},
	},
	"required": []string{"present"},
}

var classifyJSONSchema = map[string]any{
	"type": "object",
	"properties": map[string]any{
		"timestamp": map[string]any{
			"type": "object",
			"properties": map[string]any{
				"present":      map[string]any{"type": "boolean"},
				"field_index":  map[string]any{"type": "integer"},
				"span_fields":  map[string]any{"type": "integer"},
				"json_path":    map[string]any{"type": "string"},
				"strftime":     map[string]any{"type": "string"},
				"sample_value": map[string]any{"type": "string"},
			},
			"required": []string{"present"},
		},
		"level":          tagFieldSchema,
		"trace_id":       tagFieldSchema,
		"span_name":      tagFieldSchema,
		"span_id":        tagFieldSchema,
		"parent_span_id": tagFieldSchema,
		"duration_ns":    tagFieldSchema,
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
// which must not wait on Bedrock's availability/latency.
func ClassifyAsync(classifier Classifier, lines []string, final bool, onComplete func(*Detection)) *Detection {
	det := Detect(lines)
	if !final || classifier == nil {
		return det
	}

	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()

		result, err := classifier.Classify(ctx, det)
		if err != nil {
			log.Printf("classify: bedrock classification failed, falling back to heuristic text detection: %v", err)
			// Heuristic fallback covers timestamp/level only — the only
			// two fields the pattern-matching detector has ever known
			// how to find. Envelope/JSON detection is already in det;
			// running detectTimestamps/detectLevels again fills in what
			// the LLM would have replaced.
			det.detectTimestamps(det.effectiveLines())
			det.detectLevels(det.effectiveLines())
			onComplete(det)
			return
		}

		applyClassification(det, result)
		onComplete(det)
	}()

	return det
}

// effectiveLines is the lines detectTimestamps/detectLevels/etc. were
// always meant to run against — StrippedLines when an envelope was
// stripped, otherwise the cleaned raw lines. Matches Detect's own
// "stripped := d.StrippedLines; if len(stripped) == 0 { stripped =
// d.Lines }" convention (detect.go).
func (d *Detection) effectiveLines() []string {
	if len(d.StrippedLines) > 0 {
		return d.StrippedLines
	}
	return d.Lines
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
