# Tasks for the open-source module. The shape (the columns that locate a log row)
# is chosen when the coordinator is built: shape/coord_gen.go is generated from a
# spec, and the committed one is the default shape.

.PHONY: generate generate-check test hooks docs

SHAPE ?= shape/specs/default.yaml

generate:
	go run ./cmd/shapegen -spec $(SHAPE) -out shape/coord_gen.go

generate-check:
	go run ./cmd/shapegen -spec $(SHAPE) -out shape/coord_gen.go -check

test:
	go test -tags test ./...

# Point git at the committed hooks. Run once per clone.
hooks:
	git config core.hooksPath .githooks

# Regenerate the shell help corpus (shell/help_corpus_gen.go) from
# docs/{shell,manual}/*.md. The generated file is committed; the pre-commit hook
# (`make hooks`) runs this whenever a doc is staged, and CI fails if it is stale.
# A deployment with a web manual runs the same generator with -web-out.
docs:
	go run ./cmd/docgen
