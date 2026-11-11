// Command shapegen writes the coordinator's coordinate vocabulary from a shape
// spec: package shape's coord_gen.go. Run it before `go build` of the
// coordinator, through `make generate SHAPE=<spec>`.
//
//	shapegen -spec shape/specs/hosted.yaml -out shape/coord_gen.go
//	shapegen -spec shape/specs/hosted.yaml -out shape/coord_gen.go -check
//
// With -check it writes nothing and fails when the file on disk is not what the
// spec generates, so CI catches stale output the way it does for `make docs`.
package main

import (
	"bytes"
	"flag"
	"fmt"
	"os"

	"github.com/graybeardcorp/weft/internal/shapegen"
)

func main() {
	specPath := flag.String("spec", "", "the shape spec (YAML)")
	out := flag.String("out", "shape/coord_gen.go", "the Go file to write")
	check := flag.Bool("check", false, "fail if -out is not what the spec generates, and write nothing")
	flag.Parse()
	if *specPath == "" {
		fmt.Fprintln(os.Stderr, "shapegen: -spec is required")
		os.Exit(2)
	}
	if err := run(*specPath, *out, *check); err != nil {
		fmt.Fprintln(os.Stderr, "shapegen:", err)
		os.Exit(1)
	}
}

func run(specPath, out string, check bool) error {
	b, err := os.ReadFile(specPath)
	if err != nil {
		return err
	}
	spec, err := shapegen.ParseSpec(b)
	if err != nil {
		return fmt.Errorf("%s: %w", specPath, err)
	}
	src, err := shapegen.Generate(spec, specPath)
	if err != nil {
		return err
	}
	if check {
		have, err := os.ReadFile(out)
		if err != nil {
			return err
		}
		if !bytes.Equal(have, src) {
			return fmt.Errorf("%s is stale for %s: run `make generate SHAPE=%s`", out, specPath, specPath)
		}
		return nil
	}
	return os.WriteFile(out, src, 0o644)
}
