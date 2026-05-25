//go:build integration && !localstack

package integration

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	"github.com/kevlarcon/weft/auth"
	pb "github.com/kevlarcon/weft/proto/weftpb"
	"github.com/kevlarcon/weft/store"
)

const (
	onboardCoordAddr = "localhost:9020"
	onboardAPIAddr   = "http://localhost:8080"
	// Must match docker-compose.yml's JWT_SECRET for the coordinator
	// + api services, which in turn matches the compiled-in default
	// from cmd/api/main.go and cmd/coordinator/main.go.
	jwtSecret = "dev-secret-change-in-production"
)

// TestOnboardFlow exercises the full weftd schema onboarding lifecycle:
//
//  1. weftd starts, reads /var/log/testapp.log, authenticates, checks source status
//  2. Source is unknown → weftd submits a sample via gRPC SubmitSample
//  3. Source status becomes "pending"
//  4. Test (acting as human reviewer) configures the source via HTTP API
//  5. Source status becomes "configured" with schema YAML
//  6. weftd starts streaming → data becomes queryable
func TestOnboardFlow(t *testing.T) {
	conn, client := onboardGRPCClient(t)
	defer conn.Close()

	// ── Phase 1: Wait for weftd to submit its sample ──────────────
	//
	// weftd is running in Docker, tailing /var/log/testapp.log with app=testapp.
	// On startup it calls GetSourceStatus (unknown) → SubmitSample → status becomes pending.
	// We poll GetSourceStatus from outside using the same dev token.

	t.Log("waiting for weftd to submit sample...")
	var pendingStatus string
	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		resp, err := client.GetSourceStatus(context.Background(), &pb.GetSourceStatusRequest{
			Token:     "wft_dev_0000000000000000",
			Namespace: "testapp",
		})
		if err != nil {
			t.Logf("  GetSourceStatus: %v (retrying)", err)
			time.Sleep(2 * time.Second)
			continue
		}
		pendingStatus = resp.Status
		if pendingStatus == "pending" {
			t.Log("  sample submitted — source is pending")
			break
		}
		if pendingStatus == "configured" {
			t.Log("  source already configured (previous test run?) — skipping to query verification")
			break
		}
		t.Logf("  status: %s (waiting for pending)", pendingStatus)
		time.Sleep(2 * time.Second)
	}
	if pendingStatus != "pending" && pendingStatus != "configured" {
		t.Fatalf("timed out waiting for weftd to submit sample — last status: %q", pendingStatus)
	}

	// ── Phase 2: Verify pending source via HTTP API ───────────────

	jwt := generateDevJWT(t)

	if pendingStatus == "pending" {
		t.Log("verifying pending source via HTTP API...")
		sources := listPendingSources(t, jwt)
		found := false
		for _, s := range sources {
			if s.App == "testapp" && s.Namespace == "kevlar" {
				found = true
				t.Logf("  found pending source: app=%s namespace=%s instance=%s", s.App, s.Namespace, s.Instance)

				// Verify the sample contains actual log lines.
				var lines []string
				if err := json.Unmarshal(s.Sample, &lines); err != nil {
					t.Fatalf("parse sample: %v", err)
				}
				if len(lines) == 0 {
					t.Fatal("sample has 0 lines")
				}
				t.Logf("  sample has %d lines, first: %q", len(lines), lines[0])
				break
			}
		}
		if !found {
			t.Fatalf("testapp not found in pending sources: %+v", sources)
		}

		// ── Phase 3: Configure the source ─────────────────────────

		t.Log("configuring source via HTTP API...")
		configureSource(t, jwt, "kevlar", "testapp", map[string]any{
			"format":           "text",
			"timestamp_field":  "0",
			"timestamp_format": "2006-01-02 15:04:05",
			"level_field":      "2",
		})
		t.Log("  source configured")
	}

	// ── Phase 4: Verify source is now configured via gRPC ─────────

	t.Log("verifying source is configured via gRPC...")
	resp, err := client.GetSourceStatus(context.Background(), &pb.GetSourceStatusRequest{
		Token:     "wft_dev_0000000000000000",
		Namespace: "testapp",
	})
	if err != nil {
		t.Fatalf("GetSourceStatus after configure: %v", err)
	}
	if resp.Status != "configured" {
		t.Fatalf("expected status=configured, got %q", resp.Status)
	}
	if resp.SchemaYaml == "" {
		t.Fatal("configured status but empty schema YAML")
	}
	t.Logf("  schema YAML present (%d bytes)", len(resp.SchemaYaml))

	// ── Phase 5: Drive a fresh row through the configured pipeline ──
	//
	// weftd's Tailer opens the file and seeks to EOF (production-correct:
	// a restart shouldn't replay the entire log). The 6 fixture lines in
	// dev/testapp.log were already past the tailer's cursor by the time
	// configuration completed — they were used for the SubmitSample read
	// (which os.Opens fresh from byte 0), not for streaming. To verify
	// the post-configure streaming path actually works, we have to write
	// fresh lines to the host-side file and confirm one shows up via
	// query. The bind mount surfaces the inotify Write event into the
	// container; the cleanup restores the file so the working tree
	// stays clean across runs.
	//
	// Cold-start race: weftd's WaitForConfiguration polls every 5s, so
	// there's a window between the test seeing status=configured and
	// weftd's poll loop waking up to start the Tailer. A single append
	// done before the Tailer exists lands behind its EOF cursor and is
	// never emitted. Solution: append on every poll iteration with a
	// fresh marker so at least one write lands after the Tailer is
	// running.

	logPath := testappLogPath(t)
	original, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read testapp.log: %v", err)
	}
	t.Cleanup(func() {
		if err := os.WriteFile(logPath, original, 0o644); err != nil {
			t.Logf("restore testapp.log: %v", err)
		}
	})

	tag := fmt.Sprintf("onboard-test-%d", time.Now().UnixNano())
	// weftd's file tailer ships <ns>/<svc>/main, so the queryable
	// pseudofile is the canonical 3-seg form (post-d81b562 layout).
	pipeline := fmt.Sprintf("cat /log/kevlar/testapp/main.log | grep %s", tag)

	t.Logf("appending tagged lines and polling for tagged row to flow through weftd → coordinator → shard...")
	var rows []*pb.ResultRow
	deadline = time.Now().Add(60 * time.Second)
	for attempt := 1; time.Now().Before(deadline); attempt++ {
		freshLine := fmt.Sprintf("2026-04-27T22:00:00Z INFO %s attempt-%d\n", tag, attempt)
		if err := appendLine(logPath, freshLine); err != nil {
			t.Fatalf("append to testapp.log: %v", err)
		}

		time.Sleep(3 * time.Second)
		stream, err := client.Execute(context.Background(), &pb.CoordinatorExecuteRequest{
			Pipeline: pipeline,
			Customer: "kevlar",
		})
		if err != nil {
			t.Logf("  query error: %v (retrying)", err)
			continue
		}
		rows = nil
		for {
			resp, err := stream.Recv()
			if err != nil {
				break
			}
			for _, line := range resp.Lines {
				rows = append(rows, &pb.ResultRow{Raw: line.Raw})
			}
		}
		if len(rows) > 0 {
			t.Logf("  got %d rows after %d append(s)", len(rows), attempt)
			break
		}
		t.Logf("  attempt %d: 0 rows (retrying)", attempt)
	}
	if len(rows) == 0 {
		t.Fatal("timed out waiting for tagged row — weftd's Tailer didn't pick up the host-side appends, or the row didn't reach a queryable shard")
	}

	if !strings.Contains(rows[0].Raw, tag) {
		t.Errorf("first row Raw=%q did not contain tag %q", rows[0].Raw, tag)
	}
	t.Logf("onboard flow complete: tagged row queryable")
}

