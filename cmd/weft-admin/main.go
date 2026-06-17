// weft-admin is the coordinator-resident CLI for operator/deploy actions
// that drive the shard FSM out-of-band of normal cluster operation. It
// dials the local coordinator's gRPC and calls the admin RPCs.
//
// The intended invocation is `kevcons ssh --command "sudo /opt/app/bin/weft-admin …"`
// against the coordinator instance during a deploy or an ad-hoc operator
// action. Localhost trust boundary; no auth.
//
// Phase 1 shipped `roll-shard` (fire-and-exit). Phase 2 (this commit) adds:
//   - roll-cohort <bucket-id>   — sequentially roll every live shard in a
//     bucket, waiting for each to return to
//     INGESTING before moving on.
//   - roll-all                  — same, across every bucket.
//   - wait-and-verify           — pollShardReady (cmd/weft-admin/wait.go)
//     polls GetShard until state=INGESTING
//     with a post-relaunch heartbeat.
//   - --per-shard-timeout       — bound the wait per shard (default 15m).
//   - --continue-on-failure     — keep going past a failed shard.
//
// Phase 3 adds skip-if-current via --target-sha against shard_records.binary_sha.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"sort"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"

	pb "github.com/graybeardcorp/weft/proto/weftpb"
)

// adminTokenEnv is the env var that carries the shared bearer token used
// to authenticate against the coordinator's WeftAdmin gRPC listener. Both
// the coordinator (server side) and this CLI (client side) read it from
// the same env loader on the coordinator instance, so the token is
// available out of band rather than configured per-invocation.
const adminTokenEnv = "WEFT_ADMIN_TOKEN"

// adminCtx returns a context with the admin bearer attached as outgoing
// gRPC metadata. The coordinator's SharedToken interceptor rejects calls
// without it (Unauthenticated), so we fail fast here with a clear error
// when the env isn't wired.
func adminCtx(ctx context.Context) context.Context {
	tok := os.Getenv(adminTokenEnv)
	if tok == "" {
		log.Fatalf("%s is not set; weft-admin requires the shared admin bearer (load it from the same env source the coordinator uses)", adminTokenEnv)
	}
	return metadata.AppendToOutgoingContext(ctx, "authorization", "Bearer "+tok)
}

func main() {
	log.SetFlags(0) // operator-facing; timestamps just noise
	if len(os.Args) < 2 {
		usage()
	}
	switch os.Args[1] {
	case "roll-shard":
		rollShardCmd(os.Args[2:])
	case "roll-cohort":
		rollCohortCmd(os.Args[2:])
	case "roll-all":
		rollAllCmd(os.Args[2:])
	case "bump-shard-templates":
		bumpShardTemplatesCmd(os.Args[2:])
	case "-h", "--help", "help":
		usage()
	default:
		fmt.Fprintf(os.Stderr, "unknown subcommand %q\n", os.Args[1])
		usage()
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, `usage: weft-admin <subcommand> [flags]

Subcommands:
  roll-shard  <shard-id>   relaunch a single shard in place
  roll-cohort <bucket-id>  roll every live shard in a bucket, one at a time
  roll-all                 roll every live shard across every bucket
  bump-shard-templates     point shard launch templates at /<app>/ami/latest

Common flags:
  -coord                coordinator gRPC address (default "localhost:9020")
  -per-shard-timeout    deadline for each shard's relaunch + return to a live serving state (default 15m)
  -continue-on-failure  keep going past a failed shard (roll-cohort / roll-all)`)
	os.Exit(2)
}

// liveStates are the shard states that have data and a binary worth
// upgrading. LAUNCHING is skipped (already coming up on the current AMI);
// ARCHIVING/ARCHIVED/GONE/SUSPENDED have nothing to upgrade.
var liveStates = map[string]bool{
	"INGESTING": true,
	"READ_ONLY": true,
	"DEGRADED":  true,
}

type commonFlags struct {
	coord           string
	perShardTimeout time.Duration
	pollInterval    time.Duration
	continueOnFail  bool
	targetSHA       string
}

func registerCommon(fs *flag.FlagSet, includeContinue bool) *commonFlags {
	c := &commonFlags{}
	fs.StringVar(&c.coord, "coord", "127.0.0.1:9022", "coordinator WeftAdmin gRPC address (localhost-only; SharedToken auth)")
	fs.DurationVar(&c.perShardTimeout, "per-shard-timeout", 15*time.Minute, "deadline for each shard's relaunch + return to a live serving state")
	fs.DurationVar(&c.pollInterval, "poll-interval", 5*time.Second, "interval between GetShard polls during wait-and-verify")
	fs.StringVar(&c.targetSHA, "target-sha", "", "skip shards whose binary_sha already matches and require the post-relaunch shard to report this SHA (empty = roll unconditionally, no SHA verification)")
	if includeContinue {
		fs.BoolVar(&c.continueOnFail, "continue-on-failure", false, "keep going past a failed shard")
	}
	return c
}

