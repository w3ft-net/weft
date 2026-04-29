package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"testing"
	"time"
)

// shortSocketPath returns a Unix-socket path in /tmp short enough
// to fit under the OS limit (104 chars on macOS). t.TempDir() lands
// under /var/folders/... which is well over the limit on darwin.
func shortSocketPath(t *testing.T) string {
	t.Helper()
	f, err := os.CreateTemp("/tmp", "weftd-hb-*.sock")
	if err != nil {
		t.Fatal(err)
	}
	path := f.Name()
	f.Close()
	os.Remove(path)
	t.Cleanup(func() { os.Remove(path) })
	return path
}

// TestStartHeartbeat_SnapshotEmitsRow drives the snapshot ticker
// end-to-end: configure with a tight cadence, register a hit, then
// confirm a row lands on the mock coordinator stream.
func TestStartHeartbeat_SnapshotEmitsRow(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	socket := shortSocketPath(t)
	cfg := HeartbeatConfig{
		Socket:           socket,
		SnapshotInterval: "20ms",
		StaleAfter:       "10s",
		MaxServices:      16,
	}
	client := &mockCoordClient{}

	h, err := startHeartbeat(ctx, cfg, client, "kevlar", "i-001", "myns")
	if err != nil {
		t.Fatal(err)
	}

	// Register a heartbeat directly through the returned handle —
	// avoids wiring up an http.Client to the Unix socket here. The
	// listener path is exercised in TestStartHeartbeat_UnixSocketPost.
	h.Hit("test-svc")

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if s := client.latestStream(); s != nil && s.sentCount() > 0 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	stream := client.latestStream()
	if stream == nil {
		t.Fatal("snapshot ticker never opened a stream")
	}
	if stream.sentCount() == 0 {
		t.Fatal("snapshot ticker opened a stream but never sent a row")
	}

	stream.mu.Lock()
	row := stream.sent[0]
	stream.mu.Unlock()

	if row.App != "myns/test-svc/heartbeats" {
		t.Errorf("row.App = %q, want %q", row.App, "myns/test-svc/heartbeats")
	}
	if row.Customer != "kevlar" {
		t.Errorf("row.Customer = %q, want %q", row.Customer, "kevlar")
	}
	var payload map[string]any
	if err := json.Unmarshal(row.PayloadJson, &payload); err != nil {
		t.Fatal(err)
	}
	if payload["type"] != "heartbeat" {
		t.Errorf("payload.type = %v, want heartbeat", payload["type"])
	}
	if payload["service"] != "test-svc" {
		t.Errorf("payload.service = %v, want test-svc", payload["service"])
	}
}

// TestStartHeartbeat_UnixSocketPost confirms that a POST over the
// Unix socket lands in the in-memory map. Closes the loop on the
// listener wiring that TestStartHeartbeat_SnapshotEmitsRow doesn't
// exercise.
func TestStartHeartbeat_UnixSocketPost(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	socket := shortSocketPath(t)
	cfg := HeartbeatConfig{
		Socket:           socket,
		SnapshotInterval: "10s", // long enough to not race with the test
		StaleAfter:       "1m",
	}
	client := &mockCoordClient{}

	h, err := startHeartbeat(ctx, cfg, client, "kevlar", "i-001", "myns")
	if err != nil {
		t.Fatal(err)
	}

	httpClient := &http.Client{
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				return (&net.Dialer{}).DialContext(ctx, "unix", socket)
			},
		},
		Timeout: time.Second,
	}

	// The Unix-socket listener takes a moment to start; retry briefly.
	var resp *http.Response
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		resp, err = httpClient.Post("http://localhost/h/svc-from-socket", "", nil)
		if err == nil {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err != nil {
		t.Fatalf("POST over unix socket: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Errorf("status = %d, want 204", resp.StatusCode)
	}

	if h.Len() != 1 {
		t.Errorf("Heartbeats.Len = %d, want 1", h.Len())
	}
}

// TestStartHeartbeat_TCPListener exercises the optional TCP path
// when an operator sets `port:`.
func TestStartHeartbeat_TCPListener(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Find a free TCP port by binding briefly to :0, then closing.
	probe, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := probe.Addr().(*net.TCPAddr).Port
	probe.Close()

	cfg := HeartbeatConfig{
		Socket:           shortSocketPath(t),
		Port:             port,
		SnapshotInterval: "10s",
		StaleAfter:       "1m",
	}
	client := &mockCoordClient{}

	h, err := startHeartbeat(ctx, cfg, client, "kevlar", "i-001", "myns")
	if err != nil {
		t.Fatal(err)
	}

	url := fmt.Sprintf("http://127.0.0.1:%d/h/svc-from-tcp", port)
	var resp *http.Response
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		resp, err = http.Post(url, "", nil)
		if err == nil {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err != nil {
		t.Fatalf("POST over TCP: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Errorf("status = %d, want 204", resp.StatusCode)
	}
	if h.Len() != 1 {
		t.Errorf("Heartbeats.Len = %d, want 1", h.Len())
	}
}

// TestStartHeartbeat_FansOutPerService confirms each registered
// service gets its own <ns>/<service>/heartbeats app stream rather
// than being bundled into a single <ns>/heartbeats stream.
func TestStartHeartbeat_FansOutPerService(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	cfg := HeartbeatConfig{
		Socket:           shortSocketPath(t),
		SnapshotInterval: "20ms",
		StaleAfter:       "10s",
	}
	client := &mockCoordClient{}
	h, err := startHeartbeat(ctx, cfg, client, "kevlar", "i-001", "weft")
	if err != nil {
		t.Fatal(err)
	}
	h.Hit("coordinator")
	h.Hit("api")

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if s := client.latestStream(); s != nil && s.sentCount() >= 2 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	stream := client.latestStream()
	if stream == nil {
		t.Fatal("snapshot row never emitted")
	}

	stream.mu.Lock()
	apps := map[string]bool{}
	for _, r := range stream.sent {
		apps[r.App] = true
	}
	stream.mu.Unlock()

	for _, want := range []string{"weft/coordinator/heartbeats", "weft/api/heartbeats"} {
		if !apps[want] {
			t.Errorf("missing app %q in fan-out; got %v", want, apps)
		}
	}
}
