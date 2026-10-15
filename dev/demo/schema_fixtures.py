"""Fixtures for exercising the schema wizard (browser and CLI).

Each scenario is a fake service whose first 50 lines are process-startup
chatter (no trace IDs, often no level) followed by request traffic where the
interesting values appear -- the situation where weft's first sample doesn't
show everything worth extracting, so you need to resample and walk the
decision tree.

Lines are pushed through the Heroku log-drain endpoint, so no agent is
needed: a service is auto-created on first push and samples the first 50
lines it sees.

    # 1. startup chatter only -> sampling 50/50 -> pending, sample has no trace
    pipenv run python dev/demo/schema_fixtures.py startup --token wft_...

    # 2. open Namespaces (or `weft source pending --watch`), see the pending
    #    sample, hit "collect a new sample" / `weft source resample ns/svc`

    # 3. real traffic -> refills the sample with lines that carry the values
    pipenv run python dev/demo/schema_fixtures.py traffic --token wft_...

    pipenv run python dev/demo/schema_fixtures.py list      # scenarios
    pipenv run python dev/demo/schema_fixtures.py clean-hint # reset a service

--token defaults to $WEFT_FIXTURE_TOKEN; --endpoint to http://localhost:8080.
Use --suffix to get fresh services (fx-tagged-2) without removing old ones.
Output is deterministic per scenario (seeded), so tests and screenshots repeat.
"""
import argparse
import os
import random
import sys
import urllib.request
from datetime import datetime, timedelta, timezone

SAMPLE_TARGET = 50  # store.SampleTarget
BASE_TIME = datetime(2026, 10, 15, 12, 0, 0, tzinfo=timezone.utc)


def _hex(rng: random.Random, n: int) -> str:
    return "".join(rng.choice("0123456789abcdef") for _ in range(n))


def _stamp(i: int, comma: bool = False) -> str:
    t = BASE_TIME + timedelta(milliseconds=137 * i)
    ms = f"{t.microsecond // 1000:03d}"
    return t.strftime("%Y-%m-%d %H:%M:%S") + ("," if comma else ".") + ms


PATHS = ["/api/items", "/api/items/42", "/api/orders", "/api/users/me", "/login"]
STARTUP = [
    "Starting {name} v2.3.1 (python 3.12.4)",
    "Loading configuration from /etc/{name}/app.yml",
    "Connecting to postgres at db:5432",
    "Running migrations: 0041_add_index ... OK",
    "Warming cache (1204 keys)",
    "Registered 18 routes",
    "Listening on 0.0.0.0:8080",
]


def _startup_lines(name: str, fmt, n: int) -> list[str]:
    """n startup lines, shaped by fmt(i, message) so they match the traffic
    format on everything except the values of interest."""
    return [fmt(i, STARTUP[i % len(STARTUP)].format(name=name)) for i in range(n)]


# -- scenarios ---------------------------------------------------------------
# Each returns (startup_lines, traffic_lines) for a service name.


def tagged(name: str, traffic: int):
    """Trailing key=value tag on only some lines; health checks have none.
    Needs a whole-line pattern (trace_id=(\\S+)); the tag is on ~40% of lines."""
    rng = random.Random(1)
    startup = _startup_lines(name, lambda i, m: f"{_stamp(i)} {m}", SAMPLE_TARGET)
    out = []
    for i in range(traffic):
        j = SAMPLE_TARGET + i
        if rng.random() < 0.4:
            p = rng.choice(PATHS)
            out.append(f"{_stamp(j)} INFO handled GET {p} {rng.choice([200, 200, 404, 500])} {rng.randint(2, 90)}ms trace_id={_hex(rng, 16)}")
        else:
            out.append(f"{_stamp(j)} DEBUG handled GET /healthz 200 1ms")
    return startup, out


def bracketed(name: str, traffic: int):
    """Level and request id are both bracketed, at a stable position."""
    rng = random.Random(2)
    startup = _startup_lines(name, lambda i, m: f"{_stamp(i, comma=True)} [{name}] {m}", SAMPLE_TARGET)
    out = []
    for i in range(traffic):
        j = SAMPLE_TARGET + i
        lvl = rng.choice(["INFO", "INFO", "WARNING", "ERROR"])
        out.append(f"{_stamp(j, comma=True)} [{lvl}] [req-{_hex(rng, 8)}] user.{rng.choice(['login', 'logout', 'update'])} ok in {rng.randint(3, 120)}ms")
    return startup, out


