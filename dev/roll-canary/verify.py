"""Reader side of the roll canary: did every row the canary sent come back?

docs/plans/shard-seamless-upgrade-testing.md. canary.py numbers its rows
0..N-1 and prints N; this queries the canary's service back through the real
`weft` CLI and checks that each sequence number arrived exactly once.

    pipenv run python verify.py --run <id> --sent <N> --range <start>..<end>
"""

import argparse
import re
import subprocess
import sys
from collections import Counter
from dataclasses import dataclass, field

SERVICE_LOG = "/log/weft/roll-canary/main.log"


def parse_seq(line, run):
    m = re.search(rf"canary run={re.escape(run)} seq=(\d+) ", line)
    return int(m.group(1)) if m else None


@dataclass
class Account:
    sent: int
    received: int
    missing: list = field(default_factory=list)
    duplicates: dict = field(default_factory=dict)
    unexpected: list = field(default_factory=list)

    @property
    def ok(self):
        return not (self.missing or self.duplicates or self.unexpected)


def account(sent, received):
    counts = Counter(received)
    return Account(
        sent=sent,
        received=len(received),
        missing=[s for s in range(sent) if s not in counts],
        duplicates={s: n for s, n in sorted(counts.items()) if n > 1},
        unexpected=sorted(s for s in counts if s >= sent),
    )


def fetch(weft, run, rng):
    p = subprocess.run(
        [weft, "sh", "--range", rng, "-c", f"cat {SERVICE_LOG} | grep 'run={run} '"],
        capture_output=True, text=True,
    )
    if p.returncode != 0:
        sys.exit(f"query failed: {p.stderr.strip() or p.stdout.strip()}")
    return [s for s in (parse_seq(l, run) for l in p.stdout.splitlines()) if s is not None]


def main():
    ap = argparse.ArgumentParser(description=__doc__.split("\n")[0])
    ap.add_argument("--run", required=True)
    ap.add_argument("--sent", type=int, required=True)
    ap.add_argument("--range", required=True, help="weft --range covering the whole run")
    ap.add_argument("--weft", default="./weft")
    a = ap.parse_args()

    r = account(a.sent, fetch(a.weft, a.run, a.range))
    print(f"sent={r.sent} received={r.received} missing={len(r.missing)} "
          f"duplicated={len(r.duplicates)} unexpected={len(r.unexpected)}")
    if r.missing:
        print("missing seqs:", r.missing[:50], "..." if len(r.missing) > 50 else "")
    if r.duplicates:
        print("duplicate seqs (seq: copies):", dict(list(r.duplicates.items())[:50]))
    print("canary: " + ("exact" if r.ok else "FAILED"))
    sys.exit(0 if r.ok else 1)


if __name__ == "__main__":
    main()
