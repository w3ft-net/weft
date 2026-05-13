#!/bin/bash
# install.sh — first-boot installer for the loom-producer host.
#
# Called from the host's user data after fetching the two per-env
# tokens (loom-prod, loom-sand) from Secrets Manager and writing
# them into /etc/default/weftd-loom-{prod,sand}.
#
# Idempotent. Safe to re-run if the user data fires twice or after a
# config bump.
#
# Run order:
#   1. mask the AMI's stock weftd.service (this host has no
#      coordinator/shard/compute role; the inherited ops-token weftd
#      would just spam empty heartbeats)
#   2. ensure /var/log/loom exists (the producer creates per-stream
#      files on demand but the parent directory wants known perms)
#   3. systemctl enable --now the three units
#
# The AMI ships /opt/app/bin/loom-producer, /etc/loom-producer/streams.yaml,
# and the three systemd units already in /etc/systemd/system/.

set -euo pipefail

if ! command -v weftd >/dev/null 2>&1; then
  echo "install.sh: weftd missing — AMI out of date?" >&2
  exit 1
fi
if [ ! -x /opt/app/bin/loom-producer ]; then
  echo "install.sh: /opt/app/bin/loom-producer missing — AMI out of date?" >&2
  exit 1
fi

# The AMI's stock weftd.service expects /weft/WEFTD_TOKEN (an
# ops-side secret). On a loom-producer host that secret isn't
# fetched, so the unit would either crash-loop or run with stale env.
# Masking it makes the inherited unit a no-op without having to
# customize the AMI for this host class.
if systemctl is-enabled --quiet weftd.service 2>/dev/null \
   || systemctl is-active --quiet weftd.service 2>/dev/null; then
  systemctl stop weftd.service || true
  systemctl disable weftd.service || true
fi
systemctl mask weftd.service || true

install -d -m 0755 -o root -g root /var/log/loom

systemctl daemon-reload
systemctl enable --now loom-producer.service
systemctl enable --now weftd-loom@prod.service
systemctl enable --now weftd-loom@sand.service

echo "install.sh: loom-producer + 2× weftd-loom@ started"
