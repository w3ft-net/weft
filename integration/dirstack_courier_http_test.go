//go:build integration && !localstack

package integration

import (
	"bufio"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"
)

// TestDirstackCourier_HTTP drives the API's /api/shell/exec SSE endpoint
// with a `dirstack` field and asserts the post-run dirstack comes back as
// a `cwd` SSE event. This is the browser/CLI-facing half of the courier:
// JSON dirstack in → weft-dirstack gRPC metadata → coordinator → trailer
// → cwd event out. The gRPC half is covered by TestDirstackCourier_GRPC;
// this proves the api layer ferries it both ways.
func TestDirstackCourier_HTTP(t *testing.T) {
	// /api/shell/exec gates on an active subscription. The harness seeds
	// the kevlar customer + dev team member but no subscription, so seed
	// one here (idempotent) and use the dev JWT the harness's membership
	// row is wired to.
	db := openTestStore(t)
	cust, err := db.GetCustomerByName("kevlar")
	if err != nil {
		t.Fatalf("resolve kevlar customer: %v", err)
	}
	db.SeedDevSubscription(cust.ID)
	jwt := generateDevJWT(t)

	// cwd event for a pwd seeded at /bin (a plain-bash pipeline — no shard
	// data needed). The server resolves pwd against the ferried cwd and
	// returns the post-run stack on the trailer → cwd SSE event.
	cwd, stack := shellExecCwd(t, jwt, "pwd", []string{"/bin"})
	if cwd != "/bin" {
		t.Errorf("cwd event cwd = %q, want /bin", cwd)
	}
	if len(stack) != 1 || stack[0] != "/bin" {
		t.Errorf("cwd event stack = %v, want [/bin]", stack)
	}

	// A cd in the shipped line moves the returned cwd.
	cwd2, _ := shellExecCwd(t, jwt, "cd /etc && pwd", []string{"/bin"})
	if cwd2 != "/etc" {
		t.Errorf("after `cd /etc`, cwd event = %q, want /etc", cwd2)
	}
}

// shellExecCwd POSTs pipeline+dirstack to /api/shell/exec, parses the SSE
// stream, and returns the (cwd, stack) carried by the "cwd" event. Fails
// the test if no cwd event arrives before "done".
func shellExecCwd(t *testing.T, jwt, pipeline string, dirStack []string) (string, []string) {
	t.Helper()
	body, _ := json.Marshal(map[string]any{
		"pipeline": pipeline,
		"range":    "1d..",
		"dirstack": dirStack,
	})
	req, _ := http.NewRequest("POST", onboardAPIAddr+"/api/shell/exec", strings.NewReader(string(body)))
	req.Header.Set("Authorization", "Bearer "+jwt)
	req.Header.Set("Content-Type", "application/json")

	client := &http.Client{Timeout: 20 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("POST /api/shell/exec: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("POST /api/shell/exec: HTTP %d", resp.StatusCode)
	}

	var gotCwd string
	var gotStack []string
	sawCwd := false
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 64*1024), 1024*1024)
	for sc.Scan() {
		line := sc.Text()
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		var ev struct {
			Type  string   `json:"type"`
			Cwd   string   `json:"cwd"`
			Stack []string `json:"stack"`
		}
		if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &ev); err != nil {
			continue
		}
		switch ev.Type {
		case "cwd":
			gotCwd, gotStack, sawCwd = ev.Cwd, ev.Stack, true
		case "done":
			if !sawCwd {
				t.Fatalf("stream ended (done) with no cwd event for pipeline %q", pipeline)
			}
			return gotCwd, gotStack
		case "error":
			t.Fatalf("SSE error event for pipeline %q", pipeline)
		}
	}
	if err := sc.Err(); err != nil {
		t.Fatalf("scan SSE: %v", err)
	}
	if !sawCwd {
		t.Fatalf("no cwd event and no done event for pipeline %q", pipeline)
	}
	return gotCwd, gotStack
}

var _ = fmt.Sprintf
