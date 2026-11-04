"""Gross-margin model for weft's tiers, at 100% use of every limit.

    pipenv run python scripts/costmodel/model.py

COGS per customer-month = hot storage + cold storage + cold-query Lambda for
the full scan budget + the cron load's share of a Postgres cell. The Stripe
fee comes off revenue. The platform floor (load balancers, IPv4s, the fixed
fleet) is paid regardless of customers and is not part of a tier's cost.
"""
import math
import pathlib

import yaml

HERE = pathlib.Path(__file__).parent
DAYS_PER_MONTH = 30


def load():
    prices = yaml.safe_load((HERE / "prices.yaml").read_text())
    tiers = yaml.safe_load((HERE / "tiers.yaml").read_text())
    return prices, tiers


def cold_gb_month_usd(prices):
    """Cost of holding one raw GB as Parquet for a month."""
    return prices["measured"]["parquet_ratio"] * prices["aws"]["s3_standard_gb_month"]


def hot_shard_usd(prices):
    """One live shard a month: its data volume, its instance, and its share of the spares."""
    h = prices["hot_shard"]
    carried = 1 + h["spares_per_live_shard"]
    return h["data_volume_usd"] + carried * (h["instance_usd"] + h["root_volume_usd"])


def hot_gb_month_usd(prices):
    """Cost of holding one raw GB hot for a month: a shard's cost over the raw GB it holds."""
    h = prices["hot_shard"]
    return hot_shard_usd(prices) / (h["data_volume_gb"] / prices["measured"]["hot_ratio"] * h["fill"])


def cold_days(prices, tier):
    """Days a GB sits in Parquet: retention past the hot window, plus the janitor's lag."""
    days = tier["retention_days"] - min(tier["hot_days"], tier["retention_days"])
    return days + prices["measured"]["janitor_lag_days"] if days > 0 else 0


def dedicated_shards(prices, tier):
    """Whole shards a dedicated customer's hot data needs at the dedicated fill."""
    h = prices["hot_shard"]
    hot_gb = tier["ingest_gb"] * min(tier["hot_days"], tier["retention_days"]) / DAYS_PER_MONTH
    return max(math.ceil(hot_gb * prices["measured"]["hot_ratio"] / (h["data_volume_gb"] * prices["dedicated"]["fill"])), 1)


def dedicated_hot_usd(prices, tier):
    """Shards (volume + instance + root) plus the customer's own warm spares."""
    h, d = prices["hot_shard"], prices["dedicated"]
    instance = h["instance_usd"] * tier["instance_mult"] + h["root_volume_usd"]
    return dedicated_shards(prices, tier) * (h["data_volume_usd"] + instance) + d["spares_per_customer"] * instance


def hot_storage_usd(prices, tier):
    if tier.get("dedicated"):
        return dedicated_hot_usd(prices, tier)
    hot_days = min(tier["hot_days"], tier["retention_days"])
    return tier["ingest_gb"] * hot_days / DAYS_PER_MONTH * hot_gb_month_usd(prices)


def cold_storage_usd(prices, tier):
    return tier["ingest_gb"] * cold_days(prices, tier) / DAYS_PER_MONTH * cold_gb_month_usd(prices)


def marginal_gb_usd(prices, tier):
    """What one more ingested GB costs, at the tier's retention."""
    one = dict(tier, ingest_gb=1)
    return hot_storage_usd(prices, one) + cold_storage_usd(prices, one) + prices["platform"]["ingest_usd_per_gb"]


def cold_lines_stored_billions(prices, tier):
    """Lines sitting in cold storage at steady state: the archive a scan pass reads."""
    cold = tier["retention_days"] - min(tier["hot_days"], tier["retention_days"])
    return tier["ingest_gb"] * 1e9 / prices["measured"]["bytes_per_row"] * cold / DAYS_PER_MONTH / 1e9


def cold_lines_billions(prices, tier):
    """The line scan budget: lines a month the tier may read from cold storage."""
    return tier["scan_passes"] * cold_lines_stored_billions(prices, tier)


def cold_query_usd(prices, tier, which):
    return cold_lines_billions(prices, tier) * prices["measured"][f"cold_usd_per_billion_lines_{which}"]


def cron_rows_per_second(prices, tier):
    ticks_per_day = tier["crons"] * 1440 / tier["cron_interval_min"]
    return ticks_per_day * prices["measured"]["cron"]["rows_per_tick"] / 86400


