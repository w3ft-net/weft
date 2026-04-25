package store

import "testing"

// TestAgentToken_EnvRoundTrip verifies env is persisted at create
// time and surfaces from ValidateAgentToken so the IngestStream
// handler can stamp it on rows. See docs/plans/ingest-dimensions.md.
func TestAgentToken_EnvRoundTrip(t *testing.T) {
	db := testOpen(t)

	cust, err := db.FindOrCreateCustomer("env-test")
	if err != nil {
		t.Fatal(err)
	}
	ns, err := db.FindOrCreateNamespace(cust.ID, "api")
	if err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name string
		env  string
	}{
		{"with env", "production"},
		{"empty env", ""},
		{"staging env", "staging"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			raw, _, err := db.CreateAgentToken(cust.ID, ns.ID, "label-"+tc.name, tc.env)
			if err != nil {
				t.Fatalf("CreateAgentToken: %v", err)
			}

			result, err := db.ValidateAgentToken(raw)
			if err != nil {
				t.Fatalf("ValidateAgentToken: %v", err)
			}
			if result == nil {
				t.Fatal("ValidateAgentToken returned nil")
			}
			if result.Env != tc.env {
				t.Errorf("Env = %q, want %q", result.Env, tc.env)
			}
		})
	}
}
