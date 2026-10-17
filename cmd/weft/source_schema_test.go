package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
	"github.com/graybeardcorp/weft/shell"
)

const schemaBody = `{"status":"configured","schema_yaml":"service: api\nformat: text",
"preview":{"lines":[[{"text":"2026-01-01","field":"timestamp"},{"text":" INFO","field":"level"},{"text":" hi"}]],"fields":["timestamp","level"]}}`

func TestSourceSchemaDecodesPreview(t *testing.T) {
	var sch shell.SourceSchema
	if err := json.Unmarshal([]byte(schemaBody), &sch); err != nil {
		t.Fatal(err)
	}
	if sch.Preview == nil || len(sch.Preview.Lines[0]) != 3 || len(sch.Preview.Fields) != 2 {
		t.Fatalf("bad decode: %+v", sch.Preview)
	}
	var note shell.SourceSchema
	if err := json.Unmarshal([]byte(`{"status":"configured","preview_note":"no recent lines"}`), &note); err != nil || note.PreviewNote != "no recent lines" || note.Preview != nil {
		t.Fatalf("note decode: %+v %v", note, err)
	}
}

func fetchSchema(t *testing.T, body string) *shell.SourceSchema {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/sources/ns/svc/schema" {
			t.Errorf("path %s", r.URL.Path)
		}
		w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	sch, err := shell.NewHTTPCoordinator(srv.URL, "tok", "").GetSourceSchema("ns", "svc")
	if err != nil {
		t.Fatal(err)
	}
	return sch
}

func TestPrintSourceSchemaPreviewAndYAML(t *testing.T) {
	sch := fetchSchema(t, schemaBody)
	var buf bytes.Buffer
	printSourceSchema(&buf, sch, "ns", "svc", false, 80)
	if want := "2026-01-01 INFO hi\n\n■ timestamp  ■ level\n"; !strings.HasSuffix(buf.String(), want) && buf.String() != want {
		// colors may wrap the key; compare stripped
		if stripped := ansi.Strip(buf.String()); stripped != want {
			t.Fatalf("got %q", stripped)
		}
	}
	buf.Reset()
	printSourceSchema(&buf, sch, "ns", "svc", true, 80)
	if buf.String() != "service: api\nformat: text\n" {
		t.Fatalf("yaml output %q", buf.String())
	}
}

func TestPrintSourceSchemaNote(t *testing.T) {
	sch := fetchSchema(t, `{"status":"configured","schema_yaml":"x: 1\n","preview_note":"no recent lines"}`)
	var buf bytes.Buffer
	printSourceSchema(&buf, sch, "ns", "svc", false, 80)
	if got := ansi.Strip(buf.String()); got != "no recent lines\n" {
		t.Fatalf("got %q", got)
	}
}
