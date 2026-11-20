import contextlib
import copy
import io
import json
import sqlite3
import tempfile
import unittest
from pathlib import Path

import cross_check
from cross_check import fingerprint


class FingerprintTest(unittest.TestCase):
    def test_matches_usage_v2_fixture(self) -> None:
        detail = {
            "request_id": "req-123",
            "api_key": "api-key",
            "model": "gpt-5.6-sol",
            "timestamp": "2026-08-30T12:34:56.123456789Z",
            "source": "source.json",
            "auth_index": "7",
            "provider": "openai",
            "failed": True,
            "input_tokens": 1,
            "output_tokens": 2,
            "reasoning_tokens": 3,
            "cached_tokens": 4,
            "total_tokens": 10,
        }

        self.assertEqual(
            fingerprint(detail),
            "231fde333a097488867b61419171d9c4ddb24f1a",
        )


class SummaryProjectionTest(unittest.TestCase):
    def setUp(self) -> None:
        cross_check.warning_count = 0
        self.connection = sqlite3.connect(":memory:")
        self.addCleanup(self.connection.close)
        self.connection.executescript(
            "CREATE TABLE meta(key TEXT, value TEXT);INSERT INTO meta VALUES('summary_projection_v1','complete');"
        )
        fields = cross_check.EXACT_SUMMARY_FIELDS
        metrics = {field: 0 for field in fields}
        metrics.update(
            requests=1,
            success=1,
            input_tokens=100,
            output_tokens=20,
            cached_tokens=40,
            total_tokens=120,
            cost_micro=200,
        )
        dimensions = "model_id,credential_id,api_key_id,provider_id"
        for table in ("hour_bucket", "day_bucket"):
            columns = "bucket_ts_ns INTEGER," + ",".join(
                name + " INTEGER" for name in (*dimensions.split(","), *fields)
            )
            self.connection.execute(f"CREATE TABLE {table}({columns})")
            values = (0, 1, 3, 5, 4, *(metrics[field] for field in fields))
            self.connection.execute(f"INSERT INTO {table} VALUES(" + ",".join("?" for _ in values) + ")", values)
        columns = "grain_ns INTEGER,bucket_ts_ns INTEGER,dimension TEXT,members INTEGER,invalid INTEGER,"
        columns += ",".join(name + " INTEGER" for name in (*dimensions.split(","), *fields))
        self.connection.execute(f"CREATE TABLE summary_projection({columns})")
        for grain in (cross_check.NS_PER_HOUR, 24 * cross_check.NS_PER_HOUR, 720 * cross_check.NS_PER_HOUR):
            for scope, identity in (
                ("all", (None, None, None, None)),
                ("model", (1, None, None, None)),
                ("credential", (None, 3, None, 4)),
                ("api_key", (None, None, 5, None)),
            ):
                values = (grain, 0, scope, 1, 0, *identity, *(metrics[field] for field in fields))
                self.connection.execute(
                    "INSERT INTO summary_projection VALUES(" + ",".join("?" for _ in values) + ")", values
                )

    def test_all_projection_scopes_match_the_source_buckets(self) -> None:
        with contextlib.redirect_stdout(io.StringIO()) as output:
            cross_check.check_summary_projections(self.connection)
        self.assertIn("three grains, four scopes, 19 metrics", output.getvalue())
        self.assertEqual(cross_check.warning_count, 0)

    def test_invalid_projection_metrics_remain_unverified(self) -> None:
        self.connection.execute("UPDATE summary_projection SET invalid=1,cost_micro=0 WHERE dimension='all'")
        with contextlib.redirect_stdout(io.StringIO()) as output:
            cross_check.check_summary_projections(self.connection)
        self.assertIn("invalid projection groups use original buckets", output.getvalue())
        self.assertEqual(cross_check.warning_count, 3)

    def test_invalid_projection_must_still_match_source_members(self) -> None:
        self.connection.execute("UPDATE summary_projection SET invalid=1,members=2 WHERE dimension='all'")
        with contextlib.redirect_stdout(io.StringIO()), self.assertRaises(SystemExit) as raised:
            cross_check.check_summary_projections(self.connection)
        self.assertEqual(raised.exception.code, 1)


