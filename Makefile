# Tasks for the open-source module. The shape (the columns that locate a log row)
# is chosen when the coordinator is built: shape/coord_gen.go is generated from a
# spec, and the committed one is the default shape.

.PHONY: generate generate-check test hooks docs web wasm api-web

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

# Regenerate the shell help corpus (shell/help_corpus_gen.go) and the web
# manual (web/src/generated/docs) from docs/{shell,manual}/*.md. The generated
# files are committed; the pre-commit hook (`make hooks`) runs this whenever a doc
# is staged, and CI fails if they are stale.
docs:
	go run ./cmd/docgen -web-out web/src/generated/docs

# The shell as wasm, for the web app (web/public/shell.wasm, not committed).
wasm:
	GOOS=js GOARCH=wasm go build -o web/public/shell.wasm ./cmd/wasm

# The web app's bundle, web/dist, which a build with -tags webembed carries.
# dist/.gitkeep stays committed so the embed pattern always matches (and
# `go mod vendor` can resolve it); vite empties the directory, so put it back.
web: wasm
	cd web && npm ci && npm run build
	touch web/dist/.gitkeep

# The api with the web app inside it.
api-web: web
	go build -tags webembed -o bin/api ./cmd/api
