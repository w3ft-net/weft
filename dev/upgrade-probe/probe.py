"""Caller's-view probe for the seamless shard upgrade live tests.

docs/plans/shard-seamless-upgrade-testing.md, "Before any test" step 2. Once a
second it runs one cheap query per target through the real `weft` CLI and
appends timestamp, latency, row count and error to a CSV. Run it for the whole
session; Ctrl-C prints the summary.

Targets:
  current  rolling last 5m: the INGESTING shards, and proof ingest is landing
  w1012    fixed 5m slice inside the 2026-10-12 window (both stripes)
  w1019    fixed 5m slice inside the 2026-10-19 window (both stripes)

A fixed slice is immutable, so its row count must never change. A different
count means a shard answered with less than it holds: a partial result, which
is a failure even though the query "succeeded". A query that errors is a
failure; a query slower than --stall is flagged as a stall.

    pipenv run python probe.py --out /tmp/probe.csv
"""

import argparse
import csv
import signal
import statistics
import subprocess
import threading
import time
from datetime import datetime, timezone

TARGETS = {
    "current": "5m..",
    "w1012": "2026-10-14T12:00..2026-10-14T12:05",
    "w1019": "2026-10-21T12:00..2026-10-21T12:05",
}
PIPELINE = "cat /log/weft/shard/heartbeats.log | wc -l"
FIELDS = ["ts", "target", "latency_ms", "ok", "rows", "partial", "error"]


def run_query(weft, target, rng, timeout):
    start = time.monotonic()
    try:
        p = subprocess.run(
            [weft, "sh", "--range", rng, "-c", PIPELINE],
            capture_output=True, text=True, timeout=timeout,
        )
    except subprocess.TimeoutExpired:
        return (time.monotonic() - start) * 1000, False, None, f"timeout after {timeout}s"
    ms = (time.monotonic() - start) * 1000
    if p.returncode != 0:
        return ms, False, None, (p.stderr.strip() or p.stdout.strip() or f"exit {p.returncode}")[:300]
    try:
        return ms, True, int(p.stdout.strip()), ""
    except ValueError:
        return ms, False, None, f"unparseable output: {p.stdout.strip()[:100]!r}"


def main():
    ap = argparse.ArgumentParser(description=__doc__.split("\n")[0])
    ap.add_argument("--out", required=True, help="CSV path (appended)")
    ap.add_argument("--weft", default="./weft", help="weft CLI binary")
    ap.add_argument("--interval", type=float, default=1.0)
    ap.add_argument("--timeout", type=float, default=90.0)
    ap.add_argument("--stall", type=float, default=3.0, help="seconds; slower queries are flagged")
    args = ap.parse_args()

    lock = threading.Lock()
    baseline = {}  # fixed targets: first row count seen
    rows = []
    stop = threading.Event()
    signal.signal(signal.SIGINT, lambda *_: stop.set())
    signal.signal(signal.SIGTERM, lambda *_: stop.set())

    f = open(args.out, "a", newline="")
    w = csv.DictWriter(f, FIELDS)
    if f.tell() == 0:
        w.writeheader()

    def probe(target, rng):
        ts = datetime.now(timezone.utc).isoformat(timespec="milliseconds")
        ms, ok, n, err = run_query(args.weft, target, rng, args.timeout)
        partial = False
        with lock:
            if ok and target != "current":
                partial = n != baseline.setdefault(target, n)
            rec = dict(ts=ts, target=target, latency_ms=round(ms), ok=int(ok),
                       rows="" if n is None else n, partial=int(partial), error=err)
            rows.append(rec)
            w.writerow(rec)
            f.flush()
        if not ok or partial or ms > args.stall * 1000:
            tag = "FAIL" if not ok else "PARTIAL" if partial else "STALL"
            print(f"{ts} {tag} {target} {round(ms)}ms rows={n} {err}", flush=True)

    threads = []
    nxt = time.monotonic()
    while not stop.is_set():
        for target, rng in TARGETS.items():
            t = threading.Thread(target=probe, args=(target, rng), daemon=True)
            t.start()
            threads.append(t)
        nxt += args.interval
        stop.wait(max(0, nxt - time.monotonic()))
    for t in threads:
        t.join(args.timeout + 5)
    f.close()

    print("\ntarget    n     fail  partial  p50ms  p99ms  maxms")
    for target in TARGETS:
        rs = [r for r in rows if r["target"] == target]
        if not rs:
            continue
        lat = sorted(r["latency_ms"] for r in rs)
        p99 = lat[min(len(lat) - 1, int(len(lat) * 0.99))]
        print(f"{target:<9} {len(rs):<5} {sum(1 - r['ok'] for r in rs):<5} "
              f"{sum(r['partial'] for r in rs):<8} {round(statistics.median(lat)):<6} {p99:<6} {lat[-1]}")


if __name__ == "__main__":
    main()
