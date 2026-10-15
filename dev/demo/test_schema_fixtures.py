"""Tests for the schema-wizard fixtures. Plain unittest."""
import re
import sys
import unittest
from pathlib import Path

sys.path.insert(0, str(Path(__file__).parent))
import schema_fixtures as fx  # noqa: E402

VALUE_RE = {
    "fx-tagged": re.compile(r"trace_id=(\S+)"),
    "fx-bracketed": re.compile(r"\[(req-[0-9a-f]{8})\]"),
    "fx-jsonl": re.compile(r'"trace": \{"id": "([0-9a-f]{32})"'),
    "fx-celery": re.compile(r"send_email\[([0-9a-f-]{36})\]"),
}


class ScenarioTest(unittest.TestCase):
    def test_startup_fills_sample_and_has_no_trace(self):
        for name, fn in fx.SCENARIOS.items():
            startup, _ = fn(name, 10)
            self.assertEqual(len(startup), fx.SAMPLE_TARGET, name)
            for line in startup:
                self.assertIsNone(VALUE_RE[name].search(line), f"{name}: trace in startup: {line}")

    def test_traffic_carries_the_value_on_a_minority_to_most_lines(self):
        for name, fn in fx.SCENARIOS.items():
            _, traffic = fn(name, 200)
            hits = sum(1 for line in traffic if VALUE_RE[name].search(line))
            self.assertGreater(hits, 20, name)
            if name in ("fx-tagged", "fx-jsonl"):
                self.assertLess(hits, 190, f"{name} must also have lines without the value")

    def test_deterministic(self):
        for name, fn in fx.SCENARIOS.items():
            self.assertEqual(fn(name, 30), fn(name, 30), name)

    def test_heroku_frames_are_octet_counted(self):
        body = fx.heroku_body("svc", ["hello", "wörld"])
        rest = body
        n = 0
        while rest:
            length, _, rest = rest.partition(b" ")
            frame, rest = rest[: int(length)], rest[int(length):]
            self.assertIn(b" svc web.1 - - ", frame)
            n += 1
        self.assertEqual(n, 2)


if __name__ == "__main__":
    unittest.main()
