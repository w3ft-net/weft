# Convenience targets for weft. The build / deploy primary path lives
# in `kevcons` (see .kevcons.yaml); this Makefile holds local-dev
# tasks that don't touch AWS.

.PHONY: hooks docs generate generate-check test-default-shape collapse test test-deploy integration integration-clean

# Point git at the committed hooks. Run once per clone.
hooks:
	git config core.hooksPath .githooks

# Regenerate the shell help corpus (shell/help_corpus_gen.go) and the web
# manual (frontend/src/generated/docs/) from docs/{shell,manual}/*.md. The
# generated files are committed; the pre-commit hook (`make hooks`) runs this
# whenever a doc is staged, and CI fails if they are stale.
docs:
	go run ./cmd/docgen

# Write the coordinator's coordinate vocabulary (shape/coord_gen.go) from a shape
# spec. Which columns locate a log row is chosen when the coordinator is built:
# run this before `go build`. The generated file is committed, and CI runs
# `make generate-check` to fail on stale output. The in-tree default is the
# hosted shape until the hosted adapter moves out of this tree.
SHAPE ?= shape/specs/hosted.yaml
generate:
	go run ./cmd/shapegen -spec $(SHAPE) -out shape/coord_gen.go

generate-check:
	go run ./cmd/shapegen -spec $(SHAPE) -out shape/coord_gen.go -check

# Build and test the packages that no longer name the tenant under the DEFAULT
# shape (shape/specs/default.yaml: no tenant column), then put the hosted shape
# back whatever happened. A package joins DEFAULT_SHAPE_PKGS when its extraction
# step lands; CI runs this as 'Default shape builds' so a tenant column cannot
# creep back into them.
DEFAULT_SHAPE_PKGS ?= ./shape/... ./internal/shapegen/... ./planner/...
test-default-shape:
	@trap '$(MAKE) --no-print-directory generate SHAPE=shape/specs/hosted.yaml' EXIT; \
	$(MAKE) --no-print-directory generate SHAPE=shape/specs/default.yaml && \
	go build $(DEFAULT_SHAPE_PKGS) && \
	go test -tags test -count=1 $(DEFAULT_SHAPE_PKGS)

# Collapse store/migrations/* into store/schema.sql. Run after a
# successful production deploy (kevcons will eventually trigger this
# automatically via .kevcons.yaml's post_deploy section). Idempotent —
# rerunning produces no diff if there are no migration files left to
# fold in.
collapse:
	./scripts/collapse-migrations.sh

# Run the Go test suite with the `test` build tag. The tag enables
# files like logdb/fixtures.go that ship fixture-seeding helpers
# (logdb.SeedShard) — those are guarded out of production builds so
# they can't be linked into a deployed coordinator. Plain
# `go test ./...` without the tag will fail to compile any package
# whose tests reach for those helpers; that's the intended tripwire.
test:
	go test -tags test ./...

# Run bash-side tests for deploy/ scripts (load-app-env-extra etc.).
# Go tests live under `make test`; this is the bash-only side.
test-deploy:
	./deploy/load-app-env-extra_test.sh
	./deploy/export-role_test.sh
	./deploy/shard-volume_test.sh

# One-shot integration suite: bring the docker stack up (with the
# localstack overlay), run the integration tag set, tear down. Wraps
# scripts/integration-test.sh.
integration:
	./scripts/integration-test.sh

# Wipe the dev state the docker stack accumulates so the next
# integration run starts from zero. Removes postgres + localstack volumes
# (via compose down -v), the docker-launcher-spawned shard containers
# (which live outside compose), and the per-shard DuckDB subdirs under
# dev/shard-data/ that the launcher intentionally preserves across
# container kills. Idempotent.
integration-clean:
	docker compose -f docker-compose.yml -f docker-compose.localstack.yml down -v --remove-orphans 2>/dev/null || true
	docker ps -aq --filter "name=weft-shard-" | xargs -r docker rm -f >/dev/null 2>&1 || true
	rm -rf dev/shard-data
