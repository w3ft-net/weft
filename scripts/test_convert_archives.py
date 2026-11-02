"""Tests for convert_archives.py. Run: python3 -m unittest scripts/test_convert_archives.py"""

import json
import os
import sys
import tempfile
import unittest

sys.path.insert(0, os.path.dirname(__file__))
import convert_archives as ca  # noqa: E402

B = "weft-cold-220664822695"


def manifest(shard, path, rows=100):
    return {"shard_id": shard, "parquet_path": path, "rows": rows, "db_bytes": 1 << 20,
            "customers": [{"customer_id": 1, "row_count": rows}]}


class FakeRunner:
    """Answers commands from a list of (substring, response) rules, first match
    wins, and records every command."""

    def __init__(self, rules=()):
        self.rules, self.calls = list(rules), []

    def _answer(self, args):
        line = " ".join(args)
        self.calls.append(line)
        for needle, resp in self.rules:
            if needle in line:
                if isinstance(resp, Exception):
                    raise resp
                return resp(line) if callable(resp) else resp
        return ""

    def run(self, args, check=True):
        return self._answer(args)

    def stream(self, args):
        self._answer(args)

    def saw(self, needle):
        return any(needle in c for c in self.calls)


class PathTests(unittest.TestCase):
    def test_archive_dir(self):
        for path, want in [
            (f"s3://{B}/40-apps/s.attempt-1/m60/**/*.parquet", f"s3://{B}/40-apps/s.attempt-1/"),
            (f"s3://{B}/39-apps/s.attempt-1/k54/**/*.parquet", f"s3://{B}/39-apps/s.attempt-1/"),
            (f"s3://{B}/36-apps/s.attempt-1/**/*.parquet", f"s3://{B}/36-apps/s.attempt-1/"),
            (f"s3://{B}/40-apps/s.attempt-1.r9/sized/**/*.parquet", f"s3://{B}/40-apps/s.attempt-1.r9/"),
            (f"s3://{B}/40-apps/s.parquet", f"s3://{B}/40-apps/s.parquet"),
        ]:
            self.assertEqual(ca.archive_dir(path), want, path)

    def test_new_dir_and_key(self):
        p = f"s3://{B}/40-apps/s.attempt-1.r17/sized/**/*.parquet"
        self.assertEqual(ca.new_dir(p), f"s3://{B}/40-apps/s.attempt-1.r17/")
        self.assertEqual(ca.key_of(ca.new_dir(p)), "40-apps/s.attempt-1.r17/")


class PlanTests(unittest.TestCase):
    def setUp(self):
        self.ms = [
            ("u1", manifest("shard-b", f"s3://{B}/40-apps/b.attempt-1/m1440/**/*.parquet")),
            ("u2", manifest("shard-a", f"s3://{B}/39-apps/a.attempt-1/k54/**/*.parquet")),
            ("u3", manifest("shard-c", f"s3://{B}/40-apps/c.attempt-1.r9/sized/**/*.parquet")),
        ]

    def test_skips_what_is_already_sized_and_sorts(self):
        self.assertEqual([m["shard_id"] for _, m in ca.plan(self.ms, set())], ["shard-a", "shard-b"])

    def test_only_filters(self):
        self.assertEqual([m["shard_id"] for _, m in ca.plan(self.ms, {"shard-b"})], ["shard-b"])
        self.assertEqual(ca.plan(self.ms, {"shard-c"}), [])  # sized: nothing to do even if asked

    def test_list_manifests_reads_every_prefix_but_not_underscore_ones(self):
        m = manifest("shard-a", f"s3://{B}/39-apps/a.attempt-1/k54/**/*.parquet")
        r = FakeRunner([
            (f"s3 ls s3://{B}/39-apps/_manifests/", "2026-11-01 19:17:27  530 shard-a.json\n"),
            (f"s3 cp s3://{B}/39-apps/_manifests/shard-a.json -", json.dumps(m)),
            (f"s3 ls s3://{B}/", f"                           PRE 39-apps/\n                           PRE _export-drill/\n2026-05-04 22:10:31    5 test-write.txt\n"),
        ])
        got = ca.list_manifests(r, B)
        self.assertEqual([x["shard_id"] for _, x in got], ["shard-a"])
        self.assertFalse(r.saw("_export-drill/_manifests"))


class UploadTests(unittest.TestCase):
    res = {"new_path": f"s3://{B}/40-apps/s.attempt-1.r9/sized/**/*.parquet", "files": 2, "bytes": 300}

    def test_passes_when_s3_holds_exactly_what_was_converted(self):
        ls = ("2026-11-02 03:00:00  100 40-apps/s.attempt-1.r9/sized/a/data_0.parquet\n"
              "2026-11-02 03:00:00  200 40-apps/s.attempt-1.r9/sized/b/data_0.parquet\n")
        r = FakeRunner([("s3 ls", ls)])
        ca.upload(r, B, self.res, "/tmp/out")
        self.assertTrue(r.saw(f"s3 sync /tmp/out/40-apps/s.attempt-1.r9/ s3://{B}/40-apps/s.attempt-1.r9/"))

    def test_fails_when_a_file_is_missing(self):
        r = FakeRunner([("s3 ls", "2026-11-02 03:00:00  100 40-apps/x/data_0.parquet\n")])
        with self.assertRaises(RuntimeError):
            ca.upload(r, B, self.res, "/tmp/out")

    def test_fails_when_the_bytes_differ(self):
        ls = ("2026-11-02 03:00:00  100 a.parquet\n2026-11-02 03:00:00  199 b.parquet\n")
        with self.assertRaises(RuntimeError):
            ca.upload(FakeRunner([("s3 ls", ls)]), B, self.res, "/tmp/out")


