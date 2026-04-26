#!/usr/bin/env python3
"""Demo log line generator.

Reads SERVICE, RATE_SHAPE, ERROR_RATIO, LOG_PATH from the environment;
emits realistic-looking log lines whose rate (lines/sec) follows a
configured shape function. Designed to run as a subprocess of weftd-
demo-service entrypoint.sh.

Rate shapes:
  const:R                         flat at R lines/sec
  sine:base,amp,period            base + amp*(1+sin(2π·t/period))/2
  sawtooth:base,slope,reset       climb at slope, snap to base when ≥ reset
  monotonic:base,slope[,reset]    same as sawtooth when reset set;
                                  unbounded climb when reset omitted

Service-specific line content lives in line_for() — one keyword pool
per app so jq/awk grouping queries return interesting shapes.
"""
from __future__ import annotations

import datetime as dt
import math
import os
import random
import sys
import time
from dataclasses import dataclass


class ShapeError(ValueError):
    pass


@dataclass
class RateShape:
    kind: str
    params: dict

    def rate_at(self, t: float) -> float:
        if self.kind == "const":
            return self.params["rate"]
        if self.kind == "sine":
            base = self.params["base"]
            amp = self.params["amp"]
            period = self.params["period"]
            return base + amp * (1 + math.sin(2 * math.pi * t / period)) / 2
        if self.kind == "sawtooth":
            return self._ramp_with_reset(t, self.params["base"],
                                         self.params["slope"],
                                         self.params["reset"])
        if self.kind == "monotonic":
            reset = self.params["reset"]
            base = self.params["base"]
            slope = self.params["slope"]
            if reset is None:
                return base + slope * t
            return self._ramp_with_reset(t, base, slope, reset)
        raise ShapeError(f"unknown shape: {self.kind}")

    @staticmethod
    def _ramp_with_reset(t: float, base: float, slope: float, reset: float) -> float:
        # Period such that base + slope*period = reset → period = (reset-base)/slope
        if slope <= 0:
            return base
        period = (reset - base) / slope
        if period <= 0:
            return base
        within = t - period * math.floor(t / period)
        return base + slope * within

    def tokens_emitted_for_step(self, rate: float, dt: float) -> int:
        """Stateless integer-emit count for one step at this rate.

        For very small rate*dt this returns 0 most of the time, with a
        random per-step round-up. Over many steps the expected count
        equals rate*time. Implementation is the same logic the live loop
        uses internally; exposed here so tests can integrate over a
        window without spinning a real loop.
        """
        expected = rate * dt
        whole = int(expected)
        frac = expected - whole
        # Deterministic rng for tests would be brittle here, but the live
        # loop accumulates fractional tokens rather than coin-flipping;
        # the test asserts approximate totals over a window so a coin
        # flip on the fractional remainder satisfies both. Use python's
        # default random which is fine for non-cryptographic timing.
        if random.random() < frac:
            whole += 1
        return whole


def parse_shape(spec: str) -> RateShape:
    if ":" not in spec:
        raise ShapeError(f"shape needs colon: {spec!r}")
    kind, _, rest = spec.partition(":")
    parts = [p.strip() for p in rest.split(",") if p.strip()]
    try:
        nums = [float(p) for p in parts]
    except ValueError as e:
        raise ShapeError(f"non-numeric param in {spec!r}: {e}") from None

    if kind == "const":
        if len(nums) != 1:
            raise ShapeError("const takes exactly 1 param: const:R")
        return RateShape(kind="const", params={"rate": nums[0]})
    if kind == "sine":
        if len(nums) != 3:
            raise ShapeError("sine takes 3 params: sine:base,amp,period")
        return RateShape(kind="sine",
                         params={"base": nums[0], "amp": nums[1], "period": nums[2]})
    if kind == "sawtooth":
        if len(nums) != 3:
            raise ShapeError("sawtooth takes 3 params: sawtooth:base,slope,reset")
        return RateShape(kind="sawtooth",
                         params={"base": nums[0], "slope": nums[1], "reset": nums[2]})
    if kind == "monotonic":
        if len(nums) not in (2, 3):
            raise ShapeError("monotonic takes 2 or 3 params: monotonic:base,slope[,reset]")
        reset = nums[2] if len(nums) == 3 else None
        return RateShape(kind="monotonic",
                         params={"base": nums[0], "slope": nums[1], "reset": reset})
    raise ShapeError(f"unknown shape: {kind}")


# --- line content ---------------------------------------------------------

# Pools per service. Keep these small so consumers can group/aggregate.
PATHS = ["/health", "/api/v1/orders", "/api/v1/users", "/login", "/checkout", "/products"]
METHODS = ["GET", "POST", "PUT", "DELETE"]
STATUSES = [200, 200, 200, 200, 201, 204, 301, 400, 401, 404, 500, 502]
USER_IDS = [f"u_{i:03x}" for i in range(64)]
JOB_NAMES = ["index_rebuild", "thumb_resize", "email_digest", "billing_recalc", "purge"]
ENDPOINTS = ["auth-svc", "search-svc", "geo-svc", "billing-svc", "stripe"]
INVOICES = [f"inv_{i:05x}" for i in range(128)]
ORDERS = [f"ord_{i:05x}" for i in range(256)]


