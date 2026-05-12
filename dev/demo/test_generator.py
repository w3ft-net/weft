"""Tests for the rate-shape generator.

Plain unittest so it runs anywhere — `python3 -m unittest discover dev/demo`.
"""
import math
import sys
import unittest
from pathlib import Path

sys.path.insert(0, str(Path(__file__).parent))
from generator import RateShape, parse_shape, line_for, parse_error_ratio, ShapeError  # noqa: E402


class ParseShapeTest(unittest.TestCase):
    def test_const(self):
        s = parse_shape("const:8")
        self.assertEqual(s.kind, "const")
        self.assertEqual(s.params, {"rate": 8.0})

    def test_sine(self):
        s = parse_shape("sine:20,40,600")
        self.assertEqual(s.kind, "sine")
        self.assertEqual(s.params, {"base": 20.0, "amp": 40.0, "period": 600.0})

    def test_sawtooth(self):
        s = parse_shape("sawtooth:2,0.05,12")
        self.assertEqual(s.kind, "sawtooth")
        self.assertEqual(s.params, {"base": 2.0, "slope": 0.05, "reset": 12.0})

    def test_monotonic_with_reset(self):
        s = parse_shape("monotonic:5,0.05,40")
        self.assertEqual(s.kind, "monotonic")
        self.assertEqual(s.params, {"base": 5.0, "slope": 0.05, "reset": 40.0})

    def test_monotonic_without_reset(self):
        s = parse_shape("monotonic:1,0.02")
        self.assertEqual(s.kind, "monotonic")
        self.assertEqual(s.params, {"base": 1.0, "slope": 0.02, "reset": None})

    def test_unknown_kind(self):
        with self.assertRaisesRegex(ShapeError, "unknown shape"):
            parse_shape("bogus:1,2,3")

    def test_wrong_arity_sine(self):
        with self.assertRaises(ShapeError):
            parse_shape("sine:20,40")


class RateAtTest(unittest.TestCase):
    def test_const_flat(self):
        s = parse_shape("const:7")
        self.assertEqual(s.rate_at(0), 7)
        self.assertEqual(s.rate_at(100), 7)
        self.assertEqual(s.rate_at(1e9), 7)

    def test_sine_oscillates(self):
        s = parse_shape("sine:20,40,600")
        # At t=0 the (1+sin(0))/2 = 0.5 → midpoint
        self.assertAlmostEqual(s.rate_at(0), 40.0, places=6)
        # Quarter period → sin(π/2)=1 → max
        self.assertAlmostEqual(s.rate_at(150), 60.0, places=6)
        # Half → sin(π)=0 → midpoint
        self.assertAlmostEqual(s.rate_at(300), 40.0, places=6)
        # 3/4 → sin(3π/2)=-1 → min
        self.assertAlmostEqual(s.rate_at(450), 20.0, places=6)

    def test_sawtooth_climbs_then_resets(self):
        # base=2, slope=0.5, reset=10 — period = (10-2)/0.5 = 16s
        s = parse_shape("sawtooth:2,0.5,10")
        self.assertAlmostEqual(s.rate_at(0), 2.0, places=6)
        self.assertAlmostEqual(s.rate_at(8), 6.0, places=6)
        # Just below reset
        self.assertLess(s.rate_at(15.999), 10.0)
        # At reset boundary, wrap back to base
        self.assertAlmostEqual(s.rate_at(16), 2.0, places=6)
        # Second period
        self.assertAlmostEqual(s.rate_at(24), 6.0, places=6)

    def test_monotonic_with_reset_matches_sawtooth(self):
        a = parse_shape("monotonic:0,0.1,30")
        b = parse_shape("sawtooth:0,0.1,30")
        for t in (0, 50, 150, 299, 300, 301, 1000):
            self.assertAlmostEqual(a.rate_at(t), b.rate_at(t), places=9,
                                   msg=f"divergence at t={t}")

    def test_monotonic_without_reset_grows_unbounded(self):
        s = parse_shape("monotonic:1,0.02")
        self.assertAlmostEqual(s.rate_at(0), 1.0, places=6)
        self.assertAlmostEqual(s.rate_at(60), 2.2, places=6)
        self.assertAlmostEqual(s.rate_at(3600), 73.0, places=6)
        self.assertGreater(s.rate_at(10**6), 100.0)


class LineForTest(unittest.TestCase):
    def test_api_has_request_field(self):
        line = line_for("api", t=12.5, level="INFO", rng_seed=1)
        self.assertIn(" INFO ", line)
        self.assertIn("request ", line)
        self.assertIn("method=", line)
        self.assertIn("path=", line)
        self.assertIn("status=", line)

    def test_worker_emits_job_completed(self):
        line = line_for("worker", t=0, level="INFO", rng_seed=2)
        self.assertIn("job_", line)

    def test_billing_emits_invoice(self):
        line = line_for("billing", t=0, level="INFO", rng_seed=3)
        self.assertIn("invoice_", line)

    def test_unknown_service_falls_back_to_generic(self):
        line = line_for("mystery", t=0, level="INFO", rng_seed=4)
        self.assertIn(" INFO ", line)
        self.assertGreaterEqual(len(line.split()), 3)


class IntegratedEmitTest(unittest.TestCase):
    """Average-rate property tests — stochastic; tolerance wide enough to
    survive normal rng variance.

    Per-step token count is floor(rate*dt) + Bernoulli(frac), so over N
    steps with frac=0.5 the stddev is sqrt(N*0.25); we use ±4σ bounds
    so spurious flakes are vanishingly rare without seeding.
    """

    def test_const_rate_total_lines(self):
        # const:5 over 10s → expected 50, stddev=5; ±20 is comfortably wide.
        s = parse_shape("const:5")
        count = sum(s.tokens_emitted_for_step(rate=s.rate_at(t / 10), dt=0.1)
                    for t in range(100))
        self.assertGreaterEqual(count, 30)
        self.assertLessEqual(count, 70)

    def test_sine_average_total_lines(self):
        # sine:0,10,60 has time-average 5 → expected 300, stddev~12; ±40 wide.
        s = parse_shape("sine:0,10,60")
        count = sum(s.tokens_emitted_for_step(rate=s.rate_at(t / 10), dt=0.1)
                    for t in range(600))
        self.assertGreaterEqual(count, 260)
        self.assertLessEqual(count, 340)


class ParseErrorRatioTest(unittest.TestCase):
    def test_bare_float_is_constant(self):
        s = parse_error_ratio("0.08")
        self.assertEqual(s.kind, "const")
        self.assertAlmostEqual(s.rate_at(0), 0.08)
        self.assertAlmostEqual(s.rate_at(9999), 0.08)

    def test_shape_spec_oscillates(self):
        # base=0.04 amp=0.10 period=600 → mid=0.09 at t=0, max=0.14 at quarter.
        s = parse_error_ratio("sine:0.04,0.10,600")
        self.assertAlmostEqual(s.rate_at(0), 0.09, places=6)
        self.assertAlmostEqual(s.rate_at(150), 0.14, places=6)
        self.assertAlmostEqual(s.rate_at(450), 0.04, places=6)


if __name__ == "__main__":
    unittest.main()