def cell_usd(prices):
    p = prices["platform"]
    return p["cell_usd_multi_az"] if p["cell_multi_az"] else p["cell_usd_single_az"]


def cron_cell_usd(prices, tier):
    """The tier's cron write load as a share of a cell, plus its cron_results storage."""
    p, c = prices["platform"], prices["measured"]["cron"]
    share = cron_rows_per_second(prices, tier) / p["cell_rows_per_second"] * cell_usd(prices)
    raw_gb = (tier["crons"] * 1440 / tier["cron_interval_min"] * c["rows_per_tick"]
              * c["raw_retention_days"] * c["raw_row_bytes"] / 1e9)
    scale = tier["crons"] * 1440 / tier["cron_interval_min"] / (500 * 1440)  # rollups scale with load
    return share + (raw_gb + c["rollup_gb_at_bolt_limit"] * scale) * prices["aws"]["gp3_gb_month"]


def cogs_usd(prices, tier, which):
    return (hot_storage_usd(prices, tier) + cold_storage_usd(prices, tier)
            + tier["ingest_gb"] * prices["platform"]["ingest_usd_per_gb"]
            + cold_query_usd(prices, tier, which)
            + cron_cell_usd(prices, tier))


def stripe_fee_usd(prices, price):
    return 0.0 if price == 0 else price * prices["stripe"]["percent"] + prices["stripe"]["fixed_usd"]


def margin(prices, price, cogs):
    if price == 0:
        return float("-inf")
    return (price - stripe_fee_usd(prices, price) - cogs) / price


def price_for_margin(prices, cogs, target):
    s = prices["stripe"]
    return (cogs + s["fixed_usd"]) / (1 - target - s["percent"])


def overage_price_for_margin(prices, tier, target):
    """Overage is billed per GB, so only the percentage fee applies."""
    return marginal_gb_usd(prices, tier) / (1 - target - prices["stripe"]["percent"])


def passes_headroom(prices, tier, which):
    """Cold scan passes a month the advertised price can include and still hit the tier's margin."""
    stored = cold_lines_stored_billions(prices, tier)
    if stored == 0:
        return 0.0
    s = prices["stripe"]
    spend = (tier["price_usd"] * (1 - tier["margin_target"] - s["percent"]) - s["fixed_usd"]
             - cogs_usd(prices, dict(tier, scan_passes=0), which))
    return max(spend, 0) / (stored * prices["measured"][f"cold_usd_per_billion_lines_{which}"])


def price_for_passes(prices, tier, passes, which):
    """Cheapest price that includes `passes` scan passes at the tier's margin."""
    return price_for_margin(prices, cogs_usd(prices, dict(tier, scan_passes=passes), which), tier["margin_target"])


def variant(prices, **changes):
    """A copy of prices with platform/hot_shard overrides: variant(p, hot_shard={"fill": .5})."""
    out = {k: dict(v) if isinstance(v, dict) else v for k, v in prices.items()}
    for section, values in changes.items():
        out[section].update(values)
    return out


def _table(title, header, rows):
    print(f"\n{title}")
    widths = [max(len(str(r[i])) for r in [header] + rows) for i in range(len(header))]
    line = lambda r: "  ".join(str(c).rjust(w) if i else str(c).ljust(w) for i, (c, w) in enumerate(zip(r, widths)))
    print(line(header))
    print("  ".join("-" * w for w in widths))
    for r in rows:
        print(line(r))


