#!/bin/sh
# Fails CI when a PR touches wire-affecting paths but doesn't bump one of
# the two binary Version consts. Pure-server changes that don't affect
# the wire (e.g. an api/ internal refactor) can opt out by including the
# token `no-wire-change` anywhere in the PR's commit messages.
#
# Wire-affecting paths: anything that defines what bytes flow over the
# weft↔server contract — protos, the API server's request struct, the
# CLI/agent's transport layer, the WASM/JS network shim, and the shared
# helpers those depend on (e.g. internal/timerange, which is parsed
# server-side).
#
# This is intentionally a conservative *over*-trigger. False positives
# are cheap (add 'no-wire-change' to the commit body); false negatives
# silently ship an incompatible binary to a customer.
set -eu

WIRE_PATHS='^(proto/|api/|coordinator/service/|shell/http_remote\.go|frontend/src/wasm/network\.ts|cmd/weft/|cmd/weftd/|weftd/|internal/timerange/)'
VERSION_FILES='cmd/weft/version\.go|cmd/weftd/version\.go'

# Base ref defaults to origin/main; override via $BASE_REF for branches
# that target something else (e.g. a release branch).
BASE_REF="${BASE_REF:-origin/main}"

changed="$(git diff --name-only "$BASE_REF"..HEAD)"

if [ -z "$changed" ]; then
  echo "no changed files; nothing to check"
  exit 0
fi

touched_wire="$(printf '%s\n' "$changed" | grep -E "$WIRE_PATHS" || true)"
touched_version="$(printf '%s\n' "$changed" | grep -E "$VERSION_FILES" || true)"

if [ -z "$touched_wire" ]; then
  exit 0
fi

# Check for the bypass token across all commit messages in the range.
commit_msgs="$(git log --format='%B' "$BASE_REF"..HEAD)"
if printf '%s' "$commit_msgs" | grep -q 'no-wire-change'; then
  echo "wire-affecting paths touched but bypass requested via 'no-wire-change' token:"
  printf '%s\n' "$touched_wire" | sed 's/^/  /'
  exit 0
fi

if [ -n "$touched_version" ]; then
  exit 0
fi

cat <<EOF >&2

✗ wire-bump tripwire failed

The following paths affect the weft↔server wire contract:
$(printf '%s\n' "$touched_wire" | sed 's/^/  /')

…but neither cmd/weft/version.go nor cmd/weftd/version.go was modified.

Pick one:

  • If this PR changes the wire contract, bump the binary's Version const
    (cmd/weft/version.go for the CLI, cmd/weftd/version.go for the agent),
    then re-push. The release ext will publish the new artifact at
    releases.w3ft.net/<binary>/v<version>/.

  • If this PR truly doesn't affect the wire (e.g. server-internal
    refactor, comment-only change, test-only edit), include the literal
    token 'no-wire-change' somewhere in a commit message.

See docs/versioning.md for the full protocol.
EOF
exit 1
