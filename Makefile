# Tasks for the open-source module. The shape (the columns that locate a log row)
# is chosen when the coordinator is built: shape/coord_gen.go is generated from a
# spec, and the committed one is the default shape.

.PHONY: generate generate-check test

SHAPE ?= shape/specs/default.yaml

generate:
	go run ./cmd/shapegen -spec $(SHAPE) -out shape/coord_gen.go

generate-check:
	go run ./cmd/shapegen -spec $(SHAPE) -out shape/coord_gen.go -check

test:
	go test -tags test ./...
