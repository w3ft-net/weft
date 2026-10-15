package ingest

import (
	"reflect"
	"strings"
	"testing"
)

func notesByValue(notes []MigrationNote) map[string]MigrationNote {
	m := map[string]MigrationNote{}
	for _, n := range notes {
		m[n.Value] = n
	}
	return m
}

// A schema that reads fields the legacy way is rewritten to say, explicitly,
// what the legacy readers did implicitly; nothing else changes.
func TestMigrateLegacyWritesTheImplicitTrimsDown(t *testing.T) {
	old := &Schema{
		Service: "svc", Format: "text",
		TimestampField: "1", TimestampFields: 2, TimestampFormat: "2006-01-02 15:04:05,000",
		LevelField: "3", TraceIDField: "none", TraceIDPattern: `id=(\w+)`,
	}
	old.SpanIDField = ".span.id"
	old.DurationNsField = "6"

	got, notes := MigrateLegacy(old)
	n := notesByValue(notes)

	if n["timestamp"].Kind != MigrationConverted || n["level"].Kind != MigrationConverted || n["duration_ns"].Kind != MigrationConverted {
		t.Errorf("positional values should convert: %+v", notes)
	}
	if n["trace_id"].Kind != MigrationKept || n["span_id"].Kind != MigrationKept {
		t.Errorf("a whole-line regex and a JSON path have nothing implicit to write down: %+v", notes)
	}
	if got.Extract["level"].Chars != legacyFieldTrim || got.Extract["timestamp"].Chars == "" {
		t.Errorf("extract = %+v", got.Extract)
	}
	if _, ok := got.Extract["trace_id"]; ok {
		t.Error("a whole-line regex gained a spec")
	}
	if old.Extract != nil {
		t.Error("the input schema was modified")
	}
	if problems := got.Validate(); len(problems) != 0 {
		t.Errorf("migrated schema invalid: %v", problems)
	}

	// Idempotent: nothing is left to convert.
	again, notes2 := MigrateLegacy(got)
	if !reflect.DeepEqual(again, got) {
		t.Error("migrating twice changed the schema again")
	}
	for _, note := range notes2 {
		if note.Kind == MigrationConverted {
			t.Errorf("second pass converted %s", note.Value)
		}
	}
}

// A named envelope cannot be rewritten into a spec: its prefix would become
// fields (shifting every index) and its instance a value. The schema is left
// alone and the reason given.
func TestMigrateLegacyBlocksOnAnEnvelope(t *testing.T) {
	old := &Schema{Format: "text", Envelope: "kevcons", LevelField: "1"}
	got, notes := MigrateLegacy(old)
	if !reflect.DeepEqual(got, old) {
		t.Errorf("a schema with an envelope was changed: %+v", got)
	}
	var blocked bool
	for _, n := range notes {
		blocked = blocked || (n.Kind == MigrationBlocked && strings.Contains(n.Why, "envelope"))
	}
	if !blocked {
		t.Errorf("notes = %+v, want one blocking on the envelope", notes)
	}
}

// The replay is what decides whether a conversion is safe. Here the legacy
// level reader falls back to the whole field when the pattern does not match;
// the explicit form does not, and the comparison must catch it.
func TestCompareSchemasCatchesALegacyFallback(t *testing.T) {
	old := &Schema{
		Format: "text", TimestampField: "1", TimestampFields: 2, TimestampFormat: "2006-01-02 15:04:05,000",
		LevelField: "3", LevelPattern: `^(\w+)/`,
	}
	lines := []string{
		"[2026-10-15 12:00:01,123: INFO/MainProcess] a",
		"[2026-10-15 12:00:02,456: WARNING] no delimiter, so the legacy fallback reads the whole field",
		"[2026-10-15 12:00:03,789: ERROR/Worker-1] c",
	}
	migrated, _ := MigrateLegacy(old)
	diffs, err := CompareSchemas(old, migrated, lines)
	if err != nil {
		t.Fatal(err)
	}
	if len(diffs) != 1 || diffs[0].Line != 1 || diffs[0].Value != "level" || diffs[0].Old != "WARNING" || diffs[0].New != "" {
		t.Errorf("diffs = %+v, want exactly the fallback line's level (WARNING -> nothing)", diffs)
	}

	// Without the fallback case the two agree on everything.
	diffs, _ = CompareSchemas(old, migrated, []string{lines[0], lines[2]})
	if len(diffs) != 0 {
		t.Errorf("diffs = %+v, want none", diffs)
	}
}

// Identical extraction on realistic lines, timestamps included.
func TestCompareSchemasIdenticalForPlainConversion(t *testing.T) {
	old := &Schema{
		Format: "text", TimestampField: "1", TimestampFields: 2, TimestampFormat: "2006-01-02 15:04:05,000",
		LevelField: "3", TraceIDField: "5",
	}
	lines := []string{
		"[2026-10-15 12:00:01,123: [INFO] abc123 hello",
		"[2026-10-15 12:00:02,456: (WARN) def456 world",
		"[2026-10-15 12:00:03,789: ERROR: ghi789 again",
	}
	migrated, _ := MigrateLegacy(old)
	diffs, err := CompareSchemas(old, migrated, lines)
	if err != nil || len(diffs) != 0 {
		t.Errorf("diffs = %+v, err %v; want identical", diffs, err)
	}
}
