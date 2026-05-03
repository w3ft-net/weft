# Convenience targets for weft. The build / deploy primary path lives
# in `kevcons` (see .kevcons.yaml); this Makefile holds local-dev
# tasks that don't touch AWS.

.PHONY: collapse

# Collapse store/migrations/* into store/schema.sql. Run after a
# successful production deploy (kevcons will eventually trigger this
# automatically via .kevcons.yaml's post_deploy section). Idempotent —
# rerunning produces no diff if there are no migration files left to
# fold in.
collapse:
	./scripts/collapse-migrations.sh
