# Developer Onboarding

Welcome to weft. This guide walks you through the system as a
maintainer and feature builder — not as a customer. Each section
explains a subsystem, points you at the code, and ends with a
hands-on challenge that requires you to read real source files and
make a small, working change.

Work through the sections in order. Each one builds on context from
the previous ones.

## Sections

1. **[Network and Storage Topology](topology.md)** — how the
   coordinator, shards, compute pool, and agent fit together;
   where data lives; how multi-tenancy is enforced.

2. **[Documentation Generation](docgen.md)** — the pipeline
   that turns markdown into shell help text and web manual pages.

3. **[The Warp Interpreter](warp.md)** — how the bash/lua
   engine works, and how weft overrides it with a virtual
   filesystem, custom builtins, and query rewriting.

4. **[User-Facing Features](features.md)** — watches, dashboards,
   alerts, charts, meta-variables, and MCP tools; how each is
   implemented end-to-end.

## Prerequisites

- Go toolchain (the project builds with `go build`)
- Node/npm (for the React frontend in `frontend/`)
- Docker Compose (for the local dev environment)
- The `warp` repo checked out at `../warp` (the `go.mod` replace
  directive expects it there)

## How to use this guide

Read the narrative, then grep/read the files it references. Each
challenge is designed to take 1-3 hours and touch 2-4 files. Don't
skip the reading — the challenges assume you understand the
surrounding code, not just the function you're editing.

When you finish a challenge, open a PR. The diff is the proof that
you understood the subsystem.
