"""Before/after harness for docs/plans/field-types.md.

    pipenv run python bench/fieldtypes/run.py record baseline [--runs 5] [--note TEXT]
    pipenv run python bench/fieldtypes/run.py record after
    pipenv run python bench/fieldtypes/run.py compare baseline after

record runs every query in queries.json through `weft sh -c` --runs times and
saves, per query, the wall times, a hash and sample of the output, and the
`explain` text, to results/<label>.json. compare prints the change per query
and exits 1 if any query's output differs between the two labels: a field fact
must change how a query runs, never what it answers.
"""
import argparse
import hashlib
import json
import statistics
import subprocess
import sys
import time
from datetime import datetime, timezone
from pathlib import Path

HERE = Path(__file__).parent
RESULTS = HERE / "results"
SAMPLE_BYTES = 2000


def weft(*args, timeout=170):
    return subprocess.run(["weft", "--nocolor", *args], capture_output=True, text=True, timeout=timeout)


def sh(rng, pipeline, retries=2):
    """Run one pipeline; a client-side timeout is retried, anything else is final.
    Returns (result, retries used)."""
    for used in range(retries + 1):
        r = weft("sh", "--range", rng, "-c", pipeline)
        if r.returncode == 0 or "Client.Timeout" not in r.stderr:
            return r, used
    return r, retries


def record(label, runs, note, only):
    spec = json.loads((HERE / "queries.json").read_text())
    path = RESULTS / f"{label}.json"
    out = json.loads(path.read_text()) if only and path.exists() else {
        "label": label,
        "recorded_at": datetime.now(timezone.utc).isoformat(timespec="seconds"),
        "weft_cli": weft("version").stdout.strip(),
        "git_head": subprocess.run(["git", "rev-parse", "HEAD"], capture_output=True, text=True, cwd=HERE).stdout.strip(),
        "range": spec["range"],
        "runs": runs,
        "note": note,
        "queries": {},
    }
    out["queries"] = out.get("queries", {})
    out.setdefault("queries", {})
    for q in spec["queries"]:
        if only and q["id"] not in only:
            continue
        times, first, retried = [], None, 0
        for _ in range(runs):
            t = time.monotonic()
            r, used = sh(spec["range"], q["pipeline"])
            retried += used
            # A retried run's time includes the timeout, so it isn't kept.
            if not used:
                times.append(round(time.monotonic() - t, 3))
            if r.returncode != 0:
                sys.exit(f"{q['id']}: weft exited {r.returncode}: {r.stderr.strip()}")
            if first is None:
                first = r.stdout
            elif r.stdout != first:
                sys.exit(f"{q['id']}: output changed between runs; the range is not stable")
        if not times:
            sys.exit(f"{q['id']}: every run timed out")
        explain = weft("sh", "--range", spec["range"], "-c", "explain " + q["pipeline"])
        out["queries"][q["id"]] = {
            "kind": q["kind"],
            "times_s": times,
            "client_timeouts_retried": retried,
            "median_s": round(statistics.median(times), 3),
            "output_sha256": hashlib.sha256(first.encode()).hexdigest(),
            "output_sample": first[:SAMPLE_BYTES],
            "explain": (explain.stdout or explain.stderr).strip(),
        }
        print(f"{q['id']:<24} median {out['queries'][q['id']]['median_s']:>6.2f}s  {out['queries'][q['id']]['explain'].splitlines()[0][:60]}")
    RESULTS.mkdir(exist_ok=True)
    (RESULTS / f"{label}.json").write_text(json.dumps(out, indent=2) + "\n")
    print(f"wrote {RESULTS / (label + '.json')}")


def compare(before, after):
    a = json.loads((RESULTS / f"{before}.json").read_text())
    b = json.loads((RESULTS / f"{after}.json").read_text())
    if a["range"] != b["range"]:
        sys.exit(f"ranges differ: {a['range']} vs {b['range']}")
    bad = 0
    print(f"{'query':<24} {'kind':<8} {before:>9} {after:>9}  change  plan")
    for qid, x in a["queries"].items():
        y = b["queries"].get(qid)
        if y is None:
            print(f"{qid:<24} missing from {after}")
            continue
        same = x["output_sha256"] == y["output_sha256"]
        bad += not same
        ratio = y["median_s"] / x["median_s"] if x["median_s"] else float("nan")
        plan = "same plan" if x["explain"] == y["explain"] else "PLAN CHANGED"
        flag = "" if same else "  OUTPUT DIFFERS"
        print(f"{qid:<24} {x['kind']:<8} {x['median_s']:>8.2f}s {y['median_s']:>8.2f}s  x{ratio:<5.2f} {plan}{flag}")
    sys.exit(1 if bad else 0)


def main():
    p = argparse.ArgumentParser()
    sub = p.add_subparsers(dest="cmd", required=True)
    r = sub.add_parser("record")
    r.add_argument("label")
    r.add_argument("--runs", type=int, default=5)
    r.add_argument("--note", default="")
    r.add_argument("--only", default="", help="comma-separated ids: add these to an existing label")
    c = sub.add_parser("compare")
    c.add_argument("before")
    c.add_argument("after")
    args = p.parse_args()
    if args.cmd == "record":
        record(args.label, args.runs, args.note, set(filter(None, args.only.split(","))))
    else:
        compare(args.before, args.after)


if __name__ == "__main__":
    main()
