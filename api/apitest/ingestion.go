package apitest

import (
	"slices"
	"testing"
	"time"

	"github.com/w3ft-net/weft/backends/ingestfail"
)

// Ingestion runs the route that tells a tenant which of its services failed to
// ingest.
func Ingestion(t *testing.T, newSubject func(t *testing.T) Subject) {
	each(t, newSubject, []namedCase{
		{"StatusListsTheCallersServices", ingestionStatusOwn},
		{"StatusListsOnlyTheCallersServices", ingestionStatusIsolated},
	})
}

func (s Subject) affected(t *testing.T) []string {
	t.Helper()
	w := s.Do("GET", "/api/ingestion-status", nil)
	if w.Code != 200 {
		t.Fatalf("status %d: %s", w.Code, w.Body.String())
	}
	var body struct {
		Affected []string `json:"affected_services"`
	}
	decode(t, w, &body)
	return body.Affected
}

func ingestionStatusOwn(t *testing.T, s Subject) {
	if err := s.Ingest.Record(ingestfail.Failure{Owner: s.Owner, At: time.Now(), ShardID: "s", Message: "x", Rows: 1, Services: []string{"ops/mine/main"}}); err != nil {
		t.Fatal(err)
	}
	if got := s.affected(t); !slices.Equal(got, []string{"ops/mine/main"}) {
		t.Fatalf("affected_services = %v, want the caller's", got)
	}
}

// A tenant is told about its own services and nothing about anyone else's: the
// endpoint used to list every tenant's.
func ingestionStatusIsolated(t *testing.T, s Subject) {
	s.needsOther(t)
	now := time.Now()
	for owner, svc := range map[string]string{"mine": "ops/mine/main", "theirs": "ops/theirs/main"} {
		o := s.Owner
		if owner == "theirs" {
			o = s.Other
		}
		if err := s.Ingest.Record(ingestfail.Failure{Owner: o, At: now, ShardID: "s", Message: "x", Rows: 1, Services: []string{svc}}); err != nil {
			t.Fatal(err)
		}
	}
	if got := s.affected(t); !slices.Equal(got, []string{"ops/mine/main"}) {
		t.Fatalf("affected_services = %v, want only the caller's", got)
	}
}
