package store

import "testing"

// TestValidateAgentTokenForProducer pins the scoping invariant
// SyslogServer relies on (docs/plans/render.md, Component 3): a token
// only validates for the exact producer it was minted for, and a
// plain (unscoped) token never validates for any producer at all.
func TestValidateAgentTokenForProducer(t *testing.T) {
	db := testOpen(t)

	cust, err := db.FindOrCreateOrg("producer-scope-test")
	if err != nil {
		t.Fatal(err)
	}
	ns, err := db.FindOrCreateNamespace(cust.ID, "test")
	if err != nil {
		t.Fatal(err)
	}

	renderRaw, _, err := db.CreateAgentToken(cust.ID, ns.ID, "render-token", "", "render")
	if err != nil {
		t.Fatal(err)
	}
	plainRaw, _, err := db.CreateAgentToken(cust.ID, ns.ID, "plain-token", "", "")
	if err != nil {
		t.Fatal(err)
	}

	res, err := db.ValidateAgentTokenForProducer(renderRaw, "render")
	if err != nil {
		t.Fatal(err)
	}
	if res == nil {
		t.Fatal("render token should validate for producer=render")
	}
	if res.Producer != "render" {
		t.Errorf("Producer = %q, want %q", res.Producer, "render")
	}

	if res, err := db.ValidateAgentTokenForProducer(renderRaw, "flyio"); err != nil {
		t.Fatal(err)
	} else if res != nil {
		t.Fatal("render token should not validate for a different producer")
	}

	if res, err := db.ValidateAgentTokenForProducer(plainRaw, "render"); err != nil {
		t.Fatal(err)
	} else if res != nil {
		t.Fatal("unscoped token should not validate for any producer")
	}

	// The plain token still validates on the producer-agnostic path.
	res, err = db.ValidateAgentToken(plainRaw)
	if err != nil {
		t.Fatal(err)
	}
	if res == nil {
		t.Fatal("plain token should still validate on ValidateAgentToken")
	}
}
