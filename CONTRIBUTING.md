# Contributing

## Setup

    make hooks           # once per clone: points git at .githooks
    make test

Database-backed tests need `-tags test`; `make test` sets it. If parallel runs
flake, re-run with `-p 1`.

## Generated files

- `shape/coord_gen.go` comes from `make generate`; CI runs `make generate-check`.
- The shell help corpus and the web manual come from `docs/{shell,manual}` via
  `make docs`. The pre-commit hook runs it when a doc is staged, and CI fails if
  the generated files are stale.

## Conventions

- Log with `weftsdk.Infof/Warnf/Errorf(ctx, ...)`, never stdlib `log.Printf`
  (`internal/logtest` enforces it).
- Generic behaviour tests run against every backend through the conformance
  suites; add a case there rather than a backend-specific test.
- Commit messages are a single imperative sentence with no trailing period.

## Pull requests

Keep changes focused, run `make test` and, for web changes, `make web`, and say
in the description what you verified.
