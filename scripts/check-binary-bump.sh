#!/bin/sh
# Usage: check-binary-bump.sh <weft|weftd> <base-ref>
#
# Fails when any source that gets linked into <binary> changed between
# <base-ref> and HEAD but cmd/<binary>/version.go still carries the same
# Version. Self-update only moves a binary to a strictly newer version, so
# an unbumped change is never released and never reaches users.
#
# "Linked into" is computed, not listed: the in-module packages in
# `go list -deps ./cmd/<binary>` (both platforms), plus go.mod/go.sum
# (dependency bumps, e.g. warp). Tests, docs and testdata don't count.
#
# Callers:
#   - CI, on PRs:      BASE = origin/<base branch>
#   - release.yml:     BASE = the binary's latest <binary>/v* tag, which
#                      catches direct pushes to main that skipped the PR gate
#
# Opt out with the token `no-binary-change` in any commit message in
# <base-ref>..HEAD (e.g. a refactor that provably doesn't change behavior).
set -eu

bin="${1:?usage: check-binary-bump.sh <weft|weftd> <base-ref>}"
base="${2:?usage: check-binary-bump.sh <weft|weftd> <base-ref>}"
vfile="cmd/$bin/version.go"
module="$(GOWORK=off go list -m)"
root="$(git rev-parse --show-toplevel)"

# -e: sibling-checkout replaces (../warp) may be absent in CI; we only need
# the in-module packages, which resolve regardless. GOWORK=off keeps a
# developer's go.work from changing the answer.
dirs="$(
  for os in linux darwin; do
    GOWORK=off GOOS="$os" go list -e -tags release -deps -f '{{.ImportPath}} {{.Dir}}' "./cmd/$bin" 2>/dev/null
  done | awk -v m="$module" -v root="$root" '$1 == m || index($1, m "/") == 1 { sub(root "/", "", $2); print $2 }' | sort -u
)"
[ -n "$dirs" ] || { echo "no in-module packages found for ./cmd/$bin" >&2; exit 2; }

# Whether a changed path is a linked source: a non-test file directly inside
# one of the closure's package directories, or a module file.
is_linked() {
  case "$1" in
    go.mod | go.sum) return 0 ;;
    *_test.go | *.md) return 1 ;;
  esac
  printf '%s\n' "$dirs" | grep -qxF "$(dirname "$1")"
}

changed=""
for f in $(git diff --name-only "$base"..HEAD); do
  if is_linked "$f"; then changed="$changed$f
"; fi
done
changed="$(printf '%s' "$changed")"
[ -n "$changed" ] || exit 0

version_at() { git show "$1:$vfile" 2>/dev/null | sed -n 's/^var Version = "\(.*\)"/\1/p'; }
old="$(version_at "$base")"
new="$(version_at HEAD)"
if [ -n "$new" ] && [ "$new" != "$old" ]; then
  exit 0
fi

if git log --format='%B' "$base"..HEAD | grep -q 'no-binary-change'; then
  echo "$bin sources changed but bypass requested via 'no-binary-change' token"
  exit 0
fi

cat >&2 <<MSG

✗ $bin version bump required

These files are linked into $bin and changed since $base:
$(printf '%s\n' "$changed" | sed 's/^/  /')

…but $vfile still says Version = "$new". Self-update only moves users to a
strictly newer version, so this change would never be released.

  • Bump Version in $vfile (then release: kevcons ext release $bin), or
  • if the change can't affect $bin's behavior, put the token
    'no-binary-change' in a commit message.

See docs/versioning.md.
MSG
exit 1
