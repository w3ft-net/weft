# Documentation Generation

Weft's documentation is written once in markdown and compiled into
two outputs: plain-text with ANSI color codes for the interactive
shell, and HTML for the web manual. There is no runtime markdown
parsing — both the Go binary and the React frontend consume
pre-generated files.

## Source files

Documentation lives in two directories:

- `docs/shell/` — help topics and man pages for the interactive
  shell (e.g., `help builtins`, `man chart`).
- `docs/manual/` — longer reference pages for the web manual
  (e.g., dashboards, meta-variables, alerting).

A single doc can appear in both the shell and the web, or only
one, depending on its frontmatter.

## Frontmatter format

Every doc starts with YAML frontmatter between `---` fences:

```yaml
---
id: help-alerts
title: Alerts
shell:
  const: HelpAlerts
  topics: ["help alerts"]
web:
  section: alerting
  order: 70
---
```

- `id` and `title` are required.
- `shell.const` — the Go constant name in the generated file.
- `shell.topics` — dispatch strings the user types to reach this
  content (e.g., `help alerts` or `man chart`).
- `shell.banner` — optional man-page header line.
- `web.section` — the anchor id on the `/manual` page.
- `web.order` — controls render order (lower is earlier).

If a doc has neither `shell:` nor `web:`, the generator errors.
This catches orphaned docs.

The frontmatter struct is defined in `cmd/docgen/doc.go`:

```go
type shellConfig struct {
    Const  string   `yaml:"const"`
    Topics []string `yaml:"topics"`
    Banner string   `yaml:"banner"`
}

type webConfig struct {
    Section string `yaml:"section"`
    Order   int    `yaml:"order"`
}
```

## The generator

`cmd/docgen/` is a standalone Go CLI. It reads markdown from the
input directories and writes two outputs:

```
go run ./cmd/docgen \
  -docs "docs/shell,docs/manual" \
  -shell-out "shell/help_corpus_gen.go" \
  -web-out "frontend/src/generated/docs"
```

### Shell output

`cmd/docgen/shell.go` converts markdown to plain text with ANSI
escape codes using the goldmark parser. The output is
`shell/help_corpus_gen.go`, which contains:

- One `const` per topic (e.g., `const HelpAlerts = "..."`) with
  inline ANSI for headings (orange) and blockquotes (dim gray).
- A `shellDocTopics` map keyed by dispatch string
  (`"help alerts"` → `HelpAlerts`).

The shell consumes this via `shell/help_provider.go`, which
implements the `shellcore.HelpProvider` interface:

- `Topics()` returns all dispatch strings.
- `Render(topic)` returns the pre-rendered text. Some topics
  (like `help cron`) interpolate dynamic data (tier limits) at
  render time.

### Web output

`cmd/docgen/web.go` converts markdown to HTML via goldmark and
writes one TypeScript module per doc into
`frontend/src/generated/docs/`:

```typescript
const alerting = {
  id: "help-alerts",
  section: "alerting",
  title: "Alerts",
  order: 70,
  html: `<h2>Alerts</h2>\n<p>An alert evaluates...</p>`,
} as const;

export default alerting;
```

An `index.ts` re-exports all modules as a sorted
`manualSections[]` array. The frontend consumes this in
`frontend/src/pages/Manual.tsx`, which renders sections with
`dangerouslySetInnerHTML` and builds a table of contents from the
section titles.

## The full pipeline

```
docs/shell/help-alerts.md
docs/manual/dashboards.md
          │
    cmd/docgen (go run)
          │
    ┌─────┴─────────────────────┐
    │                           │
    ▼                           ▼
shell/help_corpus_gen.go    frontend/src/generated/docs/*.ts
  (Go consts + ANSI)          (TS modules + HTML)
    │                           │
    ▼                           ▼
shell/help_provider.go      pages/Manual.tsx
  (HelpProvider interface)    (dangerouslySetInnerHTML)
    │                           │
    ▼                           ▼
Interactive shell             Web manual
(help alerts, man chart)      (/manual#alerting)
```

## When to regenerate

The generated files are committed to git. When you edit a markdown
file in `docs/shell/` or `docs/manual/`, regenerate by running:

```
go run ./cmd/docgen
```

Then commit the markdown changes and the regenerated files
together. There is no CI hook that does this automatically — the
generated files must match the source at commit time.

---

## Challenge: add a new man page

**Goal:** add a `man git` page that documents the version history
commands (`git log`, `git show`, `git diff`, `git restore`,
`git commit --amend`), and make it appear in both the shell and the web
manual.

### Setup

Read these files first:

- `docs/shell/help-index.md` — see the frontmatter format and
  how the help index references man pages.
- `docs/shell/man-chart.md` — a complete man page example with
  both `shell:` and `web:` frontmatter.
- `cmd/docgen/doc.go` — the frontmatter struct and parser.
- `cmd/docgen/shell.go` — how markdown becomes ANSI text.
- `cmd/docgen/web.go` — how markdown becomes HTML + TypeScript.
- `coordinator/wit.go` — the implementation of wit commands
  (source of truth for what the commands do).

### Task

1. Create `docs/shell/man-wit.md` with frontmatter that registers
   it as both a shell man page (`man wit`, `wit --help`) and a
   web manual section.

2. Document the five wit subcommands. Include the ref syntax
   (`HEAD`, `HEAD^`, `HEAD^N`, hash prefix). Note that wit only
   works on `/bin/` and `/cron.d/` paths, and that the last 5
   versions are retained per file.

3. Run `go run ./cmd/docgen` to regenerate.

4. Verify:
   - `shell/help_corpus_gen.go` contains a new `ManWit` constant.
   - `frontend/src/generated/docs/` contains a new `.ts` module.
   - The help index at `docs/shell/help-index.md` should be
     updated to list `man wit` in the reference pages section.

5. Build the frontend (`cd frontend && npm run build`) to confirm
   the TypeScript compiles.

### What you'll learn

- The full doc pipeline from markdown to shell and web.
- How frontmatter fields control where a doc appears.
- The commit discipline: source markdown and generated files
  travel together.
