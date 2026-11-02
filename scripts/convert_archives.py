#!/usr/bin/env python3
"""Convert the existing cold archives to the sized layout, end to end.

docs/plans/per-service-tables.md, "Archive layout", explains the layout. This
reads each shard's export manifest from the cold bucket, and for every archive
not yet sized: downloads it, converts it with `convert-archive`
(which checks the copy against the original, key by key, and against the
manifest), uploads the copy beside the original, checks the upload, and swaps
each coordinator's shard to it with `weft-admin swap-archive`. The old archive
is tombstoned for deletion after a week, so a swap can be undone until then.

Stages, each including the ones before it:

    plan      list the archives and what would be done (the default; no downloads)
    convert   download, convert and verify, locally
    apply     upload, verify the upload, swap

Progress is kept in <workdir>/state.json, so a rerun resumes where it stopped.
All AWS calls go through `kevcons aws`. Stdlib only; run with python3.

    python3 scripts/convert_archives.py --stage plan
    python3 scripts/convert_archives.py --stage convert --shard shard-20261012T0000-a85dbee1
    python3 scripts/convert_archives.py --stage apply
"""

import argparse
import json
import os
import re
import shlex
import shutil
import subprocess
import sys
import time

BUCKET = "weft-cold-220664822695"
SIZED = re.compile(r"/sized/\*\*/\*\.parquet$")
LAYOUT_DIR = re.compile(r"/(m[1-9][0-9]*|k[1-9][0-9]*|sized)$")
STAGES = ("plan", "convert", "apply")
SPILL_BYTES = 4 << 30
SWAP_OK =("swapped to", "already at the new path")


class Runner:
    """Runs commands. Tests replace it."""

    def run(self, args, check=True):
        p = subprocess.run(args, capture_output=True, text=True)
        if check and p.returncode != 0:
            raise RuntimeError(f"{' '.join(args[:6])} ... failed ({p.returncode}): {p.stderr.strip()[-800:]}")
        return p.stdout

    def stream(self, args):
        """Runs a command whose output is progress; raises if it fails."""
        if subprocess.run(args).returncode != 0:
            raise RuntimeError(f"{' '.join(args[:6])} ... failed")


def aws(r, *args, check=True):
    return r.run(["kevcons", "aws", *args], check=check)


def archive_dir(parquet_path):
    """The directory (or, for a single file, the object) that holds an archive,
    as an s3:// URL: a glob without its /**/*.parquet and its layout directory."""
    if parquet_path.endswith("/**/*.parquet"):
        return LAYOUT_DIR.sub("", parquet_path[: -len("/**/*.parquet")]) + "/"
    return parquet_path


def new_dir(new_path):
    """The directory a converted archive is uploaded to."""
    return new_path[: -len("/sized/**/*.parquet")] + "/"


def key_of(url):
    return url.split("://", 1)[1].split("/", 1)[1]


def list_manifests(r, bucket):
    """Every export manifest in the bucket, as (url, parsed json)."""
    out = []
    for line in aws(r, "s3", "ls", f"s3://{bucket}/").splitlines():
        m = re.match(r"\s+PRE (\S+/)$", line)
        if not m or m.group(1).startswith("_"):
            continue
        prefix = m.group(1)
        for ml in aws(r, "s3", "ls", f"s3://{bucket}/{prefix}_manifests/", check=False).splitlines():
            parts = ml.split()
            if len(parts) == 4 and parts[3].endswith(".json"):
                url = f"s3://{bucket}/{prefix}_manifests/{parts[3]}"
                out.append((url, json.loads(aws(r, "s3", "cp", url, "-"))))
    return out


def plan(manifests, only):
    """The archives to convert: not already sized, and in `only` if it is given."""
    todo = []
    for url, m in manifests:
        if only and m["shard_id"] not in only:
            continue
        if SIZED.search(m["parquet_path"]):
            continue
        todo.append((url, m))
    return sorted(todo, key=lambda t: t[1]["shard_id"])


