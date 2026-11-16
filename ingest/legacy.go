package ingest

import (
	"regexp"
	"strings"
)

// Legacy readers: every hardcoded character and name left in the parsing path
// lives in this file. Schemas stored before the wizard learned everything from
// the data (the `extract` spec) still depend on them: a value with no `extract`
// entry is read with the trims below, and a schema may name an envelope. Nothing here is used to *build* a
// schema (detection and the wizard know no envelopes at all: a prefix like
// "[time] [id]" is just fields, read by position like any others). It exists
// only so those stored schemas keep parsing, and is meant to be deleted once
// none remain.

// legacyEnvelopes maps the name a stored schema's `envelope` field carries to
// the prefix it strips; group 2 is the instance stamped on the row.
var legacyEnvelopes = map[string]*regexp.Regexp{
	"kevcons": regexp.MustCompile(`^\[(\d{4}-\d{2}-\d{2} \d{2}:\d{2}:\d{2})\] \[(i-[a-f0-9]+)\] `),
}

// legacyFieldTrim is what the legacy positional extractors (a schema value
// with no `extract` entry) strip from both ends of a field before reading it.
const legacyFieldTrim = "[]():"

// legacyTimestampTrim is what the legacy timestamp extractor strips from the
// joined fields: one leading "[", then one trailing "]", then one trailing ":".
func legacyTimestampTrim(val string) string {
	val = strings.TrimPrefix(val, "[")
	val = strings.TrimSuffix(val, "]")
	return strings.TrimSuffix(val, ":")
}

// legacyTimestampChars approximates legacyTimestampTrim as a character set: it
// strips the same brackets and colon, but any number of them. The replay in
// CompareSchemas is what says whether that difference matters for a source.
const legacyTimestampChars = "[]:"

// MigrationKind says what MigrateLegacy did with one value.
type MigrationKind int

const (
	MigrationKept      MigrationKind = iota // nothing implicit to write down
	MigrationConverted                      // rewritten to an explicit extract spec
	MigrationBlocked                        // cannot be rewritten; the whole schema is left alone
)

// MigrationNote is MigrateLegacy's verdict on one value ("level", ...), or on
// the schema as a whole ("schema").
type MigrationNote struct {
	Value string
	Kind  MigrationKind
	Why   string
}

// MigrateLegacy rewrites a schema stored in the legacy form to the explicit
// one: every positional value gets an extract spec that states the trims the
// legacy readers applied implicitly (the legacy character sets), so the parser
// no longer needs to know them. Values read by JSON path, by a whole-line
// pattern, or already by a spec are left as they are. A schema that names an
// envelope is returned unchanged with a blocking note: the envelope's prefix
// would have to become fields (shifting every index) and its instance a value,
// which is a decision for the wizard, not a rewrite.
//
// The result is meant to be checked, not trusted: CompareSchemas replays real
// lines through both, because some legacy behavior (a level pattern falling
// back to the whole field when it does not match) has no explicit equivalent.
func MigrateLegacy(s *Schema) (*Schema, []MigrationNote) {
	out := *s
	out.Extract = nil
	if len(s.Extract) > 0 {
		out.Extract = make(map[string]ExtractSpec, len(s.Extract))
		for k, v := range s.Extract {
			out.Extract[k] = v
		}
	}

	if s.Envelope != "" && s.Envelope != "none" {
		return &out, []MigrationNote{{Value: "schema", Kind: MigrationBlocked,
			Why: "names the envelope " + s.Envelope + ": its prefix would become fields and its instance a value; re-run the wizard"}}
	}

	var notes []MigrationNote
	for id := valueID(0); id < numValues; id++ {
		name := valueNames[id]
		field, _ := s.valueRef(id)
		note := MigrationNote{Value: name, Kind: MigrationKept}
		switch _, hasSpec := s.Extract[name]; {
		case hasSpec:
			note.Why = "already explicit"
		case field == "" || field == "none" || field == "0":
			note.Why = "not read by field"
		case strings.HasPrefix(field, "."):
			note.Why = "read by JSON path"
		default:
			spec := ExtractSpec{Chars: legacyFieldTrim}
			if id == vTimestamp {
				spec.Chars = legacyTimestampChars
			}
			if out.Extract == nil {
				out.Extract = map[string]ExtractSpec{}
			}
			out.Extract[name] = spec
			note.Kind, note.Why = MigrationConverted, "field "+field+": the legacy trims, written out"
		}
		notes = append(notes, note)
	}
	return &out, notes
}
