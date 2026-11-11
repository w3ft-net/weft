package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/graybeardcorp/weft/shape"
	"github.com/graybeardcorp/weft/shard"
)

// convertResult is what convert-archive reports on stdout, one JSON object.
type convertResult struct {
	ShardID string `json:"shard_id"`
	// Status is "converted", or "skipped" for an archive that is already sized.
	Status  string `json:"status"`
	OldPath string `json:"old_path"`
	NewPath string `json:"new_path,omitempty"`
	Rows    int64  `json:"rows,omitempty"`
	Keys    int    `json:"keys,omitempty"`
	Files   int    `json:"files,omitempty"`
	Bytes   int64  `json:"bytes,omitempty"`
}

// convertArchive turns a downloaded archive into a sized copy
// (docs/plans/per-service-tables.md, "Archive layout"). root holds the download,
// mirroring the bucket, and out receives the copy at the same key its upload
// goes to. It refuses to write anything unless the download matches what the
// export recorded (so an incomplete one stops here), and returns an error unless
// the copy matches both the original, key by key, and the manifest.
func convertArchive(m shard.ExportManifest, root, out string, now time.Time) (convertResult, error) {
	res := convertResult{ShardID: m.ShardID, OldPath: m.ParquetPath}
	if shard.IsSized(m.ParquetPath) {
		res.Status = "skipped"
		return res, nil
	}
	src := shard.LocalPathOf(m.ParquetPath, root)
	if err := shard.VerifyAgainstManifest(src, m, shape.BuildLayout(), nil); err != nil {
		return res, fmt.Errorf("the download is not the archive the manifest describes: %w", err)
	}

	newPath := shard.SizedArchivePath(m.ParquetPath, now)
	dst := shard.LocalPathOf(newPath, out)
	if _, err := shard.CopyArchive(src, dst, shape.BuildLayout(), nil); err != nil {
		return res, fmt.Errorf("convert %s: %w", m.ShardID, err)
	}
	keys, err := shard.CompareArchives(src, dst, shape.BuildLayout(), nil)
	if err != nil {
		return res, fmt.Errorf("the copy differs from the original: %w", err)
	}
	if err := shard.VerifyAgainstManifest(dst, m, shape.BuildLayout(), nil); err != nil {
		return res, fmt.Errorf("the copy does not match the manifest: %w", err)
	}

	res.Status, res.NewPath, res.Rows, res.Keys = "converted", newPath, m.Rows, keys
	filepath.WalkDir(strings.TrimSuffix(dst, "/**/*.parquet"), func(p string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() && strings.HasSuffix(p, ".parquet") {
			if fi, e := d.Info(); e == nil {
				res.Files++
				res.Bytes += fi.Size()
			}
		}
		return nil
	})
	return res, nil
}

// convert-archive is the conversion step of scripts/convert_archives.py, run on
// a machine with the disk and memory for it. It is its own binary, not a
// weft-admin subcommand: weft-admin is built without cgo for the coordinator
// AMI, and the conversion needs DuckDB.
func main() {
	fs := flag.NewFlagSet("convert-archive", flag.ExitOnError)
	manifestPath := fs.String("manifest", "", "the shard's export manifest (JSON), downloaded from <prefix>/_manifests/")
	root := fs.String("root", "", "directory holding the downloaded archive, mirroring the bucket")
	out := fs.String("out", "", "directory to write the copy to, mirroring the bucket")
	if err := fs.Parse(os.Args[1:]); err != nil {
		os.Exit(2)
	}
	if *manifestPath == "" || *root == "" || *out == "" {
		fmt.Fprintln(os.Stderr, "usage: convert-archive --manifest FILE --root DIR --out DIR")
		os.Exit(2)
	}
	b, err := os.ReadFile(*manifestPath)
	if err != nil {
		log.Fatal(err)
	}
	var m shard.ExportManifest
	if err := json.Unmarshal(b, &m); err != nil {
		log.Fatalf("read manifest %s: %v", *manifestPath, err)
	}
	res, err := convertArchive(m, *root, *out, time.Now())
	if err != nil {
		log.Fatal(err)
	}
	if err := json.NewEncoder(os.Stdout).Encode(res); err != nil {
		log.Fatal(err)
	}
}