class State:
    def __init__(self, path):
        self.path = path
        self.d = {}
        if os.path.exists(path):
            with open(path) as f:
                self.d = json.load(f)

    def get(self, shard):
        return self.d.setdefault(shard, {})

    def save(self):
        tmp = self.path + ".tmp"
        with open(tmp, "w") as f:
            json.dump(self.d, f, indent=2)
        os.replace(tmp, self.path)


def remove(path):
    """Deletes a downloaded or converted archive: a directory, or a single file."""
    if os.path.isdir(path):
        shutil.rmtree(path, ignore_errors=True)
    elif os.path.exists(path):
        os.remove(path)


def log(msg):
    print(time.strftime("%H:%M:%SZ", time.gmtime()), msg, flush=True)


def download(r, bucket, m, root):
    src = archive_dir(m["parquet_path"])
    dest = os.path.join(root, key_of(src))
    if src.endswith("/"):
        os.makedirs(dest, exist_ok=True)
        r.stream(["kevcons", "aws", "s3", "sync", src, dest, "--only-show-errors"])
    else:
        os.makedirs(os.path.dirname(dest), exist_ok=True)
        r.stream(["kevcons", "aws", "s3", "cp", src, dest, "--only-show-errors"])


def convert(r, converter, manifest_file, root, out):
    stdout = r.run([converter, "--manifest", manifest_file, "--root", root, "--out", out])
    res = json.loads(stdout.strip().splitlines()[-1])
    if res["status"] != "converted":
        raise RuntimeError(f"unexpected conversion result: {res}")
    return res


def upload(r, bucket, res, out):
    dst = new_dir(res["new_path"])
    local = os.path.join(out, key_of(dst))
    r.stream(["kevcons", "aws", "s3", "sync", local, dst, "--only-show-errors"])
    # What is in S3 must be what was converted: every file, every byte.
    n = size = 0
    for line in aws(r, "s3", "ls", dst, "--recursive").splitlines():
        parts = line.split(None, 3)
        if len(parts) == 4 and parts[3].endswith(".parquet"):
            n, size = n + 1, size + int(parts[2])
    if n != res["files"] or size != res["bytes"]:
        raise RuntimeError(f"uploaded {n} files / {size} bytes to {dst}, converted {res['files']} / {res['bytes']}")


def coordinators(r):
    out = aws(r, "ec2", "describe-instances", "--filters", "Name=tag:Role,Values=coordinator",
              "Name=tag:Application,Values=weft", "Name=instance-state-name,Values=running",
              "--query", "Reservations[].Instances[].InstanceId", "--output", "text")
    ids = out.split()
    if not ids:
        raise RuntimeError("no running coordinators found")
    return ids


def swap(r, shard_id, old, new, sleep=time.sleep):
    """Swaps the shard on every coordinator. Each keeps its own registry, and the
    database is shared, so every one is told; all but the first find it done."""
    cmd = ("/opt/app/bin/weft-admin-run swap-archive --yes --shard %s --old %s --new %s"
           % (shlex.quote(shard_id), shlex.quote(old), shlex.quote(new)))
    ids = coordinators(r)
    cid = aws(r, "ssm", "send-command", "--instance-ids", *ids, "--document-name", "AWS-RunShellScript",
              "--comment", f"swap archive of {shard_id}", "--parameters", json.dumps({"commands": [cmd]}),
              "--query", "Command.CommandId", "--output", "text").strip()
    for inst in ids:
        for _ in range(60):
            sleep(2)
            p = aws(r, "ssm", "get-command-invocation", "--command-id", cid, "--instance-id", inst,
                    "--query", "[Status,StandardOutputContent,StandardErrorContent]", "--output", "json", check=False)
            try:
                status, stdout, stderr = json.loads(p)
            except ValueError:
                continue  # not registered yet
            if status in ("Pending", "InProgress", "Delayed"):
                continue
            if status != "Success" or not any(ok in stdout for ok in SWAP_OK):
                raise RuntimeError(f"swap on {inst} {status}: {stdout.strip()} {stderr.strip()}")
            break
        else:
            raise RuntimeError(f"swap on {inst} did not finish")


