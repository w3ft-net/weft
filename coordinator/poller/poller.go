package poller

import (
	"context"
	"log"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/cloudwatchlogs"

	"gopkg.in/yaml.v3"

	"github.com/kevlarcon/weft/configstore"
	"github.com/kevlarcon/weft/ingest"
	"github.com/kevlarcon/weft/shard"
)

// Poller polls CloudWatch log groups and ingests into shards.
type Poller struct {
	store  *configstore.Store
	shards []shard.Shard
}

// New creates a new CloudWatch poller.
func New(store *configstore.Store, shards []shard.Shard) *Poller {
	return &Poller{store: store, shards: shards}
}

// Start begins polling all enabled sources in background goroutines.
func (p *Poller) Start(ctx context.Context) {
	sources, err := p.store.EnabledSources()
	if err != nil {
		log.Printf("poller: failed to list sources: %v", err)
		return
	}

	for _, src := range sources {
		go p.pollLoop(ctx, src)
	}
	log.Printf("poller: started %d source pollers", len(sources))
}

func (p *Poller) pollLoop(ctx context.Context, src configstore.Source) {
	interval, err := time.ParseDuration(src.PollInterval)
	if err != nil {
		interval = 30 * time.Second
	}

	log.Printf("poller[%s]: polling %s every %s", src.App, src.LogGroup, interval)

	// Poll immediately on start, then on interval.
	p.poll(ctx, src)

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			p.poll(ctx, src)
		}
	}
}

func (p *Poller) poll(ctx context.Context, src configstore.Source) {
	cfg, err := awsconfig.LoadDefaultConfig(ctx, awsconfig.WithRegion(src.Region))
	if err != nil {
		log.Printf("poller[%s]: aws config: %v", src.App, err)
		return
	}
	client := cloudwatchlogs.NewFromConfig(cfg)

	// Determine start time: last poll or 1 hour ago.
	var startTime int64
	if src.LastPollAt != "" {
		t, err := time.Parse(time.RFC3339, src.LastPollAt)
		if err == nil {
			startTime = t.UnixMilli()
		}
	}
	if startTime == 0 {
		startTime = time.Now().Add(-1 * time.Hour).UnixMilli()
	}
	endTime := time.Now().UnixMilli()

	// Parse schema from stored YAML.
	schema, err := parseSchemaYAML(src.SchemaYAML)
	if err != nil {
		log.Printf("poller[%s]: parse schema: %v", src.App, err)
		return
	}
	schema.App = src.App
	schema.Customer = src.Customer

	parser, err := ingest.NewParser(schema)
	if err != nil {
		log.Printf("poller[%s]: create parser: %v", src.App, err)
		return
	}

	// Fetch logs from CloudWatch.
	var allRows []shard.LogRow
	var nextToken *string
	if src.LastToken != "" {
		nextToken = aws.String(src.LastToken)
	}

	for {
		input := &cloudwatchlogs.FilterLogEventsInput{
			LogGroupName: aws.String(src.LogGroup),
			StartTime:    aws.Int64(startTime),
			EndTime:      aws.Int64(endTime),
			NextToken:    nextToken,
		}

		result, err := client.FilterLogEvents(ctx, input)
		if err != nil {
			log.Printf("poller[%s]: FilterLogEvents: %v", src.App, err)
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
			allRows = append(allRows, *row)
		}

		if result.NextToken == nil {
			break
		}
		nextToken = result.NextToken
	}

	if len(allRows) == 0 {
		return
	}

	// Find the first available shard and ingest.
	for _, s := range p.shards {
		if err := s.Ingest(allRows); err != nil {
			log.Printf("poller[%s]: ingest to shard %s: %v", src.App, s.ID(), err)
			continue
		}
		log.Printf("poller[%s]: ingested %d rows into shard %s", src.App, len(allRows), s.ID())

		// Update poll state.
		now := time.Now().UTC().Format(time.RFC3339)
		token := ""
		if nextToken != nil {
			token = *nextToken
		}
		p.store.UpdatePollState(src.App, now, token)
		return
	}

	log.Printf("poller[%s]: failed to ingest %d rows — all shards rejected", src.App, len(allRows))
}

func parseSchemaYAML(yamlStr string) (*ingest.Schema, error) {
	var schema ingest.Schema
	if err := yaml.Unmarshal([]byte(yamlStr), &schema); err != nil {
		return nil, err
	}
	return &schema, nil
}
