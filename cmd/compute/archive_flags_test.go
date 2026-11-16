package main

import (
	"flag"
	"testing"

	"github.com/w3ft-net/weft/backends/logs/archive"
	"github.com/w3ft-net/weft/compute"
)

func TestArchiveReaderFlags(t *testing.T) {
	fs := flag.NewFlagSet("compute", flag.ContinueOnError)
	each := readerFlag{}
	fs.Var(each, "archive-reader", "")
	def := fs.String("archive-reader-default", "", "")
	if err := fs.Parse([]string{"-archive-reader", "parquet-sized-v1=duckdb-lambda", "-archive-reader-default", "duckdb-lambda"}); err != nil {
		t.Fatal(err)
	}
	got := archiveReaders(each, *def)
	if len(got) != 2 || got["parquet-sized-v1"] != "duckdb-lambda" || got[archive.Default] != "duckdb-lambda" {
		t.Errorf("readers = %v", got)
	}
	for _, bad := range []string{"noequals", "=x", "x=", ""} {
		if err := (readerFlag{}).Set(bad); err == nil {
			t.Errorf("-archive-reader %q was accepted", bad)
		}
	}
	dup := readerFlag{}
	if err := dup.Set("a=x"); err != nil {
		t.Fatal(err)
	}
	if dup.Set("a=y") == nil {
		t.Error("a format given two readers was accepted")
	}
}

// With no flags there is no mapping, and compute refuses it before it serves a
// query: an archived shard could never be read.
func TestNoArchiveReaderFlagsIsRefused(t *testing.T) {
	if _, err := compute.NewArchiveRouter(archiveReaders(readerFlag{}, ""), nil, ""); err == nil {
		t.Fatal("compute built a router with no reader configured")
	}
	if _, err := compute.NewArchiveRouter(archiveReaders(readerFlag{}, "duckdb-lambda"), nil, ""); err != nil {
		t.Fatalf("the Lambda reader as the default: %v", err)
	}
	if _, err := compute.NewArchiveRouter(archiveReaders(readerFlag{}, "no-such-reader"), nil, ""); err == nil {
		t.Fatal("an unregistered reader was accepted")
	}
}
