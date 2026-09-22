package compute

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/aws/aws-sdk-go-v2/credentials"

	pb "github.com/graybeardcorp/weft/proto/weftpb"
	"github.com/graybeardcorp/weft/shard"
)

// TestResolveRegion documents the contract the compute worker needs:
// AWS SDK v2's default chain does not reliably derive a region from
// IMDS (see commit f931874, which already fixed coordinator/shard/api).
// The compute worker — which moved to owning Lambda SigV4 signing in
// d477ea2 — must resolve a region the same way: AWS_REGION env, with a
// us-east-2 fallback so signing never happens with an empty region.
func TestResolveRegion(t *testing.T) {
	t.Setenv("AWS_REGION", "")
	if got := ResolveRegion(); got != "us-east-2" {
		t.Fatalf("ResolveRegion() with unset AWS_REGION = %q, want fallback %q", got, "us-east-2")
	}

	t.Setenv("AWS_REGION", "eu-west-1")
	if got := ResolveRegion(); got != "eu-west-1" {
		t.Fatalf("ResolveRegion() with AWS_REGION=eu-west-1 = %q, want %q", got, "eu-west-1")
	}
}

// credentialScopeRegion pulls the region segment out of a SigV4
// Authorization header. Scope shape:
//
//	Credential=<access>/<yyyymmdd>/<region>/<service>/aws4_request
//
// An empty <region> segment is exactly what makes a Lambda function
// URL's IAM authorizer reject the request with
// 403 {"message":"Credential should be scoped to a valid region. "}.
func credentialScopeRegion(t *testing.T, authz string) string {
	t.Helper()
	const marker = "Credential="
	i := strings.Index(authz, marker)
	if i < 0 {
		t.Fatalf("Authorization header has no Credential= scope: %q", authz)
	}
	scope := authz[i+len(marker):]
	if c := strings.IndexByte(scope, ','); c >= 0 {
		scope = scope[:c]
	}
	parts := strings.Split(scope, "/")
	if len(parts) < 5 {
		t.Fatalf("malformed credential scope %q", scope)
	}
	return parts[2]
}

// TestComputeFanoutSignsLambdaWithValidRegion reproduces the prod 403.
//
// The compute worker fans out to a Lambda-backed cold shard and signs
// the request with SigV4. With AWS_REGION unset (the prod compute
// instance state — weft-compute.service has no EnvironmentFile), the
// region used for signing must still be valid; otherwise AWS rejects
// every cold-shard query with the "Credential should be scoped to a
// valid region" 403 we see 33k times in /log/weft/compute/main.log.
//
// This test fails before the fix: there is no region fallback in the
// compute path, so the fan-out signs with an empty region scope.
func TestComputeFanoutSignsLambdaWithValidRegion(t *testing.T) {
	t.Setenv("AWS_REGION", "")

	var mu sync.Mutex
	var gotAuthz string
	var hits int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		gotAuthz = r.Header.Get("Authorization")
		hits++
		mu.Unlock()
		w.Header().Set("Content-Type", "application/x-protobuf-stream")
		_ = shard.WriteStreamEnd(w)
	}))
	defer srv.Close()

	// Configure the pool exactly as cmd/compute/main.go does.
	pool := &LocalComputePool{
		AWSCreds:  credentials.NewStaticCredentialsProvider("AKIDEXAMPLE", "secret", ""),
		AWSRegion: ResolveRegion(),
	}

	stream, err := pool.ExecuteQuery(context.Background(), &pb.ExecuteQueryRequest{
		Sql: "SELECT raw, ts FROM logs WHERE customer = 'kevlar'",
		Shards: []*pb.ShardDescriptor{{
			ShardId:     "cold-test",
			LambdaUrl:   srv.URL,
			ParquetPath: "s3://weft-cold/shard-x.parquet",
			Services:    []string{"vimgym"},
		}},
	})
	if err != nil {
		t.Fatalf("ExecuteQuery: %v", err)
	}
	ctx := context.Background()
	for {
		if _, ok, nerr := stream.Next(ctx); !ok {
			if nerr != nil {
				t.Fatalf("stream error: %v", nerr)
			}
			break
		}
	}
	_ = stream.Close()

	mu.Lock()
	defer mu.Unlock()
	if hits == 0 {
		t.Fatal("lambda cold shard was never reached by the compute fan-out")
	}
	region := credentialScopeRegion(t, gotAuthz)
	if region == "" {
		t.Fatal("compute signed the Lambda request with an EMPTY region scope " +
			"— AWS returns 403 \"Credential should be scoped to a valid region\" for this")
	}
	if region != "us-east-2" {
		t.Fatalf("signed region scope = %q, want %q", region, "us-east-2")
	}
}
