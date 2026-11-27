package s3

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"

	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	awss3 "github.com/aws/aws-sdk-go-v2/service/s3"
	awss3types "github.com/aws/aws-sdk-go-v2/service/s3/types"

	"github.com/w3ft-net/weft/shard"
)

// Manifests reads and writes manifests in S3. A single PutObject is atomic: readers
// see the whole object or none of it.
type Manifests struct {
	client *awss3.Client
}

// NewManifests builds a store from the default credential chain (the
// export instance's IAM role).
func NewManifests(ctx context.Context, region string) (*Manifests, error) {
	cfg, err := awsconfig.LoadDefaultConfig(ctx, awsconfig.WithRegion(region))
	if err != nil {
		return nil, fmt.Errorf("load aws config: %w", err)
	}
	return &Manifests{client: awss3.NewFromConfig(cfg)}, nil
}

func (s *Manifests) Put(ctx context.Context, key string, m shard.ExportManifest) error {
	bucket, objKey, err := parseS3URL(key)
	if err != nil {
		return err
	}
	b, err := json.Marshal(m)
	if err != nil {
		return err
	}
	ct := "application/json"
	_, err = s.client.PutObject(ctx, &awss3.PutObjectInput{
		Bucket: &bucket, Key: &objKey, Body: bytes.NewReader(b), ContentType: &ct,
	})
	return err
}

func (s *Manifests) Get(ctx context.Context, key string) (shard.ExportManifest, bool, error) {
	bucket, objKey, err := parseS3URL(key)
	if err != nil {
		return shard.ExportManifest{}, false, err
	}
	out, err := s.client.GetObject(ctx, &awss3.GetObjectInput{Bucket: &bucket, Key: &objKey})
	if err != nil {
		var nsk *awss3types.NoSuchKey
		if errors.As(err, &nsk) {
			return shard.ExportManifest{}, false, nil
		}
		return shard.ExportManifest{}, false, fmt.Errorf("get manifest %s: %w", key, err)
	}
	defer out.Body.Close()
	b, err := io.ReadAll(out.Body)
	if err != nil {
		return shard.ExportManifest{}, false, fmt.Errorf("read manifest %s: %w", key, err)
	}
	return shard.DecodeManifest(key, b)
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
