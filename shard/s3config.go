package shard

import (
	"database/sql"
	"fmt"
	"os"
)

// S3Config holds S3/MinIO connection settings.
// Used by ColdShard (reads) and ExportToS3Parquet (writes).
type S3Config struct {
	Endpoint  string // e.g., "localhost:9050" for MinIO, "" for AWS
	AccessKey string
	SecretKey string
	Region    string
	UseSSL    bool
}

// S3ConfigFromEnv loads S3 configuration from environment variables.
// Returns a config if either S3_ACCESS_KEY or S3_REGION is set.
// Returns nil if neither is set (meaning no S3 — use local filesystem).
//
//	S3_ENDPOINT   — S3 endpoint (empty for AWS default)
//	S3_ACCESS_KEY — access key ID (empty = use IAM/instance credentials)
//	S3_SECRET_KEY — secret access key
//	S3_REGION     — AWS region (default: us-east-1)
//	S3_USE_SSL    — "true" for HTTPS (default: "false")
//
// Three modes:
//   - MinIO (tests): all vars set → explicit credentials + custom endpoint
//   - Lambda/EC2 (prod): only S3_REGION set → DuckDB uses IAM credentials
//   - Local dev: nothing set → returns nil → local filesystem only
func S3ConfigFromEnv() *S3Config {
	ak := os.Getenv("S3_ACCESS_KEY")
	region := os.Getenv("S3_REGION")

	if ak == "" && region == "" {
		return nil
	}

	if region == "" {
		region = "us-east-1"
	}
	return &S3Config{
		Endpoint:  os.Getenv("S3_ENDPOINT"),
		AccessKey: ak,
		SecretKey: os.Getenv("S3_SECRET_KEY"),
		Region:    region,
		UseSSL:    os.Getenv("S3_USE_SSL") == "true",
	}
}

// ConfigureDuckDB sets up httpfs/S3 on a DuckDB connection.
// When AccessKey is empty, skips credential statements — DuckDB will use
// the default credential chain (IAM role on EC2/Lambda).
func (c *S3Config) ConfigureDuckDB(db *sql.DB) error {
	// DuckDB SET/INSTALL/SECRET state is connection-local, but database/sql
	// pools connections — so config could land on a different connection
	// than the query that needs it. Pin the pool to one connection so all
	// configuration and subsequent queries share the same DuckDB session.
	db.SetMaxOpenConns(1)

	// The Lambda execution environment has no HOME, so DuckDB can't derive
	// its extension directory and extension autoloading (httpfs, and aws
	// for the S3 credential chain) aborts with "Can't find the home
	// directory". DuckDB resolves this from $HOME at autoload time, so a
	// SQL `SET home_directory` doesn't reach the autoloader — set the env
	// itself, but only when absent so EC2/local are untouched.
	if os.Getenv("HOME") == "" {
		os.Setenv("HOME", os.TempDir())
	}
	if _, err := db.Exec("INSTALL httpfs; LOAD httpfs;"); err != nil {
		return fmt.Errorf("load httpfs: %w", err)
	}
	ssl := "false"
	if c.UseSSL {
		ssl = "true"
	}
	stmts := []string{
		fmt.Sprintf("SET s3_use_ssl=%s", ssl),
		"SET s3_url_style='path'",
	}
	if c.AccessKey != "" {
		// Explicit credentials (MinIO, tests).
		stmts = append(stmts,
			fmt.Sprintf("SET s3_access_key_id='%s'", c.AccessKey),
			fmt.Sprintf("SET s3_secret_access_key='%s'", c.SecretKey),
		)
	} else {
		// IAM credential chain (EC2 instance metadata, env vars, etc.).
		stmts = append(stmts,
			"CREATE SECRET IF NOT EXISTS (TYPE S3, PROVIDER CREDENTIAL_CHAIN)",
		)
	}
	if c.Endpoint != "" {
		stmts = append(stmts, fmt.Sprintf("SET s3_endpoint='%s'", c.Endpoint))
	}
	if c.Region != "" {
		stmts = append(stmts, fmt.Sprintf("SET s3_region='%s'", c.Region))
	}
	for _, stmt := range stmts {
		if _, err := db.Exec(stmt); err != nil {
			return fmt.Errorf("s3 config: %w", err)
		}
	}
	return nil
}