func newAdminClient(coord string) (pb.WeftAdminClient, func(), error) {
	conn, err := grpc.NewClient(coord, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return nil, nil, fmt.Errorf("dial coordinator at %s: %w", coord, err)
	}
	return pb.NewWeftAdminClient(conn), func() { conn.Close() }, nil
}

// rollOne fires RelaunchShard for shardID and then waits via pollShardReady
// for the shard to land in a live serving state (INGESTING / READ_ONLY /
// DEGRADED — the FSM restores the shard's pre-recovery state, so a
// rolled READ_ONLY shard correctly stays READ_ONLY) with a heartbeat
// stamped after the pre-fire cutoff. Returns nil on success; errors
// include both RPC and wait-timeout cases.
func rollOne(ctx context.Context, client pb.WeftAdminClient, shardID string, perShardTimeout, pollInterval time.Duration, wantSHA string) error {
	rollCtx, cancel := context.WithTimeout(adminCtx(ctx), perShardTimeout)
	defer cancel()

	cutoff := time.Now()
	if _, err := client.RelaunchShard(rollCtx, &pb.RelaunchShardRequest{ShardId: shardID}); err != nil {
		return fmt.Errorf("relaunch %s: %w", shardID, err)
	}
	fmt.Printf("  relaunch fired for %s; waiting for return to a live serving state...\n", shardID)

	get := func(id string) (string, time.Time, string, error) {
		resp, err := client.GetShard(rollCtx, &pb.GetShardRequest{ShardId: id})
		if err != nil {
			return "", time.Time{}, "", err
		}
		hb := time.Time{}
		if s := resp.Shard.LastHeartbeatAt; s != "" {
			if parsed, perr := time.Parse(time.RFC3339Nano, s); perr == nil {
				hb = parsed
			}
		}
		return resp.Shard.State, hb, resp.Shard.BinarySha, nil
	}
	if err := pollShardReady(rollCtx, get, shardID, cutoff, wantSHA, pollInterval); err != nil {
		return err
	}
	fmt.Printf("  %s is back serving\n", shardID)
	return nil
}

func rollShardCmd(args []string) {
	fs := flag.NewFlagSet("roll-shard", flag.ExitOnError)
	c := registerCommon(fs, false)
	if err := fs.Parse(args); err != nil {
		os.Exit(2)
	}
	if fs.NArg() != 1 {
		fmt.Fprintln(os.Stderr, "roll-shard requires exactly one positional argument: <shard-id>")
		os.Exit(2)
	}
	shardID := fs.Arg(0)

	client, closeConn, err := newAdminClient(c.coord)
	if err != nil {
		log.Fatal(err)
	}
	defer closeConn()

	if err := rollOne(context.Background(), client, shardID, c.perShardTimeout, c.pollInterval, c.targetSHA); err != nil {
		log.Fatal(err)
	}
}

// liveShardIDs filters a ShardInfo slice to the states worth rolling, in
// deterministic id order. Used as the input to shardsToRoll.
func liveShardIDs(shards []*pb.ShardInfo) []string {
	ids := make([]string, 0, len(shards))
	for _, sh := range shards {
		if liveStates[sh.State] {
			ids = append(ids, sh.Id)
		}
	}
	sort.Strings(ids)
	return ids
}

// shardsToRoll layers --target-sha skip-if-current on top of liveShardIDs.
// When target is non-empty, any live shard whose binary_sha already matches
// is skipped — the deploy is a no-op for it (the win Phase 3 unlocks for
// shard-stable deploys). Empty target rolls every live shard.
//
// An empty binary_sha on the row means "unknown" — typically a shard from
// before this column existed, or one that hasn't heartbeated since boot.
// We can't trust an unknown SHA, so we ROLL it: better one unnecessary
// rotation than silently skipping a shard that's actually on old code.
func shardsToRoll(shards []*pb.ShardInfo, target string) []string {
	ids := make([]string, 0, len(shards))
	for _, sh := range shards {
		if !liveStates[sh.State] {
			continue
		}
		if target != "" && sh.BinarySha == target {
			continue
		}
		ids = append(ids, sh.Id)
	}
	sort.Strings(ids)
	return ids
}

