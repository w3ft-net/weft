---
id: manual-cli
title: The weft CLI
web:
  section: cli
  category: getting-started
  order: 5
---

## The weft CLI

Weft has a the command-line client. It's called `weft`. It opens the
[shell](query-logs.md) where you query your logs, and it manages the things
around them: namespaces, tokens, sources and integrations. Anything you can do
in the browser, you can do from here. Sometimes the opposite is not true, so
get used to reaching for the executable in the terminal.

### Installation

Download the binary for your platform (Linux or macOS, amd64 or arm64) from the
Downloads page, or fetch it directly:

```
curl -Lo weft https://<your-releases-host>/weft/latest/weft-darwin-arm64
chmod +x weft && sudo mv weft /usr/local/bin/
```

Swap `darwin-arm64` for `darwin-amd64`, `linux-amd64` or `linux-arm64`. Check
it with `weft version`.

### Authenticating

`weft login` prints a URL and a short code, and opens the URL in your browser.
Enter the code there and approve the request; the CLI waits, then saves your
credentials. 

Credentials are saved to `~/.weft.yaml`, which holds the API endpoint, your
token and your active org. A `.weft.yaml` in the current directory, if there is
one, is read instead of it -- handy if you use weft with multiple
organizations. `weft whoami` shows who you're logged in as, your team and your
plan.

### Commands

| Command | What it does |
|---|---|
| `weft sh [-c 'pipeline'] [--range <start>..<end>]` | Open the [shell](query-logs.md), or run one pipeline and exit |
| `weft login` | Authenticate through your browser |
| `weft whoami` | Show your user, team, and plan |
| `weft org [list \| switch <name>]` | List the orgs you belong to or switch the active one |
| `weft namespace {list \| create \| remove}` | Manage [namespaces](namespaces.md) |
| `weft token {create \| list \| revoke}` | Manage [tokens](namespaces.md#tokens) |
| `weft source {add \| list \| remove \| pending \| configure \| resample \| migrate \| schema}` | Manage log sources and [their schemas](connect-source.md#sampling-logs-for-a-schema) |
| `weft integration {list \| slack \| pagerduty}` | Connect [notification](alerts.md) destinations |
| `weft mcp` | Run an MCP server on stdio, so an LLM client such as Claude Code can read your logs and propose scripts and alerting crons |
| `weft propose {review \| apply \| clear}` | Review, apply or discard the changes an MCP client has staged |
| `weft update` | Replace the binary with the latest release |
| `weft version` | Print the version |

Run `weft <command> --help` for the details of any command.

### Options for every command

| Option | Effect |
|---|---|
| `--org NAME`, `-o NAME` | Use this org for the one invocation instead of the saved one. |
| `--nocolor` | Turn off colored output. Color is also off when the output is piped, and `NO_COLOR` is honored. |

### Environment variables

| Variable | Effect |
|---|---|
| `WEFT_TOKEN` | Use this token instead of the one in `.weft.yaml`. |
| `WEFT_ORG` | Use this org instead of the one in `.weft.yaml`. `--org` wins over it. |
