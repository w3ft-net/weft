package setup

import (
	"errors"
	"fmt"

	"github.com/w3ft-net/weft/ingest"
)

// Prober applies a probe request (see ingest.Probe) to the sample being
// configured. The terminal wizard uses the server's (so the model's help and
// the exact parser are the same as the browser's); tests and offline use
// LocalProber.
type Prober func(ingest.ProbeRequest) (*ingest.ProbeResult, error)

// LocalProber probes lines in-process, without a model.
func LocalProber(lines []string) Prober {
	return func(req ingest.ProbeRequest) (*ingest.ProbeResult, error) {
		req.Lines = lines
		return ingest.Probe(req)
	}
}

// ErrValueNotInSample means every sample line was shown and none had the
// value; the caller should suggest collecting a fresh sample.
var ErrValueNotInSample = errors.New("none of the sample lines has this value")

type PickKind int

const (
	PickSelected PickKind = iota // Start/End select text within Line
	PickNextLine                 // "it's not on this line"
	PickPrevLine
	PickSkip // give up on this field
)

// PickResult is the outcome of showing the user a line to highlight on.
// Start and End are byte offsets into the line.
type PickResult struct {
	Kind       PickKind
	Line       int
	Start, End int
}

type ReviewKind int

const (
	ReviewAccept      ReviewKind = iota
	ReviewFlagMissing            // Line should have had a value and didn't
	ReviewFlagWrong              // Line got a value it should not have
	ReviewRestart
	ReviewSkip
)

// ReviewResult is the user's verdict on an extractor applied to the sample.
type ReviewResult struct {
	Kind ReviewKind
	Line int
}

// UI is everything the extraction decision tree asks of a person. The tree
// itself (ExtractField) is pure; a terminal implementation lives in tui.go
// and tests script it.
type UI interface {
	// ConfirmCandidate shows a guessed extractor's effect on the sample and
	// asks whether to use it.
	ConfirmCandidate(f ingest.ProbeField, lines []string, ex *ingest.ProbeExtractor, res *ingest.ProbeResult) (bool, error)
	// ExpectOnLines asks whether the value is expected on lines like these.
	ExpectOnLines(f ingest.ProbeField) (bool, error)
	// PickValue shows lines[line] and lets the user highlight the value, or
	// move to another line, or skip. hint says what is being asked for.
	PickValue(f ingest.ProbeField, lines []string, line int, hint string) (PickResult, error)
	// Review shows the extractor's result on every line (marks are the
	// user's flags so far) and asks whether it is right.
	Review(f ingest.ProbeField, lines []string, ex *ingest.ProbeExtractor, res *ingest.ProbeResult, marks []ingest.ProbeMark) (ReviewResult, error)
	// WantSpans asks whether the service logs spans (span name/ID, parent,
	// duration), so the four span prompts are not asked of everyone.
	WantSpans() (bool, error)
	// DurationUnit asks what a bare duration number is counted in, when the
	// marked text carries no unit of its own. Returns ns, us, ms or s.
	DurationUnit(sample []string) (string, error)
	// OfferLargerSample is asked when the value is on none of the sampled
	// lines: whether to collect a bigger sample to look for it in. Yes ends
	// the walk (ErrLargerSample).
	OfferLargerSample(f ingest.ProbeField) (bool, error)
	Notify(msg string)
}

// LargerSampleLines is the size of the sample asked for when a value was on
// none of the lines of the first one.
const LargerSampleLines = 500

// ErrLargerSample ends the walk: the user wants a bigger sample, to find a
// value the current one doesn't show. The caller requests it and stops;
// nothing is configured from a half-finished walk.
var ErrLargerSample = errors.New("a larger sample was requested")

