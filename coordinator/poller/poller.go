package poller

import (
	"context"
	"log"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials/stscreds"
	"github.com/aws/aws-sdk-go-v2/service/cloudwatchlogs"
	"github.com/aws/aws-sdk-go-v2/service/sts"
	"gopkg.in/yaml.v3"

	"github.com/kevlarcon/weft/configstore"
	"github.com/kevlarcon/weft/ingest"
	pb "github.com/kevlarcon/weft/proto/weftpb"
	"github.com/kevlarcon/weft/shard"
)

// ShardProvider returns the current list of available shards.
type ShardProvider interface {
	GetShards() []shard.Shard
}

// Poller polls CloudWatch log groups and ingests into shards.
type Poller struct {
	store    *configstore.Store
	provider ShardProvider
}

// New creates a new CloudWatch poller.
func New(store *configstore.Store, provider ShardProvider) *Poller {
	return &Poller{store: store, provider: provider}
}

// Start begins polling all enabled log groups in background goroutines.
func (p *Poller) Start(ctx context.Context) {
	logGroups, err := p.store.EnabledLogGroups()
	if err != nil {
		log.Printf("poller: failed to list log groups: %v", err)
		return
	}

	for _, lg := range logGroups {
		go p.pollLoop(ctx, lg)
	}
	log.Printf("poller: started %d log group pollers", len(logGroups))
}

func (p *Poller) pollLoop(ctx context.Context, lg configstore.LogGroupWithAccount) {
	interval, err := time.ParseDuration(lg.PollInterval)
	if err != nil {
		interval = 30 * time.Second
	}

	log.Printf("poller[%s]: polling %s (account %s) every %s", lg.App, lg.LogGroupName, lg.AWSAccount, interval)

	p.poll(ctx, lg)

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			p.poll(ctx, lg)
		}
	}
}

func (p *Poller) poll(ctx context.Context, lg configstore.LogGroupWithAccount) {
	// Load base AWS config.
	cfg, err := awsconfig.LoadDefaultConfig(ctx, awsconfig.WithRegion(lg.Region))
	if err != nil {
		log.Printf("poller[%s]: aws config: %v", lg.App, err)
		return
	}

	// Assume cross-account role to access the customer's CloudWatch logs.
	stsClient := sts.NewFromConfig(cfg)
	creds := stscreds.NewAssumeRoleProvider(stsClient, lg.RoleARN, func(o *stscreds.AssumeRoleOptions) {
		if lg.ExternalID != "" {
			o.ExternalID = aws.String(lg.ExternalID)
		}
		o.RoleSessionName = "weft-poller-" + lg.App
	})
	assumedCfg := cfg.Copy()
	assumedCfg.Credentials = aws.NewCredentialsCache(creds)

	client := cloudwatchlogs.NewFromConfig(assumedCfg)

	// Determine start time.
	var startTime int64
	if lg.LastPollAt != "" {
		t, err := time.Parse(time.RFC3339, lg.LastPollAt)
		if err == nil {
			startTime = t.UnixMilli()
		}
	}
	if startTime == 0 {
		startTime = time.Now().Add(-1 * time.Hour).UnixMilli()
	}
	endTime := time.Now().UnixMilli()

	// Parse schema.
	var schema ingest.Schema
	if err := yaml.Unmarshal([]byte(lg.SchemaYAML), &schema); err != nil {
		log.Printf("poller[%s]: parse schema: %v", lg.App, err)
		return
	}
	schema.App = lg.App
	schema.Customer = lg.CustomerName

	parser, err := ingest.NewParser(&schema)
	if err != nil {
		log.Printf("poller[%s]: create parser: %v", lg.App, err)
		return
	}

	// Fetch logs.
	var allRows []*pb.LogRow
	var nextToken *string
	if lg.LastToken != "" {
		nextToken = aws.String(lg.LastToken)
	}

	for {
		input := &cloudwatchlogs.FilterLogEventsInput{
			LogGroupName: aws.String(lg.LogGroupName),
			StartTime:    aws.Int64(startTime),
			EndTime:      aws.Int64(endTime),
			NextToken:    nextToken,
		}

		result, err := client.FilterLogEvents(ctx, input)
		if err != nil {
			log.Printf("poller[%s]: FilterLogEvents: %v", lg.App, err)
			break
		}

		for _, event := range result.Events {
			msg := aws.ToString(event.Message)
			msg = strings.TrimSpace(msg)
			if msg == "" {
				continue
			}

			ts := time.UnixMilli(aws.ToInt64(event.Timestamp))
			row, err := parser.ParseLine(msg, ts)
			if err != nil {
				continue
			}
			allRows = append(allRows, row)
		}

		if result.NextToken == nil {
			break
		}
		nextToken = result.NextToken
	}

	if len(allRows) == 0 {
		return
	}

	// Ingest into shards in batches (to stay under gRPC message size limit).
	const batchSize = 500
	ingested := 0
	for i := 0; i < len(allRows); i += batchSize {
		end := i + batchSize
		if end > len(allRows) {
			end = len(allRows)
		}
		batch := allRows[i:end]

		ok := false
		for _, s := range p.provider.GetShards() {
			if err := s.Ingest(batch); err != nil {
				continue
			}
			ok = true
			break
		}
		if !ok {
			log.Printf("poller[%s]: failed to ingest batch of %d rows — all shards rejected", lg.App, len(batch))
			continue
		}
		ingested += len(batch)
	}

	if ingested > 0 {
		log.Printf("poller[%s]: ingested %d rows", lg.App, ingested)
		now := time.Now().UTC().Format(time.RFC3339)
		token := ""
		if nextToken != nil {
			token = *nextToken
		}
		p.store.UpdatePollState(lg.App, now, token)
	}
}
