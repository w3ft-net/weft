package poller

import (
	"context"
	"fmt"
	"log"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/graybeardcorp/weft/ingest"
	pb "github.com/graybeardcorp/weft/proto/weftpb"
	"github.com/graybeardcorp/weft/shard"
	"github.com/graybeardcorp/weft/store"
)

// ShardProvider routes rows to the app-specific INGESTING shard.
// No cross-app fallback — that would land a customer's logs on
// shards bound to other customers' namespaces. Rows arriving before
// the bucket-add reconciles get dropped (the poller path is best-
// effort; ingest stream's DLQ scanner re-ingests its own).
type ShardProvider interface {
	IngestingShard(namespaceID uint, service string) shard.Shard
}

// Poller polls CloudWatch log groups and ingests into shards.
type Poller struct {
	store    *store.Store
	provider ShardProvider
	// holder identifies this coordinator process for the per-log-group
	// lease that guards polling once the coordinator ASG scales past 1
	// instance — see docs/plans/coordinator-horizontal-scaling.md.
	holder string
}

// New creates a new CloudWatch poller. holder identifies this process
// for lease ownership; it only needs to be unique per running
// coordinator, not stable across restarts.
func New(store *store.Store, provider ShardProvider, holder string) *Poller {
	return &Poller{store: store, provider: provider, holder: holder}
}

// Start begins polling all enabled log groups in background goroutines.
func (p *Poller) Start(ctx context.Context) {
	var logGroups []store.LogGroupWithAWSAccount
	err := p.store.DB().
		Table("log_groups").
		Select(`log_groups.*,
		        organizations.name as customer_name,
		        namespaces.name as namespace_name,
		        aws_accounts.role_arn,
		        aws_accounts.external_id,
		        aws_accounts.account_id`).
		Joins("JOIN namespaces ON log_groups.namespace_id = namespaces.id").
		Joins("JOIN organizations ON namespaces.org_id = organizations.id").
		Joins("JOIN aws_accounts ON log_groups.aws_account_id = aws_accounts.id").
		Where("log_groups.enabled = ? AND log_groups.source_type = ? AND log_groups.status = ?",
			true, store.SourceTypeCloudWatch, store.StatusConfigured).
		Find(&logGroups).Error
	if err != nil {
		log.Printf("poller: failed to list log groups: %v", err)
		return
	}

	started := 0
	for _, lg := range logGroups {
		// Only CloudWatch-sourced log groups are pollable. weftd rows
		// get their data via IngestStream (push) — spinning a ticker
		// for them would just do nothing, and historically a 0s
		// interval slipped through and panic'd NewTicker.
		if lg.SourceType != store.SourceTypeCloudWatch {
			continue
		}
		if d, err := time.ParseDuration(lg.PollInterval); err != nil || d <= 0 {
			log.Printf("poller[%s]: skipping — invalid poll_interval %q", lg.Service, lg.PollInterval)
			continue
		}
		go p.pollLoop(ctx, lg)
		started++
	}
	log.Printf("poller: started %d CloudWatch pollers (of %d log groups)", started, len(logGroups))
}

func (p *Poller) pollLoop(ctx context.Context, lg store.LogGroupWithAWSAccount) {
	// Only one coordinator instance may poll a given log group at a
	// time — two instances both fetching from the same CloudWatch
	// starting token would double-ingest every event. If we can't get
	// the lease, some other instance already owns this log group;
	// nothing to reconcile later since Start() only runs once at boot
	// (matches its existing no-reconcile-loop limitation).
	leaseName := fmt.Sprintf("poller:%d", lg.ID)
	leaseCtx, ok := store.AcquireAndHold(ctx, p.store, leaseName, p.holder)
	if !ok {
		log.Printf("poller[%s]: lease held by another coordinator instance, not polling", lg.Service)
		return
	}

	interval, err := time.ParseDuration(lg.PollInterval)
	if err != nil {
		interval = 30 * time.Second
	}

	log.Printf("poller[%s]: polling %s (account %s) every %s", lg.Service, lg.LogGroupName, lg.AccountID, interval)

	p.poll(leaseCtx, lg)

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-leaseCtx.Done():
			return
		case <-ticker.C:
			p.poll(leaseCtx, lg)
		}
	}
}

func (p *Poller) poll(ctx context.Context, lg store.LogGroupWithAWSAccount) {
	// Parse schema first — no point pulling events we can't parse.
	var schema ingest.Schema
	if err := yaml.Unmarshal([]byte(lg.SchemaYAML), &schema); err != nil {
		log.Printf("poller[%s]: parse schema: %v", lg.Service, err)
		return
	}
	schema.Service = lg.Service
	schema.Customer = lg.CustomerName

	parser, err := ingest.NewParser(&schema)
	if err != nil {
		log.Printf("poller[%s]: create parser: %v", lg.Service, err)
		return
	}

	// Pick up where we left off. First poll or parse failure → last hour
	// (the Fetch helper defaults StartTime=0 to "1h ago").
	var startTime int64
	if lg.LastPollAt != "" {
		if t, err := time.Parse(time.RFC3339, lg.LastPollAt); err == nil {
			startTime = t.UnixMilli()
		}
	}

	result, err := ingest.FetchCloudWatch(ctx, ingest.CloudWatchFetchOpts{
		Region:        lg.Region,
		RoleARN:       lg.RoleARN,
		ExternalID:    lg.ExternalID,
		LogGroup:      lg.LogGroupName,
		StartTime:     startTime,
		NextToken:     lg.LastToken,
		SessionSuffix: "poller-" + lg.Service,
	})
	if err != nil {
		log.Printf("poller[%s]: fetch: %v", lg.Service, err)
		return
	}

	var allRows []*pb.LogRow
	for _, event := range result.Events {
		row, err := parser.ParseLine(event.Message, event.Timestamp)
		if err != nil {
			continue
		}
		// Stamp the routing/storage coordinates the parser can't know.
		// The parser sets Service from the schema (overridden to the log
		// group's service); the poller owns namespace and kind.
		// CloudWatch sources are single-stream, so kind is always "main".
		row.Namespace = lg.NamespaceName
		row.Kind = "main"
		allRows = append(allRows, row)
	}

	if len(allRows) == 0 {
		return
	}

	// Route to the INGESTING shard serving this (namespace, service).
	// No cross-tenant fallback — see ShardProvider's doc. lg.Service is the
	// bare service; lg.NamespaceID scopes it to the customer. If no
	// shard serves it yet, skip; the next poll picks up after the
	// reconciler stamps it.
	sh := p.provider.IngestingShard(lg.NamespaceID, lg.Service)
	if sh == nil {
		log.Printf("poller[%s]: no shard serves this service yet — skipping poll", lg.Service)
		return
	}

	// Ingest in batches to stay under gRPC message size limit.
	const batchSize = 500
	ingested := 0
	for i := 0; i < len(allRows); i += batchSize {
		end := i + batchSize
		if end > len(allRows) {
			end = len(allRows)
		}
		if err := sh.Ingest(allRows[i:end], nil); err != nil {
			log.Printf("poller[%s]: ingest batch failed: %v", lg.Service, err)
			continue
		}
		ingested += end - i
	}

	if ingested > 0 {
		log.Printf("poller[%s]: ingested %d rows", lg.Service, ingested)
		now := time.Now().UTC().Format(time.RFC3339)
		p.store.DB().Model(&store.LogGroup{}).Where("id = ?", lg.ID).
			Updates(map[string]any{"last_poll_at": now, "last_token": result.NextToken})
	}
}
