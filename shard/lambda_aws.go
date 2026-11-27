package shard

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	v4signer "github.com/aws/aws-sdk-go-v2/aws/signer/v4"

	"github.com/w3ft-net/weft/backends/logs/archive"
)

// This file is the AWS side of cold reads: SigV4 for a Lambda Function URL. It
// moves to ext/storage/lambda.

// LambdaReader reads an archive through the Lambda Function URL in
// Archive.Endpoint. creds, when set, SigV4-signs the request (a URL with
// AWS_IAM authorization).
func LambdaReader(creds aws.CredentialsProvider, region string) archive.Reader {
	return RemoteReader(LambdaSigner(creds, region))
}

// LambdaSigner is the RequestSigner for a Lambda Function URL: SigV4 with creds,
// or none when creds is nil (the unsigned test/localstack path).
func LambdaSigner(creds aws.CredentialsProvider, region string) RequestSigner {
	if creds == nil {
		return nil
	}
	return func(req *http.Request, body []byte) error { return SignLambdaV4(req, body, creds, region) }
}

// SignLambdaV4 SigV4-signs a Lambda Function URL request when creds are
// present (the URL uses AWS_IAM auth); a nil provider is a no-op for the
// unsigned test/localstack path. Shared by the cold-shard query client
// and the compactor client.
func SignLambdaV4(req *http.Request, body []byte, creds aws.CredentialsProvider, region string) error {
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
