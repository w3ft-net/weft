//go:build !defaultshape

package api

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/graybeardcorp/weft/ingest"
)

// A source written by the weft SDK says so on its schema: its values come
// from the line header, and a source on an ordinary schema carries no note.
func TestSourceSchema_SDKNote(t *testing.T) {
	srv, s := testServer(t)
	cust, jwt := previewUser(t, s, "sdknote")
	ns, _ := s.FindOrCreateNamespace(cust.ID, "ns1")
	srv.coordClient = startPreviewCoord(t, &previewCoord{})

	for _, tc := range []struct {
		service string
		sdk     bool
	}{{"sdkapp", true}, {"plainapp", false}} {
		if err := s.UpsertPendingLogGroup(ns.ID, tc.service, "i", "d", previewSampleJSON, `{}`, 2, false); err != nil {
			t.Fatal(err)
		}
		yaml := "app: " + tc.service + "\nenvelope: none\nformat: text\ntimestamp_field: none\n"
		if tc.sdk {
			var err error
			if yaml, err = ingest.SDKSchema(tc.service, "json"); err != nil {
				t.Fatal(err)
			}
		}
		if err := s.ConfigureLogGroup(context.Background(), ns.ID, tc.service, yaml); err != nil {
			t.Fatal(err)
		}

		w := doJSON(srv.Handler(), "GET", "/api/sources/ns1/"+tc.service+"/schema", jwt, nil)
		if w.Code != 200 {
			t.Fatalf("%s: status %d %s", tc.service, w.Code, w.Body.String())
		}
		var resp struct {
			SDKNote string `json:"sdk_note"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
			t.Fatal(err)
		}
		if tc.sdk && resp.SDKNote != ingest.SDKDerivedNote {
			t.Errorf("%s: sdk_note = %q, want the derived-values note", tc.service, resp.SDKNote)
		}
		if !tc.sdk && resp.SDKNote != "" {
			t.Errorf("%s: sdk_note = %q on an ordinary schema, want none", tc.service, resp.SDKNote)
		}
	}
}
