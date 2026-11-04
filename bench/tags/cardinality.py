"""Cardinality benchmark for the custom tags design (docs/plans/custom-tags.md).

Needs the duckdb CLI on PATH. Writes ~1 GB of scratch files under DIR.

    python3 bench/tags/cardinality.py DIR
"""
import os, re, subprocess, sys, tempfile
D = sys.argv[1]
N = 5_000_000
CASES = [("baseline (no tags)", None), ("5 tags x 10 values", 10), ("5 tags x 1k values", 1_000),
         ("5 tags x 100k values", 100_000), ("5 tags x 5M values (unique)", N),
         ("realistic mix (20/3/8/4/500)", "mix")]

def duck(script, db=":memory:"):
    p = subprocess.run(["duckdb", db], input=script, capture_output=True, text=True)
    if p.returncode: raise SystemExit(p.stderr)
    return p.stdout

def times(out):
    return [float(x) for x in re.findall(r"Run Time \(s\): real ([\d.]+)", out)]

def mapexpr(c):
    if c is None: return None
    if c == "mix":
        return "map(['team','env','region','tier','build'],['t'||(i%20),'e'||(i%3),'r'||(i%8),'x'||(i%4),'b'||(i%500)])"
    return f"map(['k1','k2','k3','k4','k5'],['v'||((i*7)%{c}),'v'||((i*11)%{c}),'v'||((i*13)%{c}),'v'||((i*17)%{c}),'v'||((i*19)%{c})])"

print(f"{'case':32}{'parquet MB':>11}{'hot db MB':>10}{'filter ms':>10}{'filter 1thr':>12}{'groupby ms':>11}{'raw regex ms':>13}")
for name, c in CASES:
    pq, db = f"{D}/t.parquet", f"{D}/t.duckdb"
    for f in (pq, db, db + ".wal"):
        if os.path.exists(f): os.remove(f)
    m = mapexpr(c)
    cols = "i AS id, md5(i::text)||md5((i*3)::text)||md5((i*5)::text)||md5((i*7)::text)||' some log line text' AS raw" + (f", {m} AS tags" if m else "")
    src = f"(SELECT {cols} FROM range({N}) t(i))"
    duck(f"COPY {src} TO '{pq}' (FORMAT parquet, COMPRESSION zstd);")
    duck(f"CREATE TABLE logs AS SELECT * FROM {src}; CHECKPOINT;", db)
    key = "tags['k1']" if c not in (None, "mix") else "tags['team']"
    val = "'v7'" if c not in (None, "mix") else "'t7'"
    filt_pq = f"SELECT count(*) FROM read_parquet('{pq}') WHERE {key} = {val};" if c is not None else f"SELECT count(*) FROM read_parquet('{pq}') WHERE id % 50 = 7;"
    grp_pq = f"SELECT {key}, count(*) FROM read_parquet('{pq}') GROUP BY 1 ORDER BY 2 DESC LIMIT 3;" if c is not None else f"SELECT id % 10, count(*) FROM read_parquet('{pq}') GROUP BY 1 ORDER BY 2 DESC LIMIT 3;"
    reps = lambda q: ".timer on\n" + q * 3
    f_ms = min(times(duck(reps(filt_pq)))) * 1000
    f1_ms = min(times(duck("SET threads=1;\n" + reps(filt_pq)))) * 1000
    g_ms = min(times(duck(reps(grp_pq)))) * 1000
    raw_ms = min(times(duck(reps(f"SELECT count(*) FROM read_parquet('{pq}') WHERE regexp_matches(raw, 'zzz-no-such');")))) * 1000
    print(f"{name:32}{os.path.getsize(pq)/1e6:>11.0f}{os.path.getsize(db)/1e6:>10.0f}{f_ms:>10.0f}{f1_ms:>12.0f}{g_ms:>11.0f}{raw_ms:>13.0f}", flush=True)