func appendLine(path, line string) error {
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.WriteString(line)
	return err
}

// testappLogPath returns the absolute host path of dev/testapp.log,
// resolved relative to this test file rather than the cwd. The
// docker-compose.yml bind-mounts the same path into the weftd
// container at /var/log/testapp.log.
func testappLogPath(t *testing.T) string {
	t.Helper()
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	return filepath.Join(filepath.Dir(thisFile), "..", "dev", "testapp.log")
}

// TestGlobOnboardFlow reproduces the multi-file glob onboarding case used in
// the production coordinator config (path: /opt/app/logs/*.log).
//
// The weftd-glob container tails /var/log/testlogs/*.log, which matches
// nginx.log, app.log, and worker.log. Each file should onboard
// independently — the coordinator should end up with three distinct pending
// sources (one per file) within the timeout.
//
// Current behaviour: weftd derives the app name from the glob pattern
// ("*.log" → "*") and passes the glob string into SubmitSample, which tries
// os.Open("/var/log/testlogs/*.log") and fails with ENOENT. No pending
// sources are ever created, and this test times out.
func TestGlobOnboardFlow(t *testing.T) {
	conn, client := onboardGRPCClient(t)
	defer conn.Close()

	expectedApps := []string{"nginx", "app", "worker"}

	t.Log("waiting for weftd-glob to submit a sample for each file...")
	found := map[string]bool{}
	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) && len(found) < len(expectedApps) {
		for _, app := range expectedApps {
			if found[app] {
				continue
			}
			resp, err := client.GetSourceStatus(context.Background(), &pb.GetSourceStatusRequest{
				Token:   "wft_dev_0000000000000000",
				Service: app,
			})
			if err != nil {
				t.Logf("  GetSourceStatus(%s): %v (retrying)", app, err)
				continue
			}
			if resp.Status == "pending" || resp.Status == "configured" {
				t.Logf("  %s → %s", app, resp.Status)
				found[app] = true
			}
		}
		if len(found) < len(expectedApps) {
			time.Sleep(2 * time.Second)
		}
	}

	if len(found) < len(expectedApps) {
		var missing []string
		for _, app := range expectedApps {
			if !found[app] {
				missing = append(missing, app)
			}
		}
		t.Fatalf("glob onboarding did not produce pending sources for: %v (got %v)", missing, found)
	}

	// Sanity-check the pending list too: each app should appear with a
	// non-empty sample. If any file was onboarded under a weird derived name
	// (e.g. "*"), that would show up here.
	jwt := generateDevJWT(t)
	sources := listPendingSources(t, jwt)
	for _, app := range expectedApps {
		var match *pendingSourceResp
		for i := range sources {
			if sources[i].App == app && sources[i].Namespace == "kevlar" {
				match = &sources[i]
				break
			}
		}
		if match == nil {
			t.Errorf("no pending source listed for app=%s namespace=kevlar", app)
			continue
		}
		var lines []string
		if err := json.Unmarshal(match.Sample, &lines); err != nil {
			t.Errorf("%s: parse sample: %v", app, err)
			continue
		}
		if len(lines) == 0 {
			t.Errorf("%s: sample has 0 lines", app)
		}
	}

	// Also confirm nothing was onboarded under a literal "*" app name —
	// which is what the current glob-handling bug would produce if the
	// SubmitSample call were ever to succeed.
	for _, s := range sources {
		if s.App == "*" {
			t.Errorf("found pending source with literal wildcard app name: %+v", s)
		}
	}
}

