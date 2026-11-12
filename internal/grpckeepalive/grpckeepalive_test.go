package grpckeepalive

import "testing"

// A server refuses a client that pings faster than its policy allows, so the
// client's interval must not be below the server's minimum, and an idle
// connection must be probeable on both sides or the probe is itself refused.
func TestClientPingsNoFasterThanTheServerAccepts(t *testing.T) {
	if Client.Time < ServerEnforcement.MinTime {
		t.Errorf("client pings every %v but servers accept one every %v: they would send GOAWAY too_many_pings", Client.Time, ServerEnforcement.MinTime)
	}
	if Client.PermitWithoutStream && !ServerEnforcement.PermitWithoutStream {
		t.Error("clients probe idle connections but servers refuse pings with no stream open")
	}
}
