package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	pb "github.com/graybeardcorp/weft/proto/weftpb"
)

const cronModeUsage = `usage: weft-admin cron-mode <list|set|rollback> [flags]

  list                   every enabled cron and the path that runs it
  set <id>...            switch crons to per-slot claims (explicit ids only)
  rollback <id>...|all   return crons to lease mode

set and rollback are dry runs unless --yes. See docs/plans/cron-claims-rollout.md.`

func cronModeCmd(args []string) {
	if len(args) < 1 {
		fmt.Fprintln(os.Stderr, cronModeUsage)
		os.Exit(2)
	}
	fs := flag.NewFlagSet("cron-mode "+args[0], flag.ExitOnError)
	coord := fs.String("coord", "127.0.0.1:9022", "coordinator WeftAdmin gRPC address (localhost-only; SharedToken auth)")
	yes := fs.Bool("yes", false, "apply the change (default: show what would change and exit)")
	pos := parseInterspersed(fs, args[1:])

	client, closeConn, err := newAdminClient(*coord)
	if err != nil {
		log.Fatal(err)
	}
	defer closeConn()
	ctx, cancel := context.WithTimeout(adminCtx(context.Background()), time.Minute)
	defer cancel()

	list, err := client.ListCronModes(ctx, &pb.ListCronModesRequest{})
	if err != nil {
		log.Fatalf("list cron modes: %v", err)
	}

	switch args[0] {
	case "list":
		if len(pos) != 0 {
			log.Fatal("cron-mode list takes no arguments")
		}
		fmt.Print(formatCronModes(list.Crons))
	case "set":
		ids, all, err := parseCronIDs(pos, false)
		if err != nil || all {
			log.Fatalf("cron-mode set: %v", err)
		}
		fmt.Print(formatCronModes(selectCrons(list.Crons, ids)))
		if !*yes {
			fmt.Printf("dry run: re-run with --yes to switch these %d cron(s) to claims\n", len(ids))
			return
		}
		resp, err := client.EnableCronClaims(ctx, &pb.EnableCronClaimsRequest{WatchIds: ids})
		if err != nil {
			log.Fatalf("enable claims: %v", err)
		}
		fmt.Printf("switched to claims: %v\nalready claims: %v\n", resp.Changed, resp.AlreadyClaims)
	case "rollback":
		ids, all, err := parseCronIDs(pos, true)
		if err != nil {
			log.Fatalf("cron-mode rollback: %v", err)
		}
		if all {
			fmt.Print(formatCronModes(filterMode(list.Crons, "claims")))
		} else {
			fmt.Print(formatCronModes(selectCrons(list.Crons, ids)))
		}
		if !*yes {
			fmt.Println("dry run: re-run with --yes to roll these back to lease mode")
			return
		}
		resp, err := client.RollbackCronClaims(ctx, &pb.RollbackCronClaimsRequest{WatchIds: ids, All: all})
		if err != nil {
			log.Fatalf("rollback: %v", err)
		}
		fmt.Printf("rolled back: %v\nnot in claim mode: %v\n", resp.RolledBack, resp.NotClaims)
		fmt.Printf("lease mode cannot take over for %ds. If you are rolling the coordinator CODE back too, wait that long first: tombstones left in place stop those crons.\n", resp.WindowSeconds)
	default:
		fmt.Fprintf(os.Stderr, "unknown cron-mode subcommand %q\n%s\n", args[0], cronModeUsage)
		os.Exit(2)
	}
}

// parseInterspersed parses fs's flags wherever they appear among the
// arguments (so `set 12 13 --yes` works) and returns the positionals.
func parseInterspersed(fs *flag.FlagSet, args []string) []string {
	var pos []string
	for {
		if err := fs.Parse(args); err != nil {
			os.Exit(2)
		}
		if fs.NArg() == 0 {
			return pos
		}
		pos = append(pos, fs.Arg(0))
		args = fs.Args()[1:]
	}
}

// parseCronIDs reads watch ids (decimal) or, when allowAll, the single word
// "all". Rejects zero ids, duplicates are left to the server, and "all"
// mixed with ids is an error.
func parseCronIDs(args []string, allowAll bool) (ids []uint64, all bool, err error) {
	if len(args) == 0 {
		return nil, false, fmt.Errorf("give at least one cron id")
	}
	for _, a := range args {
		if a == "all" {
			if !allowAll {
				return nil, false, fmt.Errorf(`"all" is not accepted here: name the crons explicitly`)
			}
			all = true
			continue
		}
		id, perr := strconv.ParseUint(a, 10, 64)
		if perr != nil || id == 0 {
			return nil, false, fmt.Errorf("%q is not a cron id", a)
		}
		ids = append(ids, id)
	}
	if all && len(ids) > 0 {
		return nil, false, fmt.Errorf(`"all" cannot be combined with ids`)
	}
	return ids, all, nil
}

func selectCrons(all []*pb.CronModeInfo, ids []uint64) []*pb.CronModeInfo {
	want := make(map[uint64]bool, len(ids))
	for _, id := range ids {
		want[id] = true
	}
	var out []*pb.CronModeInfo
	for _, c := range all {
		if want[c.WatchId] {
			out = append(out, c)
		}
	}
	return out
}

func filterMode(all []*pb.CronModeInfo, mode string) []*pb.CronModeInfo {
	var out []*pb.CronModeInfo
	for _, c := range all {
		if c.Mode == mode {
			out = append(out, c)
		}
	}
	return out
}

func formatCronModes(crons []*pb.CronModeInfo) string {
	var b strings.Builder
	tw := tabwriter.NewWriter(&b, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "ID\tNAME\tSCHEDULE\tMODE\tLEASE HOLDER\tSINCE")
	for _, c := range crons {
		fmt.Fprintf(tw, "%d\t%s\t%s\t%s\t%s\t%s\n", c.WatchId, c.Name, c.Schedule, c.Mode, c.LeaseHolder, c.AcquiredAt)
	}
	tw.Flush()
	return b.String()
}
