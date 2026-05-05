//go:build integration

// Package integration provides the shared test harness for weft integration
// tests. TestMain verifies the docker stack is running and ensures the
// MinIO cold-storage bucket exists. Migrations are the coordinator's
// responsibility (store.Open → goose.Up) — by the time the coordinator's
// /health endpoint returns 200, the schema is up to date.
//
// Typical workflow:
//
//	docker compose up -d --build
//	go test -tags integration ./integration/ -v -timeout=5m
//
// Or use ./scripts/integration-test.sh which wraps both steps.
package integration

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"fmt"
	"log"
	"net/http"
	"os"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	_ "github.com/jackc/pgx/v5/stdlib"
)

const (
	MinioEndpoint  = "localhost:9050"
	MinioAccessKey = "minioadmin"
	MinioSecretKey = "minioadmin"
	ColdBucket     = "weft-cold"
	CoordAddr      = "localhost:9020"

	coordHealth    = "http://localhost:9021/health"
	postgresURL    = "postgres://weft:weft@localhost:5433/weft?sslmode=disable"
	readinessTries = 15
)

// Setup verifies the stack is up and ensures the MinIO bucket exists.
// Returns a cleanup function. Call from TestMain.
//
// When WEFT_SKIP_STACK_CHECK=1, Setup returns immediately without
// hitting /health or seeding fixtures. That mode is for coverage-
// boundary tests that only read coverage/<service>.out on disk —
// they don't need the docker stack up.
func Setup() func() {
	os.Setenv("S3_ENDPOINT", MinioEndpoint)
	os.Setenv("S3_ACCESS_KEY", MinioAccessKey)
	os.Setenv("S3_SECRET_KEY", MinioSecretKey)
	os.Setenv("S3_REGION", "us-east-1")
	os.Setenv("S3_USE_SSL", "false")

	if os.Getenv("WEFT_SKIP_STACK_CHECK") == "1" {
		log.Println("integration: WEFT_SKIP_STACK_CHECK=1 — skipping stack health check")
		return func() {}
	}

	requireStackRunning()

	if err := ensureMinioBucket(); err != nil {
		log.Fatalf("integration: MinIO setup: %v", err)
	}
	if err := seedFixtures(); err != nil {
		log.Fatalf("integration: seed fixtures: %v", err)
	}

	log.Println("integration: ready")

	return func() {}
}