func rollCohort(ctx context.Context, client pb.WeftAdminClient, bucketID int64, c *commonFlags) error {
	resp, err := client.ListShards(adminCtx(ctx), &pb.ListShardsRequest{BucketId: bucketID})
	if err != nil {
		return fmt.Errorf("list shards for bucket %d: %w", bucketID, err)
	}
	ids := shardsToRoll(resp.Shards, c.targetSHA)
	live := liveShardIDs(resp.Shards) // total live count, for the skip message
	skipped := len(live) - len(ids)
	switch {
	case c.targetSHA != "" && skipped > 0:
		fmt.Printf("bucket %d: %d live shard(s), %d on target SHA (skipped), %d to roll\n", bucketID, len(live), skipped, len(ids))
	default:
		fmt.Printf("bucket %d: %d live shard(s) to roll\n", bucketID, len(ids))
	}

	var failed []string
	for _, id := range ids {
		fmt.Printf("rolling %s (bucket %d)...\n", id, bucketID)
		if err := rollOne(ctx, client, id, c.perShardTimeout, c.pollInterval, c.targetSHA); err != nil {
			if !c.continueOnFail {
				return fmt.Errorf("roll %s: %w", id, err)
			}
			fmt.Fprintf(os.Stderr, "  FAILED %s: %v (continuing)\n", id, err)
			failed = append(failed, id)
		}
	}
	if len(failed) > 0 {
		return fmt.Errorf("bucket %d: %d shard(s) failed: %v", bucketID, len(failed), failed)
	}
	return nil
}

func rollCohortCmd(args []string) {
	fs := flag.NewFlagSet("roll-cohort", flag.ExitOnError)
	c := registerCommon(fs, true)
	if err := fs.Parse(args); err != nil {
		os.Exit(2)
	}
	if fs.NArg() != 1 {
		fmt.Fprintln(os.Stderr, "roll-cohort requires exactly one positional argument: <bucket-id>")
		os.Exit(2)
	}
	var bucketID int64
	if _, err := fmt.Sscanf(fs.Arg(0), "%d", &bucketID); err != nil {
		log.Fatalf("invalid bucket id %q: %v", fs.Arg(0), err)
	}

	client, closeConn, err := newAdminClient(c.coord)
	if err != nil {
		log.Fatal(err)
	}
	defer closeConn()

	if err := rollCohort(context.Background(), client, bucketID, c); err != nil {
		log.Fatal(err)
	}
}

func rollAllCmd(args []string) {
	fs := flag.NewFlagSet("roll-all", flag.ExitOnError)
	c := registerCommon(fs, true)
	if err := fs.Parse(args); err != nil {
		os.Exit(2)
	}
	if fs.NArg() != 0 {
		fmt.Fprintln(os.Stderr, "roll-all takes no positional arguments")
		os.Exit(2)
	}

	client, closeConn, err := newAdminClient(c.coord)
	if err != nil {
		log.Fatal(err)
	}
	defer closeConn()

	ctx := context.Background()

	// Default the skip-if-current target to the SHA the coordinator's own AMI
	// advertises (its ShardGitSHA tag): only shards whose binary_sha differs get
	// rolled, so a coordinator-only deploy is a fleet no-op. The coordinator's
	// own image is the live, deployed AMI shards launch from — so the fleet rolls
	// toward what the coordinator is actually running, not whatever a stale
	// launch template happens to point at. An explicit --target-sha overrides; an
	// untagged AMI ("") rolls every live shard.
	if c.targetSHA == "" {
		sha, err := resolveShardTargetSHA(ctx)
		if err != nil {
			log.Fatalf("resolve target SHA from coordinator AMI: %v", err)
		}
		if sha != "" {
			c.targetSHA = sha
			fmt.Printf("target SHA (from coordinator's own AMI): %s\n", sha)
		} else {
			fmt.Println("no ShardGitSHA tag on the coordinator AMI; rolling all live shards")
		}
	}

	buckets, err := client.ListBuckets(adminCtx(ctx), &pb.ListBucketsRequest{})
	if err != nil {
		log.Fatalf("list buckets: %v", err)
	}
	fmt.Printf("rolling %d bucket(s)\n", len(buckets.Buckets))

	var anyFailed bool
	for _, b := range buckets.Buckets {
		fmt.Printf("\n==> bucket %d (%s, tier=%s, target=%d)\n", b.Id, b.Label, b.Tier, b.TargetIngestingCount)
		if err := rollCohort(ctx, client, b.Id, c); err != nil {
			if !c.continueOnFail {
				log.Fatal(err)
			}
			fmt.Fprintf(os.Stderr, "bucket %d failed: %v (continuing)\n", b.Id, err)
			anyFailed = true
		}
	}
	if anyFailed {
		log.Fatal("roll-all completed with failures (see above)")
	}
}