class SwapTests(unittest.TestCase):
    def runner(self, invocation):
        return FakeRunner([
            ("describe-instances", "i-1 i-2\n"),
            ("send-command", "cmd-1\n"),
            ("get-command-invocation", invocation),
        ])

    def test_tells_every_coordinator_and_succeeds(self):
        ok = json.dumps(["Success", "shard x swapped to y\n", ""])
        r = self.runner(ok)
        ca.swap(r, "shard-1", "s3://b/old/**/*.parquet", "s3://b/new/sized/**/*.parquet", sleep=lambda s: None)
        send = [c for c in r.calls if "send-command" in c][0]
        self.assertIn("i-1 i-2", send)
        self.assertIn("swap-archive --yes --shard shard-1", send)
        # the globs are quoted for the remote shell
        self.assertIn("'s3://b/old/**/*.parquet'", send)
        self.assertEqual(sum("get-command-invocation" in c for c in r.calls), 2)

    def test_a_coordinator_that_finds_it_done_is_fine(self):
        done = json.dumps(["Success", "already at the new path; nothing to swap\n", ""])
        ca.swap(self.runner(done), "s", "o", "n", sleep=lambda s: None)

    def test_a_refusal_is_an_error(self):
        refused = json.dumps(["Failed", "", "shard s is at x: refusing to swap"])
        with self.assertRaises(RuntimeError) as cm:
            ca.swap(self.runner(refused), "s", "o", "n", sleep=lambda s: None)
        self.assertIn("refusing to swap", str(cm.exception))

    def test_success_without_the_expected_output_is_an_error(self):
        weird = json.dumps(["Success", "usage: weft-admin ...", ""])
        with self.assertRaises(RuntimeError):
            ca.swap(self.runner(weird), "s", "o", "n", sleep=lambda s: None)

    def test_waits_while_pending(self):
        answers = iter([json.dumps(["InProgress", "", ""]), json.dumps(["Success", "swapped to n", ""]),
                        json.dumps(["Success", "swapped to n", ""])])
        r = FakeRunner([("describe-instances", "i-1 i-2"), ("send-command", "c"),
                        ("get-command-invocation", lambda line: next(answers))])
        ca.swap(r, "s", "o", "n", sleep=lambda s: None)

    def test_no_coordinators_is_an_error(self):
        with self.assertRaises(RuntimeError):
            ca.swap(FakeRunner([("describe-instances", "\n")]), "s", "o", "n", sleep=lambda s: None)


class MainTests(unittest.TestCase):
    def setUp(self):
        self.dir = tempfile.mkdtemp()
        self.m = manifest("shard-1", f"s3://{B}/40-apps/s.attempt-1/m1440/**/*.parquet")
        self.rules = [
            (f"s3 ls s3://{B}/40-apps/_manifests/", "2026-11-01 19:17:27  530 shard-1.json\n"),
            (f"s3 cp s3://{B}/40-apps/_manifests/shard-1.json -", json.dumps(self.m)),
            (f"s3 ls s3://{B}/", "                           PRE 40-apps/\n"),
        ]

    def test_the_default_stage_only_plans(self):
        r = FakeRunner(self.rules)
        self.assertEqual(ca.main(["--workdir", self.dir], r), 0)
        self.assertFalse(r.saw("sync"))
        self.assertFalse(r.saw("weft-admin"))
        self.assertFalse(os.path.exists(os.path.join(self.dir, "state.json")))

    def test_nothing_to_do_when_everything_is_sized(self):
        self.m["parquet_path"] = f"s3://{B}/40-apps/s.attempt-1.r9/sized/**/*.parquet"
        r = FakeRunner(self.rules[:1] + [(self.rules[1][0], json.dumps(self.m))] + self.rules[2:])
        self.assertEqual(ca.main(["--workdir", self.dir, "--stage", "convert"], r), 0)
        self.assertFalse(r.saw("sync"))

    def test_a_shard_already_swapped_is_not_touched_again(self):
        with open(os.path.join(self.dir, "state.json"), "w") as f:
            json.dump({"shard-1": {"converted": True, "uploaded": True, "swapped": True,
                                   "result": {"new_path": f"s3://{B}/x.r1/sized/**/*.parquet"}}}, f)
        r = FakeRunner(self.rules)
        self.assertEqual(ca.main(["--workdir", self.dir, "--stage", "apply", "--weft-admin", "/bin/true"], r), 0)
        self.assertFalse(r.saw("sync"))
        self.assertFalse(r.saw("send-command"))

    def test_a_failure_is_reported_and_the_run_exits_nonzero(self):
        r = FakeRunner(self.rules + [("convert-archive", RuntimeError("the download is not the archive the manifest describes"))])
        code = ca.main(["--workdir", self.dir, "--stage", "convert", "--weft-admin", "/bin/weft-admin"], r)
        self.assertEqual(code, 1)
        self.assertFalse(r.saw("send-command"))


if __name__ == "__main__":
    unittest.main()