def process(r, args, state, url, m):
    shard_id = m["shard_id"]
    st = state.get(shard_id)
    root, out = os.path.join(args.workdir, "root"), os.path.join(args.workdir, "out")
    manifest_file = os.path.join(args.workdir, f"{shard_id}.manifest.json")

    if st.get("swapped"):
        log(f"{shard_id}: already swapped to {st['result']['new_path']}")
        return
    if not st.get("converted"):
        # the download, the converted copy, and room for DuckDB to spill
        need = 2 * args.size_hint(m) + SPILL_BYTES
        free = shutil.disk_usage(args.workdir).free
        if free < need:
            raise RuntimeError(f"{shard_id}: needs about {need >> 20} MiB free, {free >> 20} MiB available in {args.workdir}")
        log(f"{shard_id}: downloading {archive_dir(m['parquet_path'])}")
        download(r, args.bucket, m, root)
        with open(manifest_file, "w") as f:
            json.dump(m, f)
        log(f"{shard_id}: converting {m['rows']} rows")
        st["result"] = convert(r, args.convert_archive, manifest_file, root, out)
        st["converted"] = True
        state.save()
        res = st["result"]
        log(f"{shard_id}: converted and verified: {res['keys']} keys, {res['files']} files, {res['bytes'] >> 20} MiB")
    if args.stage == "convert":
        return
    res = st["result"]
    if not st.get("uploaded"):
        log(f"{shard_id}: uploading to {new_dir(res['new_path'])}")
        upload(r, args.bucket, res, out)
        st["uploaded"] = True
        state.save()
    log(f"{shard_id}: swapping {res['old_path']} -> {res['new_path']}")
    swap(r, shard_id, res["old_path"], res["new_path"])
    st["swapped"] = True
    state.save()
    log(f"{shard_id}: swapped. The old archive is tombstoned and deleted after 7 days; to undo before then, "
        f"swap back: weft-admin swap-archive --shard {shard_id} --old '{res['new_path']}' --new '{res['old_path']}' --yes")
    if not args.keep:
        remove(os.path.join(root, key_of(archive_dir(m["parquet_path"]))))
        remove(os.path.join(out, key_of(new_dir(res["new_path"]))))


def main(argv=None, r=None):
    ap = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument("--stage", choices=STAGES, default="plan")
    ap.add_argument("--shard", action="append", default=[], help="only this shard id (repeatable)")
    ap.add_argument("--bucket", default=BUCKET)
    ap.add_argument("--workdir", default=os.path.expanduser("~/weft-archive-conversion"))
    ap.add_argument("--convert-archive", default=None, help="path to a convert-archive built from this tree (default: build one in the workdir)")
    ap.add_argument("--keep", action="store_true", help="keep the downloaded and converted files after a swap")
    args = ap.parse_args(argv)
    r = r or Runner()
    os.makedirs(args.workdir, exist_ok=True)

    todo = plan(list_manifests(r, args.bucket), set(args.shard))
    # bytes: an upper bound on the archive, from the shard database or ~200 bytes a row, whichever is larger
    args.size_hint = lambda m: max(m.get("db_bytes") or 0, m["rows"] * 200)
    if not todo:
        log("nothing to convert: every archive is already sized")
        return 0
    for url, m in todo:
        log(f"{m['shard_id']}: {m['rows']} rows at {m['parquet_path']}")
    if args.stage == "plan":
        log(f"{len(todo)} archives to convert. Re-run with --stage convert, then --stage apply.")
        return 0

    if not args.convert_archive:
        args.convert_archive = os.path.join(args.workdir, "convert-archive")
        log(f"building {args.convert_archive}")
        r.stream(["go", "build", "-o", args.convert_archive, "./cmd/convert-archive"])
    state = State(os.path.join(args.workdir, "state.json"))
    failed = []
    for url, m in todo:
        try:
            process(r, args, state, url, m)
        except Exception as e:  # keep going: each shard is independent, and the state is saved
            log(f"{m['shard_id']}: FAILED: {e}")
            failed.append(m["shard_id"])
    if failed:
        log(f"{len(failed)} failed: {', '.join(failed)}. Fix and re-run: finished stages are skipped.")
        return 1
    log("done")
    return 0


if __name__ == "__main__":
    sys.exit(main())
