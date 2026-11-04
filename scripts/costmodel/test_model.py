import math

import model

P, CFG = model.load()
T = CFG["tiers"]
D = CFG["dedicated"]


def test_cold_storage_matches_the_measured_per_gb_month():
    assert math.isclose(model.cold_gb_month_usd(P), 0.0177, abs_tol=0.0001)


def test_bolt_all_cold_is_about_eleven_cents_per_ingested_gb():
    # 180 days plus the janitor's 7
    t = dict(D, hot_days=0, ingest_gb=1)
    assert math.isclose(model.cold_storage_usd(P, t), 187 / 30 * 0.0177, abs_tol=0.001)


def test_the_janitor_lag_adds_cold_days_only_where_there_is_cold_data():
    assert model.cold_days(P, D) == 180 - 14 + 7
    assert model.cold_days(P, T["trial"]) == 0


def test_hot_gb_cost_is_a_shards_cost_over_what_it_holds():
    shard = 8.00 + 1.5 * (6.13 + 0.64)
    assert math.isclose(model.hot_shard_usd(P), shard)
    assert math.isclose(model.hot_gb_month_usd(P), shard / (100 / 1.07))


def test_a_half_full_shard_doubles_the_hot_cost_per_gb():
    half = {**P, "hot_shard": {**P["hot_shard"], "fill": 0.5}}
    assert math.isclose(model.hot_gb_month_usd(half), 2 * model.hot_gb_month_usd(P))


def test_priced_margin_hits_the_target():
    for cogs in (3.0, 40.0, 250.0):
        price = model.price_for_margin(P, cogs, 0.30)
        assert math.isclose(model.margin(P, price, cogs), 0.30)


def test_overage_at_its_priced_margin_leaves_the_target():
    for t in (T["swatch"], T["yard"], D):
        o = model.overage_price_for_margin(P, t, 0.30)
        assert math.isclose((o * (1 - P["stripe"]["percent"]) - model.marginal_gb_usd(P, t)) / o, 0.30)


def test_a_tier_that_never_leaves_hot_has_no_cold_storage():
    assert model.cold_storage_usd(P, T["trial"]) == 0.0


def test_the_scan_headroom_is_exactly_what_keeps_the_margin():
    t = dict(T["swatch"], price_usd=100)
    for which in ("low", "high"):
        k = model.passes_headroom(P, t, which)
        assert k > 0
        full = dict(t, scan_passes=k)
        assert math.isclose(model.margin(P, t["price_usd"], model.cogs_usd(P, full, which)), t["margin_target"], abs_tol=1e-9)


def test_the_price_for_a_budget_leaves_exactly_the_margin():
    for t in (T["swatch"], T["yard"], D):
        for k in (0, 1, 4):
            for which in ("low", "high"):
                price = model.price_for_passes(P, t, k, which)
                cogs = model.cogs_usd(P, dict(t, scan_passes=k), which)
                assert math.isclose(model.margin(P, price, cogs), t["margin_target"], abs_tol=1e-9)


def test_the_scan_budget_grows_with_stored_volume():
    stored = [model.cold_lines_stored_billions(P, T[n]) for n in ("swatch", "yard")] + [model.cold_lines_stored_billions(P, D)]
    assert stored == sorted(stored) and stored[0] > 0
    # Bolt: 500 GB/mo at 155 B/row for 166 cold days
    assert math.isclose(stored[2], 500e9 / 155 * 166 / 30 / 1e9)


def test_margin_targets_step_down_with_scale():
    targets = [T[n]["margin_target"] for n in ("swatch", "yard")] + [D["margin_target"]]
    assert targets == sorted(targets, reverse=True) and targets[0] == 0.95 and targets[-1] == 0.80


def test_bolt_cron_load_is_the_plans_720k_ticks_a_day():
    assert math.isclose(model.cron_rows_per_second(P, D), 500 * 1440 * 1.2 / 86400)


def test_a_higher_margin_target_needs_a_higher_price():
    cogs = 40.0
    prices = [model.price_for_margin(P, cogs, m) for m in (0.3, 0.5, 0.7)]
    assert prices == sorted(prices) and len(set(prices)) == 3


def test_a_price_below_the_margin_line_has_no_scan_headroom():
    # Stripe's fixed fee plus COGS already exceed 5% of $19
    assert model.passes_headroom(P, T["swatch"], "low") == 0.0


def test_multi_az_prices_the_cron_load_at_the_dearer_cell():
    multi = model.variant(P, platform={"cell_multi_az": True})
    assert model.cell_usd(P) == 15 and model.cell_usd(multi) == 29
    assert model.cron_cell_usd(multi, D) > model.cron_cell_usd(P, D)


def test_a_smaller_cell_capacity_costs_the_cron_load_proportionally_more():
    small = model.variant(P, platform={"cell_rows_per_second": 50})
    before = model.cron_rows_per_second(P, D) / 100 * 15
    after = model.cron_rows_per_second(P, D) / 50 * 15
    assert math.isclose(after, 2 * before)
    assert model.cron_cell_usd(small, D) > model.cron_cell_usd(P, D)


def test_a_bigger_shard_instance_raises_the_hot_cost_per_gb():
    big = model.variant(P, hot_shard={"instance_usd": 6.13 * 8})
    assert model.hot_gb_month_usd(big) > model.hot_gb_month_usd(P)
    assert math.isclose(model.hot_shard_usd(big), 8.00 + 1.5 * (6.13 * 8 + 0.64))


def test_variant_leaves_the_original_untouched():
    model.variant(P, hot_shard={"fill": 0.1})
    assert P["hot_shard"]["fill"] == 1.0


def test_bolt_is_not_a_posted_tier():
    assert "bolt" not in T
    assert "price_usd" not in D and D["dedicated"] is True


def test_a_dedicated_customer_gets_whole_shards_for_their_hot_data():
    # 500 GB/mo x 14/30 = 233.3 raw GB hot; x1.07 over 100 GB x 80% fill
    assert model.dedicated_shards(P, D) == math.ceil(233.33 * 1.07 / 80)
    assert model.dedicated_shards(P, dict(D, ingest_gb=1)) == 1


def test_dedicated_hot_cost_is_shards_plus_one_spare():
    n = model.dedicated_shards(P, D)
    inst = 6.13 + 0.64
    assert math.isclose(model.dedicated_hot_usd(P, D), n * (8.00 + inst) + inst)
    assert model.hot_storage_usd(P, D) == model.dedicated_hot_usd(P, D)


def test_a_bigger_dedicated_instance_costs_more():
    assert model.dedicated_hot_usd(P, dict(D, instance_mult=4)) > model.dedicated_hot_usd(P, D)
