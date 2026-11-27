package shard

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"

	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	s3types "github.com/aws/aws-sdk-go-v2/service/s3/types"
)

// This file is the S3 side of the archive export; it moves to ext/storage/s3.

// S3ManifestStore reads and writes manifests in S3. A single PutObject is atomic: readers
// see the whole object or none of it.
type S3ManifestStore struct {
	client *s3.Client
}

// NewS3ManifestStore builds a store from the default credential chain (the
// export instance's IAM role).
func NewS3ManifestStore(ctx context.Context, region string) (*S3ManifestStore, error) {
	cfg, err := awsconfig.LoadDefaultConfig(ctx, awsconfig.WithRegion(region))
	if err != nil {
		return nil, fmt.Errorf("load aws config: %w", err)
	}
	return &S3ManifestStore{client: s3.NewFromConfig(cfg)}, nil
}

func (s *S3ManifestStore) Put(ctx context.Context, key string, m ExportManifest) error {
	bucket, objKey, err := parseS3URL(key)
	if err != nil {
		return err
	}
	b, err := json.Marshal(m)
	if err != nil {
		return err
	}
	ct := "application/json"
	_, err = s.client.PutObject(ctx, &s3.PutObjectInput{
		Bucket: &bucket, Key: &objKey, Body: bytes.NewReader(b), ContentType: &ct,
	})
	return err
}

func (s *S3ManifestStore) Get(ctx context.Context, key string) (ExportManifest, bool, error) {
	bucket, objKey, err := parseS3URL(key)
	if err != nil {
		return ExportManifest{}, false, err
	}
	out, err := s.client.GetObject(ctx, &s3.GetObjectInput{Bucket: &bucket, Key: &objKey})
	if err != nil {
		var nsk *s3types.NoSuchKey
		if errors.As(err, &nsk) {
			return ExportManifest{}, false, nil
		}
		return ExportManifest{}, false, fmt.Errorf("get manifest %s: %w", key, err)
	}
	defer out.Body.Close()
	b, err := io.ReadAll(out.Body)
	if err != nil {
		return ExportManifest{}, false, fmt.Errorf("read manifest %s: %w", key, err)
	}
	return decodeManifest(key, b)
}

// parseS3URL splits "s3://bucket/key" into its bucket and key.
func parseS3URL(u string) (bucket, key string, err error) {
	rest, ok := strings.CutPrefix(u, "s3://")
	if !ok {
		return "", "", fmt.Errorf("not an s3:// url: %q", u)
	}
	bucket, key, ok = strings.Cut(rest, "/")
	if !ok || bucket == "" || key == "" {
		return "", "", fmt.Errorf("s3 url needs a bucket and a key: %q", u)
	}
	return bucket, key, nil
}
