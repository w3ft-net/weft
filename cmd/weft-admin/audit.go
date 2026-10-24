package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"strconv"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"

	"github.com/graybeardcorp/weft/internal/fleetaudit"
	pb "github.com/graybeardcorp/weft/proto/weftpb"
)

// auditCmd checks the fleet invariants of docs/plans/shard-seamless-upgrade-testing.md
// against the coordinator's database and EC2, and exits 1 on any violation.
// -save records the ingestion_errors count; -baseline fails if it has grown
// since (the DLQ and ingest error counts must not move during a test).
func auditCmd(args []string) {
	fs := flag.NewFlagSet("audit", flag.ExitOnError)
	coord := fs.String("coord", "127.0.0.1:9022", "coordinator WeftAdmin gRPC address (localhost-only; SharedToken auth)")
	save := fs.String("save", "", "write the current ingestion_errors count to this file")
	baseline := fs.String("baseline", "", "fail if ingestion_errors has grown past the count saved in this file")
	if err := fs.Parse(args); err != nil {
		os.Exit(2)
	}

	ctx := context.Background()
	client, closeConn, err := newAdminClient(*coord)
	if err != nil {
		log.Fatal(err)
	}
	defer closeConn()
	db, err := client.AuditFleet(adminCtx(ctx), &pb.AuditFleetRequest{})
	if err != nil {
		log.Fatalf("audit fleet: %v", err)
	}
	cfg, err := loadAWSConfig(ctx)
	if err != nil {
		log.Fatalf("load aws config: %v", err)
	}
	instances, volumes, err := describeFleetEC2(ctx, ec2.NewFromConfig(cfg))
	if err != nil {
		log.Fatal(err)
	}

	view := fleetaudit.FleetView{Instances: instances, Volumes: volumes}
	for _, sh := range db.Shards {
		view.Shards = append(view.Shards, fleetaudit.AuditShard{
			ID: sh.Id, State: sh.State, InstanceID: sh.InstanceId, VolumeID: sh.VolumeId, UpgradePhase: sh.UpgradePhase,
		})
	}
	for _, sp := range db.Spares {
		view.Spares = append(view.Spares, fleetaudit.AuditSpare{InstanceID: sp.InstanceId, Claimed: sp.Claimed})
	}
	violations := fleetaudit.AuditFleet(view)

	if *baseline != "" {
		raw, err := os.ReadFile(*baseline)
		if err != nil {
			log.Fatalf("read baseline: %v", err)
		}
		was, err := strconv.ParseInt(strings.TrimSpace(string(raw)), 10, 64)
		if err != nil {
			log.Fatalf("baseline %s: %v", *baseline, err)
		}
		if db.IngestionErrors > was {
			violations = append(violations, fmt.Sprintf("ingestion_errors grew %d -> %d (DLQ'd or failed batches)", was, db.IngestionErrors))
		}
	}
	if *save != "" {
		if err := os.WriteFile(*save, []byte(strconv.FormatInt(db.IngestionErrors, 10)+"\n"), 0o644); err != nil {
			log.Fatalf("save baseline: %v", err)
		}
	}

	fmt.Printf("%d shards, %d spares, %d instances, %d volumes, ingestion_errors=%d\n",
		len(view.Shards), len(view.Spares), len(instances), len(volumes), db.IngestionErrors)
	if len(violations) == 0 {
		fmt.Println("audit: healthy")
		return
	}
	for _, v := range violations {
		fmt.Println("VIOLATION:", v)
	}
	os.Exit(1)
}

// describeFleetEC2 lists the weft shard-role instances and every shard-tagged
// volume, flattened to what the audit compares.
func describeFleetEC2(ctx context.Context, api *ec2.Client) ([]fleetaudit.AuditInstance, []fleetaudit.AuditVolume, error) {
	tag := func(tags []ec2types.Tag, key string) string {
		for _, t := range tags {
			if aws.ToString(t.Key) == key {
				return aws.ToString(t.Value)
			}
		}
		return ""
	}

	var instances []fleetaudit.AuditInstance
	ip := ec2.NewDescribeInstancesPaginator(api, &ec2.DescribeInstancesInput{Filters: []ec2types.Filter{
		{Name: aws.String("tag:app"), Values: []string{appName()}},
		{Name: aws.String("tag:Role"), Values: []string{"shard"}},
		{Name: aws.String("instance-state-name"), Values: []string{"pending", "running", "stopping", "stopped"}},
	}})
	for ip.HasMorePages() {
		page, err := ip.NextPage(ctx)
		if err != nil {
			return nil, nil, fmt.Errorf("describe instances: %w", err)
		}
		for _, r := range page.Reservations {
			for _, in := range r.Instances {
				ai := fleetaudit.AuditInstance{
					ID:      aws.ToString(in.InstanceId),
					State:   string(in.State.Name),
					ShardID: tag(in.Tags, "shard_id"),
					Spare:   tag(in.Tags, "shard_spare") == "true",
				}
				for _, bdm := range in.BlockDeviceMappings {
					if bdm.Ebs != nil && aws.ToString(bdm.DeviceName) != aws.ToString(in.RootDeviceName) {
						ai.DataVolumes = append(ai.DataVolumes, aws.ToString(bdm.Ebs.VolumeId))
					}
				}
				instances = append(instances, ai)
			}
		}
	}

	var volumes []fleetaudit.AuditVolume
	vp := ec2.NewDescribeVolumesPaginator(api, &ec2.DescribeVolumesInput{Filters: []ec2types.Filter{
		{Name: aws.String("tag-key"), Values: []string{"shard_id"}},
	}})
	for vp.HasMorePages() {
		page, err := vp.NextPage(ctx)
		if err != nil {
			return nil, nil, fmt.Errorf("describe volumes: %w", err)
		}
		for _, v := range page.Volumes {
			av := fleetaudit.AuditVolume{
				ID:      aws.ToString(v.VolumeId),
				State:   string(v.State),
				ShardID: tag(v.Tags, "shard_id"),
			}
			for _, a := range v.Attachments {
				switch a.State {
				case ec2types.VolumeAttachmentStateAttached, ec2types.VolumeAttachmentStateAttaching, ec2types.VolumeAttachmentStateBusy:
					av.AttachedTo = aws.ToString(a.InstanceId)
				}
			}
			volumes = append(volumes, av)
		}
	}
	return instances, volumes, nil
}
