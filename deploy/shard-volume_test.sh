#!/bin/bash
# Tests for deploy/shard-volume.sh's device lookup and argument handling.
# Run via `make test-deploy`. Returns non-zero on first failure so CI can flag.

set -uo pipefail

LIB="$(cd "$(dirname "$0")" && pwd)/shard-volume.sh"
PASS=0
FAIL=0

assert_eq() {
    local name="$1" want="$2" got="$3"
    if [ "$want" = "$got" ]; then
        PASS=$((PASS + 1))
        printf "  ok %s\n" "$name"
    else
        FAIL=$((FAIL + 1))
        printf "  FAIL %s\n    want: %q\n     got: %q\n" "$name" "$want" "$got"
    fi
}

section() { printf "%s\n" "── $1 ──"; }

WORK=$(mktemp -d)
trap 'rm -rf "$WORK"' EXIT

# A fake /dev and a fake ebsnvme-id that reports the attach-time name
# recorded next to each fake nvme node.
mkdir -p "$WORK/dev" "$WORK/bin"
cat >"$WORK/bin/ebsnvme-id" <<'EOF'
#!/bin/bash
echo "Volume ID: vol-test"
echo "Disk: $(cat "$1.requested")"
EOF
chmod +x "$WORK/bin/ebsnvme-id"
export PATH="$WORK/bin:$PATH"
export SHARD_VOLUME_DEV_ROOT="$WORK/dev"

# shellcheck source=shard-volume.sh
. "$LIB"

section "resolve_device"
assert_eq "nothing attached" "" "$(resolve_device)"

touch "$WORK/dev/nvme1n1"
echo "/dev/xvda" >"$WORK/dev/nvme1n1.requested"
assert_eq "ignores a disk attached under another name" "" "$(resolve_device)"

touch "$WORK/dev/nvme2n1"
echo "sdf" >"$WORK/dev/nvme2n1.requested"
assert_eq "finds the nvme disk attached as sdf" "$WORK/dev/nvme2n1" "$(resolve_device)"

rm "$WORK/dev/nvme2n1" "$WORK/dev/nvme2n1.requested"
touch "$WORK/dev/sdf"
assert_eq "falls back to a literal /dev/sdf" "$WORK/dev/sdf" "$(resolve_device)"
rm "$WORK/dev/sdf"

section "wait_for_device"
slept=0
sleep() { [ "$1" = "0.1" ] && slept=$((slept + 1)); } # count, don't wait
wait_for_device 4 >/dev/null
assert_eq "gives up after --within with nothing attached" "1" "$?"
assert_eq "polls every 100ms for --within seconds" "40" "$slept"
touch "$WORK/dev/sdf"
assert_eq "returns the device once it appears" "$WORK/dev/sdf" "$(wait_for_device 0)"
unset -f sleep

section "argument handling"
for args in "" "frobnicate" "mount --within" "mount --within ten" "mount extra" "unmount extra"; do
    # shellcheck disable=SC2086
    bash "$LIB" $args >/dev/null 2>&1
    assert_eq "rejects '$args'" "2" "$?"
done

printf "\n%d passed, %d failed\n" "$PASS" "$FAIL"
[ "$FAIL" -eq 0 ]
