package retention

import (
	"context"
	"os"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	weftsdk "github.com/w3ft-net/weft-go"

	"github.com/graybeardcorp/weft/logdb"
	"github.com/graybeardcorp/weft/shape"
	"github.com/graybeardcorp/weft/shard"
	"github.com/graybeardcorp/weft/store"
)

// Retention is hosted weft's logdb.Retention: it records each archive's
// per-customer stats and runs the daily janitor over them.
type Retention struct {
	region string
	db     *store.Store
}

// New is the retention for a coordinator whose shard rows live in db. region
// signs the compactor Function URL calls.
func New(db *store.Store, region string) *Retention {
	return &Retention{region: region, db: db}
}

var _ logdb.Retention = (*Retention)(nil)

// ArchiveRecorded stores an archive's stats for the janitor to classify by.
func (r *Retention) ArchiveRecorded(shardID string, stats []shard.CustomerStat) error {
	return Store{logdb.ShardStore{Store: r.db}}.RecordArchiveStats(shardID, stats)
}

// Start wires and starts the daily janitor. With COMPACTOR_FUNCTION_URL unset
// it falls back to an in-process compactor (dev).
func (r *Retention) Start(ctx context.Context, prov *logdb.Provisioner) {
	if prov == nil {
		return
	}
	var compactor shard.Compactor
	if url := os.Getenv("COMPACTOR_FUNCTION_URL"); url != "" {
		lc := shard.NewLambdaCompactor(url, lambdaSigningCreds(ctx, r.region), r.region)
		lc.Layout = shape.BuildLayout()
		compactor = lc
		weftsdk.Infof(ctx, "janitor: compactor Lambda at %s", url)
	} else {
		compactor = shard.LocalCompactor{S3: shard.S3ConfigFromEnv(), Layout: shape.BuildLayout()}
		weftsdk.Infof(ctx, "janitor: in-process compactor (COMPACTOR_FUNCTION_URL unset)")
	}
	NewProvisionerJanitor(prov, compactor, 24*time.Hour).Start(ctx)
	weftsdk.Infof(ctx, "coordinator: retention janitor started (daily)")
}

// lambdaSigningCreds loads the credential provider used to SigV4-sign
// compactor Function URL calls. Returns nil (unsigned) on failure — the
// invoke then fails loudly at call time rather than here at boot.
func lambdaSigningCreds(ctx context.Context, region string) aws.CredentialsProvider {
	cfg, err := awsconfig.LoadDefaultConfig(ctx, awsconfig.WithRegion(region))
	if err != nil {
		weftsdk.Infof(ctx, "janitor: aws creds for compactor signing: %v", err)
		return nil
	}
	return cfg.Credentials
}