// ExtractField walks the decision tree for one value:
//
//  1. Show each detected candidate in turn. One accepted: done.
//     1b. None accepted (or none detected): ask the server for a first guess,
//     which asks the model, and show it the same way.
//  2. Otherwise ask whether the value is expected on these lines at all.
//     No: skip it.
//  3. Yes: show lines until the user highlights the value on one, or has
//     seen them all (ErrValueNotInSample).
//  4. Build an extractor from the highlight, apply it to the whole sample,
//     and ask for review. Lines it missed or got wrong are flagged; each
//     flag becomes a constraint and the extractor is rebuilt, until the
//     user accepts it.
//
// A nil extractor with a nil error means the field is skipped.
func ExtractField(ui UI, probe Prober, f ingest.ProbeField, lines []string, base ingest.ProbeBase, candidates []*ingest.ProbeExtractor) (*ingest.ProbeExtractor, error) {
	for _, cand := range candidates {
		res, err := probe(ingest.ProbeRequest{Field: f, Base: base, Previous: cand})
		if err != nil {
			ui.Notify(fmt.Sprintf("could not check a suggested %s rule: %v", fieldNames[f], err))
			continue
		}
		ok, err := ui.ConfirmCandidate(f, lines, cand, res)
		if err != nil {
			return nil, err
		}
		if ok {
			return cand, nil
		}
	}
	// A guess from the model, for when detection found nothing or the person
	// turned it down. A failure here only means no guess: they can still point.
	if res, err := probe(ingest.ProbeRequest{Field: f, Base: base, Propose: true}); err == nil && res.Extractor != nil && !sameExtractor(res.Extractor, candidates) {
		ok, err := ui.ConfirmCandidate(f, lines, res.Extractor, res)
		if err != nil {
			return nil, err
		}
		if ok {
			return res.Extractor, nil
		}
	}
	expect, err := ui.ExpectOnLines(f)
	if err != nil {
		return nil, err
	}
	if !expect {
		return nil, nil
	}

	var marks []ingest.ProbeMark
	var prev *ingest.ProbeExtractor
	hint := "is it on this line?"
	line, seen := 0, map[int]bool{}
	unit, unitAsked := "", false

	mark := func(p PickResult) ingest.ProbeMark {
		return ingest.ProbeMark{Line: p.Line, Prefix: lines[p.Line][:p.Start], Text: lines[p.Line][p.Start:p.End]}
	}

	for {
		if !hasPositive(marks) {
			seen[line] = true
			p, err := ui.PickValue(f, lines, line, hint)
			if err != nil {
				return nil, err
			}
			switch p.Kind {
			case PickSkip:
				return nil, nil
			case PickNextLine:
				if len(seen) >= len(lines) {
					return nil, ErrValueNotInSample
				}
				line = (line + 1) % len(lines)
				continue
			case PickPrevLine:
				line = (line - 1 + len(lines)) % len(lines)
				continue
			}
			marks = setMark(marks, mark(p))
		}

		res, err := probe(ingest.ProbeRequest{Field: f, Base: base, Marks: marks, Previous: prev, DurationUnit: unit})
		if err != nil {
			return nil, err
		}
		// Marked text with no unit of its own ("12.5") says nothing about
		// what it counts; the person does.
		if f == ingest.ProbeDurationNs && res.Extractor != nil && res.NeedsUnit && !unitAsked {
			unitAsked = true
			u, err := ui.DurationUnit(markedTexts(marks))
			if err != nil {
				return nil, err
			}
			if u != "" && u != "ns" {
				unit = u
				continue
			}
		}
		if res.Extractor == nil {
			ui.Notify(res.Message)
			marks = marks[:len(marks)-1] // drop the mark that made it unsatisfiable
			if !hasPositive(marks) {
				prev = nil
			}
			continue
		}
		prev = res.Extractor

		rv, err := ui.Review(f, lines, res.Extractor, res, marks)
		if err != nil {
			return nil, err
		}
		switch rv.Kind {
		case ReviewAccept:
			return res.Extractor, nil
		case ReviewSkip:
			return nil, nil
		case ReviewRestart:
			marks, prev, hint = nil, nil, "is it on this line?"
			line, seen = 0, map[int]bool{}
		case ReviewFlagWrong:
			marks = setMark(marks, ingest.ProbeMark{Line: rv.Line, None: true})
		case ReviewFlagMissing:
			p, err := ui.PickValue(f, lines, rv.Line, fmt.Sprintf("the rule missed line %d; highlight the value on it", rv.Line+1))
			if err != nil {
				return nil, err
			}
			switch p.Kind {
			case PickSelected:
				marks = setMark(marks, mark(p))
			case PickSkip:
				return nil, nil
			}
		}
	}
}

// sameExtractor reports whether ex is one of cands (already shown).
func sameExtractor(ex *ingest.ProbeExtractor, cands []*ingest.ProbeExtractor) bool {
	for _, c := range cands {
		if c.Field == ex.Field && c.Pattern == ex.Pattern && c.TimestampFormat == ex.TimestampFormat && c.DurationUnit == ex.DurationUnit {
			return true
		}
	}
	return false
}