// seedFixtures idempotently inserts the customers + namespaces the
// integration tests assume exist. The CoordinatorExecuteRequest's
// customer field is ignored server-side (callerCustomer() reads the
// JWT), so every test-internal customer name collapses onto the JWT's
// customer (kevlar). Seeding under kevlar covers all the namespace
// names tests reference.
func seedFixtures() error {
	conn, err := sql.Open("pgx", postgresURL)
	if err != nil {
		return fmt.Errorf("open postgres: %w", err)
	}
	defer conn.Close()

	// customer id for kevlar. The coordinator container's own
	// FindOrCreateCustomer flow normally creates this row at startup
	// via WEFT_CUSTOMER, but the demo seed's --reset path can clobber
	// the dev DB out from under a long-running coordinator. Upsert
	// here so the integration suite owns its preconditions instead of
	// inheriting them from the operator's compose-stack lifecycle.
	var kevID int64
	if err := conn.QueryRow(`
		INSERT INTO customers (name, created_at, updated_at)
		VALUES ('kevlar', now(), now())
		ON CONFLICT (name) DO UPDATE SET updated_at = now()
		RETURNING id
	`).Scan(&kevID); err != nil {
		return fmt.Errorf("ensure kevlar customer: %w", err)
	}

	// Namespaces that tests call ensureToken()/queryPipeline() against.
	// Kept as a single list — if a new test adds a fixture name, append
	// here and the seeder covers it.
	fixtures := []string{
		"integration",
		"suspend-test",
		"isolation",
		"jqtest",
		"op5compute",
		"restart-data",
		"restart-resume",
	}
	for _, ns := range fixtures {
		_, err := conn.Exec(`
			INSERT INTO namespaces (customer_id, name, created_at, updated_at)
			VALUES ($1, $2, now(), now())
			ON CONFLICT (customer_id, name) DO NOTHING`,
			kevID, ns,
		)
		if err != nil {
			return fmt.Errorf("seed namespace %s: %w", ns, err)
		}
	}

	// Dev user under kevlar — generateDevJWT in onboard_test.go signs
	// with ID "dev-user", and the email-verified middleware looks the
	// user up to confirm. Seed a verified user for that ID so the
	// middleware lets the request through.
	_, err = conn.Exec(`
		INSERT INTO users (id, email, name, email_verified_at, last_active_customer_id, created_at, updated_at)
		VALUES ('dev-user', 'dev@test.local', 'Dev User', now(), $1, now(), now())
		ON CONFLICT (email) DO UPDATE SET email_verified_at = now(), last_active_customer_id = $1`,
		kevID,
	)
	if err != nil {
		return fmt.Errorf("seed dev user: %w", err)
	}
	_, err = conn.Exec(`
		INSERT INTO team_members (id, email, user_id, role, status, customer_id, accepted_at, created_at, updated_at)
		VALUES ('tm-dev-kevlar', 'dev@test.local', 'dev-user', 'owner', 'active', $1, now(), now(), now())
		ON CONFLICT (id) DO UPDATE SET customer_id = $1`,
		kevID,
	)
	if err != nil {
		return fmt.Errorf("seed dev team member: %w", err)
	}
	_, err = conn.Exec(`
		INSERT INTO users (id, email, name, email_verified_at, last_active_customer_id, created_at, updated_at)
		VALUES ('integration-user', 'it@example.com', 'Integration User', now(), $1, now(), now())
		ON CONFLICT (email) DO UPDATE SET email_verified_at = now(), last_active_customer_id = $1`,
		kevID,
	)
	if err != nil {
		return fmt.Errorf("seed integration user: %w", err)
	}
	_, err = conn.Exec(`
		INSERT INTO team_members (id, email, user_id, role, status, customer_id, accepted_at, created_at, updated_at)
		VALUES ('tm-integ-kevlar', 'it@example.com', 'integration-user', 'owner', 'active', $1, now(), now(), now())
		ON CONFLICT (id) DO UPDATE SET customer_id = $1`,
		kevID,
	)
	if err != nil {
		return fmt.Errorf("seed integration team member: %w", err)
	}

	// Dev agent token wft_dev_0000000000000000 under kevlar/kevlar.
	// Mirrors store.SeedDevAgentToken — duplicated here so the
	// integration harness owns its own preconditions even when the
	// coordinator's customer-creation flow at startup ran against a
	// previous (now-deleted) row, which cascaded the original token.
	const devToken = "wft_dev_0000000000000000"
	tokenSum := sha256.Sum256([]byte(devToken))
	tokenHash := hex.EncodeToString(tokenSum[:])
	tokenPrefix := devToken[:8]

	var devNSID int64
	if err := conn.QueryRow(`
		INSERT INTO namespaces (customer_id, name, created_at, updated_at)
		VALUES ($1, 'kevlar', now(), now())
		ON CONFLICT (customer_id, name) DO UPDATE SET updated_at = now()
		RETURNING id
	`, kevID).Scan(&devNSID); err != nil {
		return fmt.Errorf("ensure kevlar namespace: %w", err)
	}

	if _, err := conn.Exec(`
		INSERT INTO agent_tokens (
			customer_id, namespace_id, token_hash, prefix, label,
			revoked, created_at, updated_at
		)
		VALUES ($1, $2, $3, $4, 'dev-agent', false, now(), now())
		ON CONFLICT (token_hash) DO UPDATE SET
			customer_id = EXCLUDED.customer_id,
			namespace_id = EXCLUDED.namespace_id,
			revoked = false,
			updated_at = now()
	`, kevID, devNSID, tokenHash, tokenPrefix); err != nil {
		return fmt.Errorf("ensure dev agent token: %w", err)
	}

	return nil
}

// requireStackRunning aborts the test run with a helpful message when the
// docker compose stack isn't listening. Integration tests don't boot the
// stack anymore — that's an explicit prerequisite.
func requireStackRunning() {
	for i := 0; i < readinessTries; i++ {
		resp, err := http.Get(coordHealth)
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return
			}
		}
		time.Sleep(time.Second)
	}
	log.Fatalf(
		"integration: coordinator at %s is not reachable. "+
			"Start the stack first: `docker compose up -d --build` "+
			"(or use ./scripts/integration-test.sh).",
		coordHealth,
	)
}

func ensureMinioBucket() error {
	cfg, err := config.LoadDefaultConfig(context.Background(),
		config.WithRegion("us-east-1"),
		config.WithCredentialsProvider(credentials.NewStaticCredentialsProvider(MinioAccessKey, MinioSecretKey, "")),
	)
	if err != nil {
		return err
	}

	client := s3.NewFromConfig(cfg, func(o *s3.Options) {
		o.BaseEndpoint = aws.String(fmt.Sprintf("http://%s", MinioEndpoint))
		o.UsePathStyle = true
	})

	for i := 0; i < 30; i++ {
		_, err := client.ListBuckets(context.Background(), &s3.ListBucketsInput{})
		if err == nil {
			break
		}
		time.Sleep(time.Second)
	}

	client.CreateBucket(context.Background(), &s3.CreateBucketInput{
		Bucket: aws.String(ColdBucket),
	})

	return nil
}