// ── Helpers ──────────────────────────────────────────────────────

func onboardGRPCClient(t *testing.T) (*grpc.ClientConn, pb.WeftCoordinatorClient) {
	t.Helper()
	tok := generateDevJWT(t)
	conn, err := grpc.NewClient(onboardCoordAddr,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithUnaryInterceptor(auth.ClientTokenUnaryInterceptor(tok)),
		grpc.WithStreamInterceptor(auth.ClientTokenStreamInterceptor(tok)),
	)
	if err != nil {
		t.Fatalf("connect to coordinator: %v", err)
	}
	return conn, pb.NewWeftCoordinatorClient(conn)
}

func generateDevJWT(t *testing.T) string {
	t.Helper()
	user := &store.User{
		ID:    "dev-user",
		Email: "dev@test.local",
	}
	token, err := auth.Generate(user, jwtSecret)
	if err != nil {
		t.Fatalf("generate JWT: %v", err)
	}
	return token
}

type pendingSourceResp struct {
	App       string          `json:"app"`
	Namespace string          `json:"namespace"`
	Instance  string          `json:"instance"`
	HostGroup string          `json:"host_group"`
	Sample    json.RawMessage `json:"sample"`
	Detection json.RawMessage `json:"detection"`
	CreatedAt string          `json:"created_at"`
}

func listPendingSources(t *testing.T, jwt string) []pendingSourceResp {
	t.Helper()
	req, _ := http.NewRequest("GET", onboardAPIAddr+"/api/sources/pending", nil)
	req.Header.Set("Authorization", "Bearer "+jwt)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("list pending: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("list pending: HTTP %d: %s", resp.StatusCode, body)
	}

	var sources []pendingSourceResp
	if err := json.NewDecoder(resp.Body).Decode(&sources); err != nil {
		t.Fatalf("decode pending: %v", err)
	}
	return sources
}

func configureSource(t *testing.T, jwt, namespace, app string, config map[string]any) {
	t.Helper()
	body, _ := json.Marshal(config)
	url := fmt.Sprintf("%s/api/sources/%s/%s/configure", onboardAPIAddr, namespace, app)
	req, _ := http.NewRequest("POST", url, strings.NewReader(string(body)))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+jwt)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("configure source: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		respBody, _ := io.ReadAll(resp.Body)
		t.Fatalf("configure source: HTTP %d: %s", resp.StatusCode, respBody)
	}
}