def main():
    prices, cfg = load()
    tiers = cfg["tiers"]
    paid = {n: t for n, t in tiers.items() if t["price_usd"] > 0}
    s, mm = prices["stripe"], prices["measured"]
    d = lambda x: f"${x:,.2f}"
    pc = lambda x: f"{x:.1%}".replace(".0%", "%")
    lo, hi = "low", "high"

    print("FLAG: ingest compute $/GB is unmeasured (0); hot-path query compute and S3 requests are not modeled")
    print(f"FLAG: cell capacity ({prices['platform']['cell_rows_per_second']} rows/s) and shard fill ({prices['hot_shard']['fill']:.0%}) are assumptions; Yard's margin is a midpoint")
    print(f"Every limit used at 100%; the platform floor is not allocated; Lambda $/billion lines: "
          f"{mm['cold_usd_per_billion_lines_low']} (low) / {mm['cold_usd_per_billion_lines_high']} (high), n=2 samples each")

    _table("1. Entitlements", ["tier", "price", "margin target", "incl GB", "$/GB incl", "retention d", "hot d", "cold d", "overage/GB", "crons", "interval m"],
           [[n, d(t["price_usd"]), pc(t["margin_target"]) if "margin_target" in t else "-", t["ingest_gb"], d(t["price_usd"] / t["ingest_gb"]),
             t["retention_days"], t["hot_days"], t["retention_days"] - min(t["hot_days"], t["retention_days"]),
             d(t["overage_usd_per_gb"]), t["crons"], t["cron_interval_min"]] for n, t in tiers.items()])

    _table("2. Cold line scan budget (passes over the cold archive a month; every pass is a read of every cold line)",
           ["tier", "lines/mo ingested", "cold lines stored", "passes", "budget lines", "budget cost lo", "budget cost hi"],
           [[n, f"{t['ingest_gb'] * 1e9 / mm['bytes_per_row'] / 1e6:,.0f}M", f"{cold_lines_stored_billions(prices, t):.2f}B",
             t["scan_passes"], f"{cold_lines_billions(prices, t):.2f}B", d(cold_query_usd(prices, t, lo)), d(cold_query_usd(prices, t, hi))]
            for n, t in tiers.items()])

    rows = []
    for n, t in tiers.items():
        parts = [hot_storage_usd(prices, t), cold_storage_usd(prices, t), cron_cell_usd(prices, t),
                 t["ingest_gb"] * prices["platform"]["ingest_usd_per_gb"]]
        clo, chi = cogs_usd(prices, t, lo), cogs_usd(prices, t, hi)
        rows.append([n, d(parts[0]), d(parts[1]), d(cold_query_usd(prices, t, lo)), d(cold_query_usd(prices, t, hi)),
                     d(parts[2]), d(parts[3]), d(clo), d(chi)])
    _table("3. Cost per tier at 100% use, scan budget fully used ($/mo)",
           ["tier", "hot store", "cold store", "scan cost lo", "scan cost hi", "cron cell", "ingest", "COGS lo", "COGS hi"], rows)

    rows = []
    for n, t in paid.items():
        clo, chi = cogs_usd(prices, t, lo), cogs_usd(prices, t, hi)
        fee = stripe_fee_usd(prices, t["price_usd"])
        tgt = t["margin_target"]
        rows.append([n, d(t["price_usd"]), d(fee), pc(margin(prices, t["price_usd"], clo)), pc(margin(prices, t["price_usd"], chi)),
                     pc(tgt), d(price_for_margin(prices, clo, tgt)), d(price_for_margin(prices, chi, tgt))])
    _table("4. Margin at the advertised price, and the price each tier's target needs (with the budget above)",
           ["tier", "price", "Stripe fee", "margin lo", "margin hi", "target", "min price lo", "min price hi"], rows)

    rows = []
    for n, t in paid.items():
        stored = cold_lines_stored_billions(prices, t)
        row = [n, d(price_for_passes(prices, t, 0, lo)), d(price_for_passes(prices, t, 0, hi))]
        for k in (1, 2, 4):
            row += [d(price_for_passes(prices, t, k, lo)), d(price_for_passes(prices, t, k, hi))]
        row += [f"{passes_headroom(prices, t, lo):.1f}", f"{passes_headroom(prices, t, hi):.1f}",
                f"{passes_headroom(prices, t, lo) * stored:.2f}B", f"{passes_headroom(prices, t, hi) * stored:.2f}B"]
        rows.append(row)
    _table("5. What a scan budget costs in price at each tier's margin, and what the advertised price already affords",
           ["tier", "0 passes lo", "0 passes hi", "1 pass lo", "1 pass hi", "2 passes lo", "2 passes hi", "4 passes lo", "4 passes hi",
            "affords lo", "affords hi", "= lines lo", "= lines hi"], rows)

    rows = []
    for n, t in paid.items():
        one = dict(t, ingest_gb=1)
        tgt = t["margin_target"]
        rows.append([n, d(hot_storage_usd(prices, one)), d(cold_storage_usd(prices, one)), d(marginal_gb_usd(prices, t)),
                     d(overage_price_for_margin(prices, t, tgt)), d(t["overage_usd_per_gb"]),
                     pc((t["overage_usd_per_gb"] * (1 - s["percent"]) - marginal_gb_usd(prices, t)) / t["overage_usd_per_gb"])])
    _table("6. Overage, per extra GB ingested", ["tier", "hot", "cold", "marginal cost", "min at target", "advertised", "margin at advertised"], rows)


    # RDS cell: what the cron load costs per tier as cell capacity and AZ vary.
    rows = []
    for az in (False, True):
        for cap in (25, 50, 100, 200):
            q = variant(prices, platform={"cell_multi_az": az, "cell_rows_per_second": cap})
            rows.append([("Multi-AZ" if az else "single-AZ"), cap, d(cell_usd(q))]
                        + [d(cron_cell_usd(q, t)) for t in paid.values()]
                        + [f"{cron_rows_per_second(q, t) / cap:.0%}" for t in paid.values()])
    _table("7. RDS cell: cron write load and cron_results storage per tier ($/mo), by cell capacity (rows/s) and AZ",
           ["cell", "rows/s cap", "cell $/mo"] + [f"{n} $" for n in paid] + [f"{n} of cell" for n in paid], rows)

    # DuckDB shard: hot storage per tier as the instance class and fill vary.
    rows = []
    for name, mult in (("t4g.micro", 1), ("t4g.small", 2), ("t4g.medium", 4), ("t4g.large", 8)):
        for fill in (1.0, 0.6, 0.3):
            q = variant(prices, hot_shard={"instance_usd": prices["hot_shard"]["instance_usd"] * mult, "fill": fill})
            rows.append([name, f"{fill:.0%}", d(hot_shard_usd(q)), d(hot_gb_month_usd(q))]
                        + [d(hot_storage_usd(q, t)) for t in paid.values()]
                        + [pc(margin(q, t["price_usd"], cogs_usd(q, t, hi))) for t in paid.values()])
    _table("8. DuckDB shard: shard $/mo, hot $/raw GB-mo, hot storage per tier, and margin at the advertised price (high Lambda rate)",
           ["instance", "fill", "shard $/mo", "$/GB-mo"] + [f"{n} hot" for n in paid] + [f"{n} mgn" for n in paid], rows)


    ded = cfg["dedicated"]
    s_lo, s_hi = lo, hi
    rows = []
    for name, mult in (("t4g.micro", 1), ("t4g.small", 2), ("t4g.medium", 4), ("t4g.large", 8)):
        t = dict(ded, instance_mult=mult)
        clo, chi = cogs_usd(prices, t, s_lo), cogs_usd(prices, t, s_hi)
        tgt = t["margin_target"]
        rows.append([name, dedicated_shards(prices, t), d(dedicated_hot_usd(prices, t)), d(cold_storage_usd(prices, t)),
                     d(cold_query_usd(prices, t, lo)), d(cold_query_usd(prices, t, hi)), d(cron_cell_usd(prices, t)),
                     d(clo), d(chi), d(price_for_margin(prices, clo, tgt)), d(price_for_margin(prices, chi, tgt)),
                     d(price_for_margin(prices, chi, tgt) / t["ingest_gb"])])
    _table(f"9. Dedicated (Bolt) quote reference: {ded['ingest_gb']} GB/mo, {ded['retention_days']} d retention, {ded['scan_passes']} scan pass, "
           f"{ded['margin_target']:.0%} margin, dedicated fill {prices['dedicated']['fill']:.0%}",
           ["instance", "shards", "hot (shards+spare)", "cold store", "scan lo", "scan hi", "cron cell", "COGS lo", "COGS hi", "quote lo", "quote hi", "quote $/GB hi"], rows)

    rows = []
    for gb in (100, 250, 500, 1000, 2000, 5000):
        row = [gb]
        for mult in (1, 2, 4):
            t = dict(ded, ingest_gb=gb, instance_mult=mult, crons=ded["crons"], scan_passes=ded["scan_passes"])
            row += [dedicated_shards(prices, t), d(price_for_margin(prices, cogs_usd(prices, t, hi), t["margin_target"]))]
        rows.append(row)
    _table("10. Dedicated quote by ingest volume (high Lambda rate, same retention, scan passes, crons): shards and monthly quote",
           ["GB/mo", "micro shards", "micro quote", "small shards", "small quote", "medium shards", "medium quote"], rows)


if __name__ == "__main__":
    main()
