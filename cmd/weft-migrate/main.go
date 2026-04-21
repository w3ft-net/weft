// weft-migrate applies pending database migrations.
//
// Invoked by the coordinator ASG's launch lifecycle hook (see
// asg-weft-application terraform stack). The hook holds the instance in
// Pending:Wait until this exits 0; on non-zero exit the instance is
// abandoned and never joins the NLB.
//
// Subsequent scale-up nodes run this too — they're no-ops because the
// embedded goose runner records applied migrations in the
// `goose_db_version` table.
//
// The DSN comes from $DB_DSN (set by fetch-config.sh into
// /opt/app/.env before the lifecycle hook executes). --db-dsn is available
// for one-off manual runs.
package main

import (
	"flag"
	"fmt"
	"log"
	"os"
	"time"

	"github.com/kevlarcon/weft/store"
)

func main() {
	dsn := flag.String("db-dsn", os.Getenv("DB_DSN"), "postgres DSN (defaults to $DB_DSN)")
	flag.Parse()

	if *dsn == "" {
		log.Fatal("weft-migrate: no DSN provided (set --db-dsn or $DB_DSN)")
	}

	start := time.Now()
	if err := store.Migrate(*dsn); err != nil {
		log.Fatalf("weft-migrate: %v", err)
	}
	fmt.Printf("weft-migrate: schema up to date (%s)\n", time.Since(start).Round(time.Millisecond))
}