func markedTexts(marks []ingest.ProbeMark) []string {
	var out []string
	for _, m := range marks {
		if !m.None {
			out = append(out, m.Text)
		}
	}
	return out
}

func hasPositive(marks []ingest.ProbeMark) bool {
	for _, m := range marks {
		if !m.None {
			return true
		}
	}
	return false
}

// setMark records m, replacing any earlier mark on the same line.
func setMark(marks []ingest.ProbeMark, m ingest.ProbeMark) []ingest.ProbeMark {
	for i := range marks {
		if marks[i].Line == m.Line {
			marks[i] = m
			return marks
		}
	}
	return append(marks, m)
}

// ExtractAll walks the decision tree for every extractable value and
// records the answers on schema. Each starts from detection's guess (det
// may be nil). A value that is nowhere in the sample offers a larger one;
// accepting returns ErrLargerSample, declining leaves the value unset.
func ExtractAll(ui UI, probe Prober, schema *ingest.Schema, lines []string, det *ingest.Detection) error {
	base := ingest.ProbeBase{
		Envelope: schema.Envelope, Format: schema.Format,
		FieldSeparator: schema.FieldSeparator, JSONStartField: schema.JSONStartField,
	}
	run := func(f ingest.ProbeField) error {
		ex, err := ExtractField(ui, probe, f, lines, base, ingest.DetectedExtractors(det, f))
		if errors.Is(err, ErrValueNotInSample) {
			want, aerr := ui.OfferLargerSample(f)
			if aerr != nil {
				return aerr
			}
			if want {
				return ErrLargerSample
			}
			ex, err = nil, nil
		}
		if err != nil {
			return err
		}
		applyExtractor(schema, f, ex)
		return nil
	}

	for _, f := range []ingest.ProbeField{ingest.ProbeTimestamp, ingest.ProbeLevel, ingest.ProbeTraceID} {
		if err := run(f); err != nil {
			return err
		}
	}

	spanFields := []ingest.ProbeField{ingest.ProbeSpanName, ingest.ProbeSpanID, ingest.ProbeParentSpanID, ingest.ProbeDurationNs}
	detected := false
	for _, f := range spanFields {
		if len(ingest.DetectedExtractors(det, f)) > 0 {
			detected = true
		}
	}
	if !detected {
		want, err := ui.WantSpans()
		if err != nil {
			return err
		}
		if !want {
			return nil
		}
	}
	for _, f := range spanFields {
		if err := run(f); err != nil {
			return err
		}
	}
	return nil
}

// applyExtractor records ex as f's extraction on schema; nil clears it.
func applyExtractor(s *ingest.Schema, f ingest.ProbeField, ex *ingest.ProbeExtractor) {
	if ex == nil {
		ex = &ingest.ProbeExtractor{}
	}
	// A spec belongs to the value, so it is replaced or removed with it.
	if ex.Spec != nil {
		if s.Extract == nil {
			s.Extract = map[string]ingest.ExtractSpec{}
		}
		s.Extract[string(f)] = *ex.Spec
	} else {
		delete(s.Extract, string(f))
	}
	switch f {
	case ingest.ProbeTimestamp:
		s.TimestampField, s.TimestampFormat, s.TimestampFields = ex.Field, ex.TimestampFormat, ex.TimestampFields
	case ingest.ProbeLevel:
		s.LevelField, s.LevelPattern = ex.Field, ex.Pattern
	case ingest.ProbeTraceID:
		s.TraceIDField, s.TraceIDPattern = ex.Field, ex.Pattern
	case ingest.ProbeSpanName:
		s.SpanNameField, s.SpanNamePattern = ex.Field, ex.Pattern
	case ingest.ProbeSpanID:
		s.SpanIDField, s.SpanIDPattern = ex.Field, ex.Pattern
	case ingest.ProbeParentSpanID:
		s.ParentSpanIDField, s.ParentSpanIDPattern = ex.Field, ex.Pattern
	case ingest.ProbeDurationNs:
		s.DurationNsField, s.DurationNsPattern, s.DurationUnit = ex.Field, ex.Pattern, ex.DurationUnit
	}
}

// distinctValues lists up to max distinct values a result read, in order of
// first appearance: what a person looks at to judge a candidate.
func distinctValues(res *ingest.ProbeResult, max int) []string {
	var out []string
	seen := map[string]bool{}
	for _, r := range res.Results {
		if r.Matched && !seen[r.Value] && len(out) < max {
			seen[r.Value] = true
			out = append(out, r.Value)
		}
	}
	return out
}