def jsonl(name: str, traffic: int):
    """JSON lines; the trace id is nested (context.trace.id) and only on
    request lines."""
    import json

    rng = random.Random(3)
    startup = _startup_lines(
        name,
        lambda i, m: json.dumps({"ts": _stamp(i).replace(" ", "T") + "Z", "msg": m}),
        SAMPLE_TARGET,
    )
    out = []
    for i in range(traffic):
        j = SAMPLE_TARGET + i
        rec = {"ts": _stamp(j).replace(" ", "T") + "Z", "level": rng.choice(["info", "info", "warn", "error"]), "msg": "request"}
        if rng.random() < 0.7:
            rec["context"] = {"trace": {"id": _hex(rng, 32)}, "route": rng.choice(PATHS)}
        else:
            rec["msg"] = "tick"
        out.append(json.dumps(rec))
    return startup, out


def celery(name: str, traffic: int):
    """Celery-style: level is INFO/MainProcess (needs a pattern), and the
    correlation id is the task uuid inside square brackets."""
    rng = random.Random(4)
    startup = _startup_lines(name, lambda i, m: f"[{_stamp(i, comma=True)}: INFO/MainProcess] {m}", SAMPLE_TARGET)
    out = []
    for i in range(traffic):
        j = SAMPLE_TARGET + i
        u = f"{_hex(rng, 8)}-{_hex(rng, 4)}-{_hex(rng, 4)}-{_hex(rng, 4)}-{_hex(rng, 12)}"
        proc = rng.choice(["ForkPoolWorker-1", "ForkPoolWorker-2", "MainProcess"])
        out.append(f"[{_stamp(j, comma=True)}: INFO/{proc}] Task app.tasks.send_email[{u}] succeeded in {rng.random():.3f}s: None")
    return startup, out


SCENARIOS = {
    "fx-tagged": tagged,
    "fx-bracketed": bracketed,
    "fx-jsonl": jsonl,
    "fx-celery": celery,
}


# -- transport ---------------------------------------------------------------


def heroku_body(service: str, lines: list[str], start: int = 0) -> bytes:
    """Octet-counted RFC 5424 frames, as Heroku's drain delivers them."""
    frames = []
    for i, line in enumerate(lines):
        ts = (BASE_TIME + timedelta(seconds=start + i)).strftime("%Y-%m-%dT%H:%M:%S.000000+00:00")
        msg = f"<134>1 {ts} host {service} web.1 - - {line}".encode()
        frames.append(f"{len(msg)} ".encode() + msg)
    return b"".join(frames)


def push(endpoint: str, token: str, service: str, lines: list[str], batch: int = 25) -> None:
    for off in range(0, len(lines), batch):
        body = heroku_body(service, lines[off:off + batch], start=off)
        req = urllib.request.Request(
            f"{endpoint.rstrip('/')}/api/ingest/heroku/{token}",
            data=body,
            headers={"Content-Type": "application/logplex-1"},
            method="POST",
        )
        with urllib.request.urlopen(req) as resp:
            if resp.status not in (200, 204):
                raise RuntimeError(f"ingest returned {resp.status}")


def main(argv=None) -> int:
    ap = argparse.ArgumentParser(description=__doc__.split("\n\n")[0])
    ap.add_argument("command", choices=["startup", "traffic", "list", "clean-hint"])
    ap.add_argument("--token", default=os.environ.get("WEFT_FIXTURE_TOKEN", ""))
    ap.add_argument("--endpoint", default="http://localhost:8080")
    ap.add_argument("--suffix", default="", help="appended to service names, e.g. -2")
    ap.add_argument("--only", help="comma-separated scenario names")
    ap.add_argument("--count", type=int, default=200, help="traffic lines per scenario")
    args = ap.parse_args(argv)

    chosen = SCENARIOS
    if args.only:
        want = args.only.split(",")
        unknown = [w for w in want if w not in SCENARIOS]
        if unknown:
            ap.error(f"unknown scenario(s): {', '.join(unknown)}")
        chosen = {k: v for k, v in SCENARIOS.items() if k in want}

    if args.command == "list":
        for name, fn in SCENARIOS.items():
            print(f"{name:14s} {(fn.__doc__ or '').strip().splitlines()[0]}")
        return 0
    if args.command == "clean-hint":
        for name in chosen:
            print(f"weft source remove --namespace <ns> --service {name}{args.suffix}")
        return 0
    if not args.token:
        ap.error("--token (or $WEFT_FIXTURE_TOKEN) is required")

    for name, fn in chosen.items():
        service = name + args.suffix
        startup, traffic = fn(service, args.count)
        lines = startup if args.command == "startup" else traffic
        push(args.endpoint, args.token, service, lines)
        print(f"pushed {len(lines):4d} {args.command} lines to {service}")
    return 0


if __name__ == "__main__":
    sys.exit(main())
