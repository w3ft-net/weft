package retention

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"

	"github.com/graybeardcorp/weft/shape"
	"github.com/graybeardcorp/weft/shard"
)

// Compactor performs the janitor's heavy parquet work — rewriting an
// archived file to drop over-retained rows, and capturing per-customer
// stats from a file. Two implementations: LocalCompactor runs DuckDB
// in-process; LambdaCompactor offloads to the compactor Lambda so the
// coordinator stays free of DuckDB + S3 I/O (the same reason cold reads
// route to a Lambda in prod). See docs/plans/retention-janitor.md.
type Compactor interface {
	Rewrite(src, dst string, cutoffs []CustomerCutoff) (CompactResult, error)
	CaptureStats(src string) ([]shard.CustomerStat, error)
}

// CompactRequest is the compactor Lambda's JSON request body. Op selects
// the operation; Dst/Cutoffs are only read for "rewrite".
type CompactRequest struct {
	Op      string           `json:"op"` // "rewrite" | "capture"
	Src     string           `json:"src"`
	Dst     string           `json:"dst,omitempty"`
	Cutoffs []CustomerCutoff `json:"cutoffs,omitempty"`
	// Layout is the partition layout the archive was written in. Absent from
	// an older caller: the compactor then reads the legacy layout.
	Layout shape.Layout `json:"layout,omitzero"`
}

const (
	CompactOpRewrite = "rewrite"
	CompactOpCapture = "capture"
)

// LocalCompactor runs the work in-process. S3 is nil for local-filesystem
// parquet (dev) and set for s3:// paths. Layout is the partition layout the
// archives were written in; zero means the legacy one.
type LocalCompactor struct {
	S3     *shard.S3Config
	Layout shape.Layout
}

func (c LocalCompactor) Rewrite(src, dst string, cutoffs []CustomerCutoff) (CompactResult, error) {
	return RewriteRetention(src, dst, cutoffs, c.Layout.OrLegacy(), c.S3)
}

func (c LocalCompactor) CaptureStats(src string) ([]shard.CustomerStat, error) {
	return CaptureStats(src, c.Layout.OrLegacy(), c.S3)
}

// LambdaCompactor invokes the compactor Lambda over its Function URL,
// SigV4-signing when creds are set (the URL uses AWS_IAM auth).
type LambdaCompactor struct {
	url    string
	client *http.Client
	creds  aws.CredentialsProvider
	region string
	// Layout is the partition layout the archives were written in, sent with
	// every request; zero means the legacy one.
	Layout shape.Layout
}

// NewLambdaCompactor builds a Lambda-backed compactor. Pass nil creds for
// the unsigned test/localstack path. The client timeout spans the
// Lambda's max execution so a large rewrite isn't cut off.
func NewLambdaCompactor(url string, creds aws.CredentialsProvider, region string) *LambdaCompactor {
	return &LambdaCompactor{
		url:    url,
		client: &http.Client{Timeout: 15 * time.Minute},
		creds:  creds,
		region: region,
	}
}

func (c *LambdaCompactor) Rewrite(src, dst string, cutoffs []CustomerCutoff) (CompactResult, error) {
	return c.invoke(CompactRequest{Op: CompactOpRewrite, Src: src, Dst: dst, Cutoffs: cutoffs, Layout: c.Layout})
}

func (c *LambdaCompactor) CaptureStats(src string) ([]shard.CustomerStat, error) {
	res, err := c.invoke(CompactRequest{Op: CompactOpCapture, Src: src, Layout: c.Layout})
	return res.Stats, err
}

func (c *LambdaCompactor) invoke(reqBody CompactRequest) (CompactResult, error) {
	body, err := json.Marshal(reqBody)
	if err != nil {
		return CompactResult{}, fmt.Errorf("marshal compact request: %w", err)
	}
	req, err := http.NewRequest(http.MethodPost, c.url, bytes.NewReader(body))
	if err != nil {
		return CompactResult{}, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if err := shard.SignLambdaV4(req, body, c.creds, c.region); err != nil {
		return CompactResult{}, fmt.Errorf("sign request: %w", err)
	}
	resp, err := c.client.Do(req)
	if err != nil {
		return CompactResult{}, fmt.Errorf("call compactor lambda: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		return CompactResult{}, fmt.Errorf("compactor lambda returned %d: %s", resp.StatusCode, b)
	}
	var res CompactResult
	if err := json.NewDecoder(resp.Body).Decode(&res); err != nil {
		return CompactResult{}, fmt.Errorf("decode compact response: %w", err)
	}
	return res, nil
}
