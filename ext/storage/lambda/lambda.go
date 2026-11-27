// Package lambda reads archived shards through a Lambda Function URL: DuckDB runs
// the query over the Parquet inside the function, and requests are SigV4-signed
// when the URL uses AWS_IAM authorization.
package lambda

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	v4signer "github.com/aws/aws-sdk-go-v2/aws/signer/v4"

	"github.com/w3ft-net/weft/backends/logs/archive"
	"github.com/w3ft-net/weft/shard"
)

// ReaderName is the name configuration gives this reader (-archive-reader-default).
const ReaderName = "duckdb-lambda"

// Reader reads an archive through the Lambda Function URL in
// Archive.Endpoint. creds, when set, SigV4-signs the request (a URL with
// AWS_IAM authorization).
func Reader(creds aws.CredentialsProvider, region string) archive.Reader {
	return shard.RemoteReader(Signer(creds, region))
}

// Signer is the shard.RequestSigner for a Lambda Function URL: SigV4 with creds,
// or none when creds is nil (the unsigned test/localstack path).
func Signer(creds aws.CredentialsProvider, region string) shard.RequestSigner {
	if creds == nil {
		return nil
	}
	return func(req *http.Request, body []byte) error { return Sign(req, body, creds, region) }
}

// Sign SigV4-signs a Lambda Function URL request when creds are
// present (the URL uses AWS_IAM auth); a nil provider is a no-op for the
// unsigned test/localstack path. Shared by the cold-shard query client
// and the compactor client.
func Sign(req *http.Request, body []byte, creds aws.CredentialsProvider, region string) error {
	if creds == nil {
		return nil
	}
	hash := sha256.Sum256(body)
	payloadHash := hex.EncodeToString(hash[:])
	c, err := creds.Retrieve(req.Context())
	if err != nil {
		return fmt.Errorf("retrieve credentials: %w", err)
	}
	return v4signer.NewSigner().SignHTTP(req.Context(), c, req, payloadHash, "lambda", region, time.Now())
}
