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
// Returns nil if WEFT_S3_ACCESS_KEY is not set.
//
//	WEFT_S3_ENDPOINT   — S3 endpoint (empty for AWS default)
//	WEFT_S3_ACCESS_KEY — access key ID
//	WEFT_S3_SECRET_KEY — secret access key
//	WEFT_S3_REGION     — AWS region (default: us-east-1)
//	WEFT_S3_USE_SSL    — "true" for HTTPS (default: "false")
func S3ConfigFromEnv() *S3Config {
	ak := os.Getenv("WEFT_S3_ACCESS_KEY")
	if ak == "" {
		return nil
	}
	region := os.Getenv("WEFT_S3_REGION")
	if region == "" {
		region = "us-east-1"
	}
	return &S3Config{
		Endpoint:  os.Getenv("WEFT_S3_ENDPOINT"),
		AccessKey: ak,
		SecretKey: os.Getenv("WEFT_S3_SECRET_KEY"),
		Region:    region,
		UseSSL:    os.Getenv("WEFT_S3_USE_SSL") == "true",
	}
}

// ConfigureDuckDB sets up httpfs/S3 on a DuckDB connection.
func (c *S3Config) ConfigureDuckDB(db *sql.DB) error {
	if _, err := db.Exec("INSTALL httpfs; LOAD httpfs;"); err != nil {
		return fmt.Errorf("load httpfs: %w", err)
	}
	ssl := "false"
	if c.UseSSL {
		ssl = "true"
	}
	stmts := []string{
		fmt.Sprintf("SET s3_access_key_id='%s'", c.AccessKey),
		fmt.Sprintf("SET s3_secret_access_key='%s'", c.SecretKey),
		fmt.Sprintf("SET s3_use_ssl=%s", ssl),
		"SET s3_url_style='path'",
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