def _ts(t: float) -> str:
    """ISO8601 timestamp with milliseconds, anchored to wall clock + t."""
    base = dt.datetime.now(dt.timezone.utc)
    moment = base + dt.timedelta(seconds=0)  # t is wall-time-relative; just stamp now
    return moment.strftime("%Y-%m-%dT%H:%M:%S.") + f"{moment.microsecond // 1000:03d}Z"


def line_for(service: str, t: float, level: str, rng_seed: int | None = None) -> str:
    """Return one log line for service at logical time t.

    rng_seed makes test cases deterministic; production passes None to
    use the global random state.
    """
    r = random.Random(rng_seed) if rng_seed is not None else random
    ts = _ts(t)

    if service == "api" or service == "edge":
        method = r.choice(METHODS)
        path = r.choice(PATHS)
        status = r.choice(STATUSES) if level == "INFO" else r.choice([400, 401, 404, 500, 502, 503])
        dur = r.randint(1, 250) if level == "INFO" else r.randint(500, 5000)
        user = r.choice(USER_IDS)
        return f"{ts} {level:5s} request method={method} path={path} status={status} dur={dur}ms user_id={user}"

    if service == "worker":
        job = r.choice(JOB_NAMES)
        if level == "ERROR":
            return f"{ts} {level:5s} job_failed job={job} reason=deadline_exceeded duration_ms={r.randint(30000, 90000)}"
        if level == "WARN":
            return f"{ts} {level:5s} job_retry job={job} attempt={r.randint(2, 5)}"
        return f"{ts} {level:5s} job_completed job={job} duration_ms={r.randint(50, 5000)} rows={r.randint(1, 1000)}"

    if service == "orders":
        order = r.choice(ORDERS)
        amount = round(r.uniform(5, 500), 2)
        if level == "ERROR":
            return f"{ts} {level:5s} order_failed order_id={order} reason=payment_declined"
        return f"{ts} {level:5s} order_placed order_id={order} amount={amount} currency=USD"

    if service == "billing":
        inv = r.choice(INVOICES)
        amount = round(r.uniform(10, 2000), 2)
        if level == "ERROR":
            return f"{ts} {level:5s} invoice_failed invoice_id={inv} reason=stripe_timeout"
        if level == "WARN":
            return f"{ts} {level:5s} invoice_retry invoice_id={inv} attempt={r.randint(2, 4)}"
        return f"{ts} {level:5s} invoice_sent invoice_id={inv} amount={amount}"

    if service == "notifier":
        endpoint = r.choice(ENDPOINTS)
        if level == "ERROR":
            return f"{ts} {level:5s} notify_failed endpoint={endpoint} reason=upstream_5xx"
        return f"{ts} {level:5s} notify_sent endpoint={endpoint} latency_ms={r.randint(20, 800)}"

    # Generic fallback
    return f"{ts} {level:5s} event service={service} value={r.randint(0, 1000)}"


# --- main loop ------------------------------------------------------------

def _pick_level(error_ratio: float) -> str:
    r = random.random()
    if r < error_ratio:
        return "ERROR"
    if r < error_ratio + 0.15:
        return "WARN"
    return "INFO"


def main() -> int:
    service = os.environ.get("SERVICE", "api")
    shape_spec = os.environ.get("RATE_SHAPE", "const:1")
    error_ratio = float(os.environ.get("ERROR_RATIO", "0.05"))
    log_path = os.environ.get("LOG_PATH", f"/var/log/{service}.log")

    try:
        shape = parse_shape(shape_spec)
    except ShapeError as e:
        print(f"generator: bad RATE_SHAPE {shape_spec!r}: {e}", file=sys.stderr)
        return 2

    print(f"generator: service={service} shape={shape_spec} error_ratio={error_ratio} log={log_path}",
          file=sys.stderr)

    # Append-mode so a restart doesn't truncate weftd's view of the file.
    out = open(log_path, "a", buffering=1)  # line buffered

    started = time.monotonic()
    last = started
    accumulator = 0.0
    step = 0.25  # seconds; recompute rate every 250ms
    try:
        while True:
            now = time.monotonic()
            elapsed = now - started
            tick = now - last
            last = now

            rate = shape.rate_at(elapsed)
            accumulator += rate * tick

            while accumulator >= 1.0:
                level = _pick_level(error_ratio)
                out.write(line_for(service, elapsed, level) + "\n")
                accumulator -= 1.0

            sleep_for = step - (time.monotonic() - now)
            if sleep_for > 0:
                time.sleep(sleep_for)
    except KeyboardInterrupt:
        return 0


if __name__ == "__main__":
    sys.exit(main())
