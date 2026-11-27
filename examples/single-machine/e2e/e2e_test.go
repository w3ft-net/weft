//go:build e2e

// Package e2e drives the open-source stack end to end: the api, the
// coordinator and compute in containers, and a shard the coordinator launches
// as a sibling container. Nothing is in process; every call crosses the wire
// the way a user's does.
//
//	docker compose up -d --build
//	go test -tags e2e ./e2e/ -v -timeout=10m
package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"

	pb "github.com/w3ft-net/weft/proto/weftpb"
)

// These match docker-compose.yml.
const (
	apiURL     = "http://localhost:18080"
	healthURL  = "http://localhost:19021/health"
	ingestAddr = "localhost:19020"
	basicUser  = "e2e"
	basicPass  = "e2e-password"
)

func TestOpenStack_IngestsAndQueriesALine(t *testing.T) {
	requireStack(t)

	post(t, "/api/namespaces", map[string]string{"name": "e2e"}, http.StatusOK, http.StatusCreated)
	var tok struct {
		Token string `json:"token"`
	}
	json.Unmarshal(post(t, "/api/tokens", map[string]string{"label": "e2e", "namespace": "e2e"}, http.StatusCreated), &tok)
	if tok.Token == "" {
		t.Fatal("no agent token returned")
	}

	marker := fmt.Sprintf("needle-%d", time.Now().UnixNano())

	// The first shard is launched on demand, some seconds after the first row
	// arrives, so this line reaches the drain before any shard serves its
	// service. It is dead-lettered, and the leader's replay loop (every minute)
	// puts it back once a shard does. It is sent once: the test fails if the
	// dead-letter store loses it.
	ingest(t, tok.Token, marker)

	deadline := time.Now().Add(5 * time.Minute)
	var last string
	for time.Now().Before(deadline) {
		last = query(t, "cat /log/e2e/web/main.log | grep "+marker)
		if strings.Contains(last, marker) {
			return
		}
		time.Sleep(3 * time.Second)
	}
	t.Fatalf("the ingested line never became queryable; last response:\n%s", last)
}

func TestOpenStack_RefusesARequestWithoutTheCredential(t *testing.T) {
	requireStack(t)
	resp, err := http.Get(apiURL + "/api/sources")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("GET /api/sources without a credential = %d, want 401", resp.StatusCode)
	}
}

// requireStack fails (not skips) when the stack is not up: an e2e run that
// silently tested nothing is worse than one that says why.
func requireStack(t *testing.T) {
	t.Helper()
	deadline := time.Now().Add(90 * time.Second)
	for {
		hc, err := http.Get(healthURL)
		if err == nil {
			hc.Body.Close()
			if hc.StatusCode == 200 {
				req, _ := http.NewRequest("GET", apiURL+"/api/sources", nil)
				req.SetBasicAuth(basicUser, basicPass)
				if r, err := http.DefaultClient.Do(req); err == nil {
					r.Body.Close()
					if r.StatusCode == 200 {
						return
					}
				}
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("the stack is not up (docker compose up -d --build): coordinator %s, api %s", healthURL, apiURL)
		}
		time.Sleep(2 * time.Second)
	}
}

func post(t *testing.T, path string, body any, want ...int) []byte {
	t.Helper()
	b, _ := json.Marshal(body)
	req, _ := http.NewRequest("POST", apiURL+path, bytes.NewReader(b))
	req.SetBasicAuth(basicUser, basicPass)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST %s: %v", path, err)
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(resp.Body)
	for _, w := range want {
		if resp.StatusCode == w {
			return out
		}
	}
	t.Fatalf("POST %s = %d, want one of %v: %s", path, resp.StatusCode, want, out)
	return nil
}

func ingest(t *testing.T, token, marker string) {
	t.Helper()
	conn, err := grpc.NewClient(ingestAddr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	ctx := metadata.AppendToOutgoingContext(context.Background(), "authorization", "Bearer "+token)
	stream, err := pb.NewWeftCoordinatorClient(conn).IngestStream(ctx)
	if err != nil {
		t.Fatalf("open ingest stream: %v", err)
	}
	for i := 0; i < 5; i++ {
		if err := stream.Send(&pb.LogRow{
			Namespace: "e2e", Service: "web", Instance: "e2e-host",
			Raw:      fmt.Sprintf("%s line %d", marker, i),
			TsUnixNs: time.Now().UnixNano(),
		}); err != nil {
			t.Fatalf("send: %v", err)
		}
	}
	if _, err := stream.CloseAndRecv(); err != nil {
		t.Fatalf("close ingest stream: %v", err)
	}
}

// query runs a pipeline through the api's shell and returns the response body.
func query(t *testing.T, pipeline string) string {
	t.Helper()
	b, _ := json.Marshal(map[string]string{"pipeline": pipeline, "range": "1h.."})
	req, _ := http.NewRequest("POST", apiURL+"/api/shell/exec", bytes.NewReader(b))
	req.SetBasicAuth(basicUser, basicPass)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("shell exec: %v", err)
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(resp.Body)
	return fmt.Sprintf("%d %s", resp.StatusCode, out)
}