class PricingTest(unittest.TestCase):
    def setUp(self) -> None:
        cross_check.warning_count = 0
        self.connection = sqlite3.connect(":memory:")
        self.addCleanup(self.connection.close)
        self.connection.executescript("""
            CREATE TABLE dim_model(id INTEGER PRIMARY KEY, name TEXT);
            INSERT INTO dim_model VALUES(1, 'fixture-model');
            CREATE TABLE events(
                model_id INTEGER, input_tokens INTEGER, output_tokens INTEGER,
                cached_tokens INTEGER, total_tokens INTEGER, ts_ns INTEGER,
                cost_micro INTEGER, cache_read_tokens INTEGER,
                cache_write_tokens INTEGER, metadata TEXT, pricing_receipt TEXT
            );
        """)
        self.directory = tempfile.TemporaryDirectory()
        self.addCleanup(self.directory.cleanup)
        self.prices = Path(self.directory.name) / "prices.json"
        self.price = {"prompt": 2, "completion": 3, "cache": 0.5}

    def add_event(self, cost=200, **values) -> None:
        event = {
            "model_id": 1,
            "input_tokens": 100,
            "output_tokens": 20,
            "cached_tokens": 40,
            "total_tokens": 120,
            "ts_ns": 1_000_000_000,
            "cost_micro": cost,
            "cache_read_tokens": 0,
            "cache_write_tokens": 0,
            "metadata": None,
            "pricing_receipt": None,
        }
        event.update(values)
        self.connection.execute(
            "INSERT INTO events(" + ",".join(event) + ") VALUES(" + ",".join("?" for _ in event) + ")",
            tuple(event.values()),
        )

    def check(self, price=None, catalog=None) -> str:
        payload = catalog if catalog is not None else {"fixture-model": self.price if price is None else price}
        self.prices.write_text(json.dumps(payload))
        output = io.StringIO()
        with contextlib.redirect_stdout(output):
            cross_check.check_cost_recompute(self.connection, self.prices)
        return output.getvalue()

    @staticmethod
    def receipt(items):
        coverage = {}
        lines = []
        for category, tokens, rate, cost in items:
            coverage[category + "_tokens"] = str(tokens)
            lines.append(
                {
                    "category": category,
                    "tokens": str(tokens),
                    "rate_per_million": rate,
                    "rate_source": "fixture",
                    "cost_micro": str(cost),
                    "priced": rate is not None,
                }
            )
        coverage.update(
            {
                "total_tokens": str(sum(item[1] for item in items)),
                "priced_tokens": str(sum(item[1] for item in items if item[2] is not None)),
                "unpriced_tokens": str(sum(item[1] for item in items if item[2] is None)),
            }
        )
        return {
            "schema_version": 1,
            "cost_status": "complete",
            "model": "fixture-model",
            "canonical_model": "fixture-model",
            "source": "fixture",
            "currency": "USD",
            "captured_at": "2026-01-01T00:00:00Z",
            "priced_at": "2026-01-01T00:00:00Z",
            "cost_micro": str(sum(item[3] for item in items)),
            "coverage": coverage,
            "line_items": lines,
        }

    def captured_receipt(self):
        return self.receipt(
            [
                ("uncached_input", 60, 2, 120),
                ("cache_read", 40, 0.5, 20),
                ("output", 20, 3, 60),
            ]
        )

    def test_legacy_cached_input_is_not_charged_twice(self) -> None:
        self.add_event()
        self.assertIn("1 reference comparisons", self.check())
        self.assertEqual(cross_check.warning_count, 0)

    def test_old_double_counted_formula_is_rejected(self) -> None:
        self.add_event(cost=280)
        with self.assertRaises(SystemExit) as raised:
            self.check()
        self.assertEqual(raised.exception.code, 1)

    def test_explicit_cache_write_uses_its_own_rate(self) -> None:
        self.add_event(cost=220, cached_tokens=50, cache_read_tokens=40, cache_write_tokens=10)
        self.check({**self.price, "cache_write": 4})
        self.assertEqual(cross_check.warning_count, 0)

    def test_breakdown_preserves_unclassified_tokens(self) -> None:
        breakdown = {
            "schema_version": 2,
            "quality": "unclassified",
            "total_tokens": 130,
            "input": {"total_tokens": 100, "uncached_tokens": 80, "cache_read_tokens": 20, "cache_write_tokens": 0},
            "output": {"total_tokens": 20, "non_reasoning_tokens": 15, "reasoning_tokens": 5},
            "unclassified_tokens": 10,
        }
        self.add_event(
            cost=230, cached_tokens=20, total_tokens=130, metadata=json.dumps({"token_breakdown": breakdown})
        )
        self.check()
        self.assertEqual(cross_check.warning_count, 0)

    def test_immutable_receipt_keeps_its_captured_price(self) -> None:
        self.add_event(pricing_receipt=json.dumps(self.captured_receipt()))
        self.assertIn("1 immutable receipts", self.check({"prompt": 3, "completion": 4, "cache": 0.5}))
        self.assertEqual(cross_check.warning_count, 0)

    def test_repriced_cost_is_separate_from_immutable_receipt(self) -> None:
        self.add_event(cost=280, pricing_receipt=json.dumps(self.captured_receipt()))
        self.assertIn("1 repriced rows", self.check({"prompt": 3, "completion": 4, "cache": 0.5}))
        self.assertEqual(cross_check.warning_count, 0)

    def test_prior_repricing_without_a_snapshot_is_unverified(self) -> None:
        self.add_event(cost=250, pricing_receipt=json.dumps(self.captured_receipt()))
        self.assertIn(
            "prior repricing snapshot is unavailable", self.check({"prompt": 3, "completion": 4, "cache": 0.5})
        )
        self.assertEqual(cross_check.warning_count, 1)

    def test_corrupt_captured_rate_arithmetic_is_rejected(self) -> None:
        receipt = self.captured_receipt()
        receipt["line_items"][0]["cost_micro"] = "121"
        receipt["cost_micro"] = "201"
        self.add_event(pricing_receipt=json.dumps(receipt))
        with self.assertRaises(SystemExit):
            self.check()

    def test_fractional_receipt_cost_uses_largest_remainder(self) -> None:
        receipt = self.receipt([("uncached_input", 1, 0.5, 1), ("output", 1, 0.5, 0)])
        original = copy.deepcopy(receipt)
        self.assertEqual(cross_check.validate_receipt(receipt, 2)["uncached_input"], 1)
        self.assertEqual(receipt, original)

    def test_cache_write_ttl_rates_are_distinct(self) -> None:
        receipt = self.receipt(
            [
                ("uncached_input", 50, 2, 100),
                ("cache_read", 20, 0.5, 10),
                ("cache_write_5m", 10, 2.5, 25),
                ("cache_write_1h", 10, 4, 40),
                ("cache_write_unknown", 10, 4, 40),
                ("output", 20, 3, 60),
            ]
        )
        self.add_event(
            cost=275, cached_tokens=50, cache_read_tokens=20, cache_write_tokens=30, pricing_receipt=json.dumps(receipt)
        )
        self.check({**self.price, "cache_write_5m": 2.5, "cache_write_1h": 4})
        self.assertEqual(cross_check.warning_count, 0)
        amounts = cross_check.validate_receipt(receipt, 120)
        self.assertEqual(
            cross_check.reference_cost(amounts, {**self.price, "cache_write_5m": 3, "cache_write_1h": 5}, True), 300
        )

    def test_missing_reference_is_reported_as_unverified(self) -> None:
        self.add_event()
        self.assertIn("no supplied reference price", self.check(catalog={}))
        self.assertEqual(cross_check.warning_count, 1)

    def test_ambiguous_oauth_price_alias_is_not_selected(self) -> None:
        self.add_event()
        catalog = {
            "schema_version": 3,
            "prices": {"first": self.price, "second": {"prompt": 10}},
            "oauth_model_alias": {
                "codex": [{"name": "first", "alias": "fixture-model"}],
                "claude": [{"name": "second", "alias": "fixture-model"}],
            },
        }
        self.assertIn("no supplied reference price", self.check(catalog=catalog))
        self.assertEqual(cross_check.warning_count, 1)

    def test_direct_price_wins_over_oauth_alias(self) -> None:
        self.add_event()
        catalog = {
            "schema_version": 3,
            "prices": {"fixture-model": self.price, "other": {"prompt": 10}},
            "oauth_model_alias": {"codex": [{"name": "other", "alias": "fixture-model"}]},
        }
        self.check(catalog=catalog)
        self.assertEqual(cross_check.warning_count, 0)

    def test_nanosecond_price_boundaries_remain_exact(self) -> None:
        base = cross_check.price_boundary_ns("2026-01-01T00:00:00Z", False)
        price = {
            "prompt": 2,
            "effective_from": "2026-01-01T00:00:00.000000001Z",
            "effective_until": "2026-01-01T00:00:00.000000003Z",
        }
        self.assertIsNone(cross_check.reference_price_at(price, base))
        self.assertIsNotNone(cross_check.reference_price_at(price, base + 1))
        self.assertIsNotNone(cross_check.reference_price_at(price, base + 2))
        self.assertIsNone(cross_check.reference_price_at(price, base + 3))
        self.assertEqual(cross_check.price_boundary_ns("1969-12-31T23:59:59.999999999Z", False), -1)

    def test_date_only_end_boundary_includes_the_whole_day(self) -> None:
        end = cross_check.price_boundary_ns("2026-01-02T00:00:00Z", False)
        price = {"prompt": 2, "effective_until": "2026-01-01"}
        self.assertIsNotNone(cross_check.reference_price_at(price, end - 1))
        self.assertIsNone(cross_check.reference_price_at(price, end))

    def test_utc_price_band_replaces_the_base_rates(self) -> None:
        ts = cross_check.price_boundary_ns("2026-11-02T00:30:00Z", False)
        self.add_event(cost=104, ts_ns=ts)
        self.check(
            {
                **self.price,
                "utc_rate_bands": [
                    {
                        "weekdays": ["Monday"],
                        "start": "00:00",
                        "end": "01:00",
                        "prompt": 1,
                        "completion": 2,
                        "cache_read": 0.1,
                    }
                ],
            }
        )
        self.assertEqual(cross_check.warning_count, 0)


if __name__ == "__main__":
    unittest.main()
