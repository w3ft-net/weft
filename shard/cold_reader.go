package shard

import (
	"errors"

	"github.com/aws/aws-sdk-go-v2/aws"

	"github.com/w3ft-net/weft/backends/logs/archive"
	"github.com/w3ft-net/weft/backends/logs/archive/parquet"
	"github.com/w3ft-net/weft/backends/logs/dialect"
)

// The archive readers of the open build, which read the Parquet archives
// DuckDB writes. LambdaReader is the production one; LocalReader runs DuckDB in
// the reading process, which tests use.

// LambdaReader reads an archive by sending the query to the Lambda Function URL
// in Archive.Endpoint, where DuckDB runs it over the Parquet. creds, when set,
// SigV4-signs the request (a URL with AWS_IAM authorization).
func LambdaReader(creds aws.CredentialsProvider, region string) archive.Reader {
	return coldReader{remote: true, creds: creds, region: region}
}

// LocalReader reads an archive with DuckDB in this process, from a local path or,
// with s3, from S3 through DuckDB's httpfs.
func LocalReader(s3 *S3Config) archive.Reader { return coldReader{s3: s3} }

type coldReader struct {
	remote bool
	creds  aws.CredentialsProvider
	region string
	s3     *S3Config
}

func (coldReader) Reads() []string {
	return []string{parquet.FormatFile, parquet.FormatTime, parquet.FormatKeyed, parquet.FormatSized}
}

func (coldReader) Dialect() dialect.Dialect { return dialect.DuckDB }

func (r coldReader) Open(a archive.Archive) (archive.Handle, error) {
	url := ""
	if r.remote {
		if a.Endpoint == "" {
			return nil, errors.New("archive " + a.ID + " has no Lambda function URL to read it through")
		}
		url = a.Endpoint
	}
	cs, err := NewColdShard(a.ID, a.Services, a.Path, url, r.s3, ColdLayout(a.Layout))
	if err != nil {
		return nil, err // not cs: a nil *ColdShard in an archive.Handle is not nil
	}
	if r.remote && r.creds != nil {
		cs.WithAWSAuth(r.creds, r.region)
	}
	return cs, nil
}
