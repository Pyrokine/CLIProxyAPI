#!/usr/bin/env python3
# CPA usage 数据离线交叉验证(R-552)
from __future__ import annotations

import argparse
import datetime as dt
import hashlib
import json
import math
import re
import sqlite3
import struct
import sys
from pathlib import Path

SUMMARY_FIELDS = (
    "requests",
    "success",
    "failure",
    "input_tokens",
    "output_tokens",
    "reasoning_tokens",
    "cached_tokens",
    "total_tokens",
    "cost_micro",
)
EXACT_SUMMARY_FIELDS = SUMMARY_FIELDS + (
    "cache_read_tokens",
    "cache_write_tokens",
    "complete_requests",
    "partial_requests",
    "unpriced_requests",
    "complete_tokens",
    "partial_tokens",
    "unpriced_tokens",
    "latency_ms_total",
    "latency_samples",
)
BUCKET_DIMENSIONS = "model_id,credential_id,api_key_id,provider_id"
NS_PER_HOUR = 3_600_000_000_000
warning_count = 0


def append_fingerprint_string(payload: bytearray, value: object) -> None:
    encoded = str(value or "").encode()
    payload.extend(struct.pack("<Q", len(encoded)))
    payload.extend(encoded)


def append_fingerprint_int64(payload: bytearray, value: object) -> None:
    payload.extend(struct.pack("<q", int(value or 0)))


def fingerprint(d: dict) -> str:
    payload = bytearray(b"usage-fingerprint-v2")
    for field in (
        "request_id",
        "api_key",
        "model",
        "timestamp",
        "source",
        "auth_index",
        "provider",
    ):
        append_fingerprint_string(payload, d.get(field, ""))
    payload.append(1 if d.get("failed") else 0)
    for field in (
        "input_tokens",
        "output_tokens",
        "reasoning_tokens",
        "cached_tokens",
        "total_tokens",
    ):
        append_fingerprint_int64(payload, d.get(field, 0))
    return hashlib.sha256(payload).digest()[:20].hex()


def fail(msg: str) -> None:
    print(f"FAIL: {msg}")
    sys.exit(1)


def warn(msg: str) -> None:
    global warning_count
    warning_count += 1
    print(f"WARN: {msg}")


def ok(msg: str) -> None:
    print(f"OK:   {msg}")


def ns_to_rfc3339_nano(ts_ns: int) -> str:
    sec, ns = divmod(int(ts_ns), 1_000_000_000)
    base = dt.datetime.fromtimestamp(sec, dt.timezone.utc).strftime("%Y-%m-%dT%H:%M:%S")
    if ns == 0:
        return base + "Z"
    frac = f"{ns:09d}".rstrip("0")
    return f"{base}.{frac}Z"


def int_value(value: object) -> int:
    if value is None:
        return 0
    return int(value)


def totals_query(table: str) -> str:
    return (
        "SELECT COALESCE(SUM(requests),0), COALESCE(SUM(success),0), COALESCE(SUM(failure),0), "
        "COALESCE(SUM(input_tokens),0), COALESCE(SUM(output_tokens),0), "
        "COALESCE(SUM(reasoning_tokens),0), COALESCE(SUM(cached_tokens),0), "
        f"COALESCE(SUM(total_tokens),0), COALESCE(SUM(cost_micro),0) FROM {table}"
    )


def fetch_totals(conn: sqlite3.Connection, table: str) -> tuple[int, ...]:
    row = conn.execute(totals_query(table)).fetchone()
    return tuple(int_value(value) for value in row)


def fetch_grouped_totals(
    conn: sqlite3.Connection,
    table: str,
    dimension: str,
    fields: tuple[str, ...] = SUMMARY_FIELDS,
) -> dict[tuple, tuple[int, ...]]:
    expressions = {field: f"COALESCE(SUM({field}),0)" for field in fields}
    if table == "events":
        expressions.update(
            {
                "requests": "COUNT(*)",
                "success": "COALESCE(SUM(CASE WHEN failed=0 THEN 1 ELSE 0 END),0)",
                "failure": "COALESCE(SUM(CASE WHEN failed!=0 THEN 1 ELSE 0 END),0)",
            }
        )
        for status in ("complete", "partial", "unpriced"):
            expressions[f"{status}_requests"] = f"SUM(CASE WHEN cost_status='{status}' THEN 1 ELSE 0 END)"
            expressions[f"{status}_tokens"] = f"SUM(CASE WHEN cost_status='{status}' THEN total_tokens ELSE 0 END)"
        latency = (
            "CASE WHEN json_valid(metadata) AND json_type(metadata,'$.latency_ms')='integer' "
            "AND json_extract(metadata,'$.latency_ms')>0 THEN json_extract(metadata,'$.latency_ms') ELSE 0 END"
        )
        expressions["latency_ms_total"] = f"SUM({latency})"
        expressions["latency_samples"] = f"SUM(CASE WHEN ({latency})>0 THEN 1 ELSE 0 END)"
    query = f"SELECT {dimension}," + ",".join(expressions[field] for field in fields)
    query += f" FROM {table} GROUP BY {dimension}"
    return {
        tuple(row[: -len(fields)]): tuple(int_value(value) for value in row[-len(fields) :])
        for row in conn.execute(query)
    }


def compare_grouped_totals(
    label: str,
    left: dict[tuple, tuple[int, ...]],
    right: dict[tuple, tuple[int, ...]],
    fields: tuple[str, ...] = SUMMARY_FIELDS,
) -> None:
    empty = (0,) * len(fields)
    for key in sorted(left.keys() | right.keys(), key=repr):
        compare_totals(f"{label} {key!r}", left.get(key, empty), right.get(key, empty), fields)


def bucket_expression(column: str, grain: int) -> str:
    return f"CASE WHEN {column}<0 THEN (({column}+1)/{grain}-1)*{grain} ELSE {column}/{grain}*{grain} END"


def fetch_event_totals(conn: sqlite3.Connection) -> tuple[int, ...]:
    row = conn.execute(
        """
        SELECT COUNT(*),
               COALESCE(SUM(CASE WHEN failed = 0 THEN 1 ELSE 0 END), 0),
               COALESCE(SUM(CASE WHEN failed != 0 THEN 1 ELSE 0 END), 0),
               COALESCE(SUM(input_tokens), 0),
               COALESCE(SUM(output_tokens), 0),
               COALESCE(SUM(reasoning_tokens), 0),
               COALESCE(SUM(cached_tokens), 0),
               COALESCE(SUM(total_tokens), 0),
               COALESCE(SUM(cost_micro), 0)
        FROM events
        """
    ).fetchone()
    return tuple(int_value(value) for value in row)


def compare_totals(
    label: str,
    left: tuple[int, ...],
    right: tuple[int, ...],
    fields: tuple[str, ...] = SUMMARY_FIELDS,
) -> None:
    if left != right:
        detail = ", ".join(
            f"{field}: {left_value} != {right_value}"
            for field, left_value, right_value in zip(fields, left, right)
            if left_value != right_value
        )
        fail(f"{label} mismatch: {detail}")


def check_fingerprint_roundtrip(conn: sqlite3.Connection) -> None:
    rows = conn.execute(
        """
        SELECT events.fingerprint,
               CASE WHEN json_valid(events.metadata)
                    THEN COALESCE(json_extract(events.metadata, '$.request_id'), '')
                    ELSE ''
               END,
               (SELECT name FROM dim_api_key WHERE id = events.api_key_id),
               (SELECT name FROM dim_model WHERE id = events.model_id),
               events.ts_ns,
               (SELECT name FROM dim_source WHERE id = events.source_id),
               (SELECT name FROM dim_auth_index WHERE id = events.auth_index_id),
               (SELECT name FROM dim_provider WHERE id = events.provider_id),
               events.failed,
               events.input_tokens,
               events.output_tokens,
               events.reasoning_tokens,
               events.cached_tokens,
               events.total_tokens
        FROM events
        """
    ).fetchall()
    mismatches = 0
    sample = []

    for row in rows:
        (fp_db, request_id, api_key, model, ts_ns, source, auth_index, provider, failed, it, ot, rt, ct, tt) = row
        ts_iso = ns_to_rfc3339_nano(ts_ns)
        d = {
            "request_id": request_id or "",
            "api_key": api_key or "",
            "model": model or "",
            "timestamp": ts_iso,
            "source": source or "",
            "auth_index": auth_index or "",
            "provider": provider or "",
            "failed": bool(failed),
            "input_tokens": it,
            "output_tokens": ot,
            "reasoning_tokens": rt,
            "cached_tokens": ct,
            "total_tokens": tt,
        }
        fp_db_hex = fp_db.hex() if isinstance(fp_db, bytes) else str(fp_db)
        recomputed = fingerprint(d)
        if recomputed != fp_db_hex:
            mismatches += 1
            if len(sample) < 3:
                sample.append((fp_db_hex, recomputed, model, ts_iso))
    if mismatches:
        for s in sample:
            print(f"  db={s[0]} recomputed={s[1]} model={s[2]} ts={s[3]}")
        fail(f"fingerprint mismatches: {mismatches} / {len(rows)}")
    ok(f"fingerprint round-trip: {len(rows)} rows")


def check_totals_conservation(
    conn: sqlite3.Connection,
    complete_hours: set[int] | None = None,
) -> None:
    if complete_hours is None:
        complete_hours = check_hour_bucket_vs_events(conn)
    actual = fetch_grouped_totals(conn, "hour_bucket", "bucket_ts_ns," + BUCKET_DIMENSIONS)
    expected = fetch_grouped_totals(conn, "events", bucket_expression("ts_ns", NS_PER_HOUR) + "," + BUCKET_DIMENSIONS)
    actual = {key: value for key, value in actual.items() if key[0] in complete_hours}
    expected = {key: value for key, value in expected.items() if key[0] in complete_hours}
    compare_grouped_totals("hour/model/credential/api_key/provider conservation", actual, expected)
    ok(f"dimension conservation: {len(actual)} groups over {len(complete_hours)} fully retained hours")


def check_hour_bucket_vs_events(conn: sqlite3.Connection) -> set[int]:
    expected = fetch_grouped_totals(conn, "events", bucket_expression("ts_ns", NS_PER_HOUR))
    actual = fetch_grouped_totals(conn, "hour_bucket", "bucket_ts_ns")
    complete_hours = set()
    historical = 0
    empty = (0,) * len(SUMMARY_FIELDS)
    for key in sorted(actual.keys() | expected.keys()):
        bucket, events = actual.get(key, empty), expected.get(key, empty)
        if bucket[0] < events[0]:
            fail(f"hour_bucket {key[0]} has {bucket[0]} requests for {events[0]} retained events")
        if bucket[0] > events[0]:
            historical += 1
            continue
        compare_totals(f"hour_bucket vs events at {key[0]}", bucket, events)
        complete_hours.add(key[0])
    if historical:
        warn(
            f"{historical} hourly buckets include history without complete retained details; "
            "their event-level allocation is unverified"
        )
    ok(f"hour_bucket vs events: {len(complete_hours)} fully retained hours")
    return complete_hours


def check_exact_summary(conn: sqlite3.Connection) -> None:
    if conn.execute("SELECT 1 FROM sqlite_master WHERE type='table' AND name='summary_bucket'").fetchone() is None:
        warn("summary_bucket is absent; exact summary validation requires a migrated database")
        return
    state = conn.execute("SELECT value FROM meta WHERE key='summary_grains_v1'").fetchone()
    if state is None or state[0] != "complete":
        fail("exact summary migration is incomplete")
    dimensions = BUCKET_DIMENSIONS + ",auth_index_id"
    for grain in (1, 600_000_000_000, NS_PER_HOUR):
        expected = fetch_grouped_totals(
            conn, "events", bucket_expression("ts_ns", grain) + "," + dimensions, EXACT_SUMMARY_FIELDS
        )
        actual = fetch_grouped_totals(
            conn,
            f"(SELECT * FROM summary_bucket WHERE grain_ns={grain})",
            "bucket_ts_ns," + dimensions,
            EXACT_SUMMARY_FIELDS,
        )
        compare_grouped_totals(f"summary_bucket grain={grain}", actual, expected, EXACT_SUMMARY_FIELDS)
    ok(f"exact summary: three grains, five dimensions, {len(EXACT_SUMMARY_FIELDS)} metrics")


def check_summary_projections(conn: sqlite3.Connection) -> None:
    if conn.execute("SELECT 1 FROM sqlite_master WHERE type='table' AND name='summary_projection'").fetchone() is None:
        warn("summary_projection is absent; aggregate query validation requires a migrated database")
        return
    state = conn.execute("SELECT value FROM meta WHERE key='summary_projection_v1'").fetchone()
    if state is None or state[0] != "complete":
        fail("summary projection migration is incomplete")
    fields = ("members",) + EXACT_SUMMARY_FIELDS
    names = ("model_id", "credential_id", "api_key_id", "provider_id")
    dimensions = "bucket_ts_ns," + ",".join(names)
    scopes = {
        "all": ("NULL", "NULL", "NULL", "NULL"),
        "model": ("b.model_id", "NULL", "NULL", "NULL"),
        "credential": ("NULL", "b.credential_id", "NULL", "b.provider_id"),
        "api_key": ("NULL", "NULL", "b.api_key_id", "NULL"),
    }
    for grain, table in (
        (NS_PER_HOUR, "hour_bucket"),
        (24 * NS_PER_HOUR, "day_bucket"),
        (720 * NS_PER_HOUR, "day_bucket"),
    ):
        bucket = bucket_expression("b.bucket_ts_ns", grain)
        for scope, values in scopes.items():
            condition = "b.credential_id IS NOT NULL" if scope == "credential" else "1"
            if grain == 720 * NS_PER_HOUR:
                condition += f" AND b.bucket_ts_ns>={-(2**63) + grain - 1}"
            selection = ",".join(value + " AS " + name for name, value in zip(names, values))
            base = f"SELECT {bucket} AS bucket_ts_ns,{selection},1 AS members,"
            base += ",".join("b." + field for field in EXACT_SUMMARY_FIELDS)
            base += f" FROM {table} b WHERE {condition}"
            key = f"p.grain_ns={grain} AND p.dimension='{scope}' AND p.bucket_ts_ns=({bucket})"
            key += "".join(" AND p." + name + " IS " + value for name, value in zip(names, values))
            source = f"({base} AND NOT EXISTS(SELECT 1 FROM summary_projection p WHERE p.invalid=1 AND {key}))"
            actual = f"(SELECT * FROM summary_projection WHERE grain_ns={grain} AND dimension='{scope}')"
            expected = fetch_grouped_totals(conn, source, dimensions, fields)
            valid = fetch_grouped_totals(conn, f"(SELECT * FROM {actual} WHERE invalid=0)", dimensions, fields)
            label = f"summary_projection grain={grain} dimension={scope}"
            compare_grouped_totals(label, valid, expected, fields)
            invalid = conn.execute(f"SELECT COUNT(*) FROM {actual} WHERE invalid=1").fetchone()[0]
            if invalid:
                expected_members = fetch_grouped_totals(conn, f"({base})", dimensions, ("members",))
                actual_members = fetch_grouped_totals(conn, actual, dimensions, ("members",))
                compare_grouped_totals(label + " members", actual_members, expected_members, ("members",))
                warn(
                    f"{label}: {invalid} invalid projection groups use original buckets; "
                    "projected metrics remain unverified"
                )
    ok(f"summary projections: three grains, four scopes, {len(EXACT_SUMMARY_FIELDS)} metrics")


def model_price_key(model: str) -> str:
    model = model.strip()
    if model[:7].lower() == "models/":
        model = model[7:]
    return model.strip().lower()


def price_boundary_ns(value: str, exclusive: bool) -> int:
    fraction = re.search(r"[.,](\d+)", value)
    remainder = 0
    normalized = value
    if fraction is not None:
        digits = fraction.group(1).ljust(9, "0")
        remainder = int(digits[6:9])
        normalized = value[: fraction.start()] + "." + digits[:6] + value[fraction.end() :]
    parsed = dt.datetime.fromisoformat(normalized.replace("Z", "+00:00"))
    if parsed.tzinfo is None:
        parsed = parsed.replace(tzinfo=dt.timezone.utc)
    if exclusive and len(value) == 10:
        parsed += dt.timedelta(days=1)
    delta = parsed - dt.datetime(1970, 1, 1, tzinfo=dt.timezone.utc)
    return (delta.days * 86400 + delta.seconds) * 1_000_000_000 + delta.microseconds * 1000 + remainder


def reference_price_at(price: dict, ts_ns: int) -> dict | None:
    at = dt.datetime.fromtimestamp(ts_ns // 1_000_000_000, dt.timezone.utc)
    for field, exclusive in (("effective_from", False), ("effective_until", True)):
        boundary = str(price.get(field) or "").strip()
        if not boundary:
            continue
        boundary_ns = price_boundary_ns(boundary, exclusive)
        if (exclusive and ts_ns >= boundary_ns) or (not exclusive and ts_ns < boundary_ns):
            return None
    if str(price.get("currency") or "USD").strip().upper() != "USD":
        return None
    weekday = ("monday", "tuesday", "wednesday", "thursday", "friday", "saturday", "sunday")[at.weekday()]
    minute = at.hour * 60 + at.minute
    for band in price.get("utc_rate_bands") or []:
        start, end = (
            sum(int(part) * factor for part, factor in zip(str(band[key]).split(":"), (60, 1)))
            for key in ("start", "end")
        )
        if weekday in {day.strip().lower() for day in band["weekdays"]} and start <= minute < end:
            return {
                **price,
                **{
                    field: band.get(field)
                    for field in (
                        "prompt",
                        "completion",
                        "cache",
                        "cache_read",
                        "cache_write",
                        "cache_write_5m",
                        "cache_write_1h",
                    )
                },
            }
    return price


def validate_receipt(receipt: dict, total_tokens: int) -> dict[str, int]:
    categories = (
        "uncached_input",
        "cache_read",
        "cache_write_5m",
        "cache_write_1h",
        "cache_write_unknown",
        "output",
        "reasoning",
        "unclassified",
    )
    if receipt.get("schema_version") != 1:
        fail("unsupported pricing receipt schema")
    if receipt.get("cost_status") not in ("complete", "partial", "unpriced"):
        fail("invalid pricing receipt cost status")
    coverage = receipt.get("coverage", {})
    amounts = {category: int_value(coverage.get(category + "_tokens")) for category in categories}
    priced, unpriced = int_value(coverage.get("priced_tokens")), int_value(coverage.get("unpriced_tokens"))
    if (
        min(*amounts.values(), priced, unpriced) < 0
        or sum(amounts.values()) != total_tokens
        or int_value(coverage.get("total_tokens")) != total_tokens
        or priced + unpriced != total_tokens
    ):
        fail("pricing receipt coverage does not match event tokens")
    seen = set()
    line_cost = line_priced = line_unpriced = 0
    raw_costs = []
    items = receipt.get("line_items", [])
    for item in items:
        category = item.get("category")
        tokens, cost = int_value(item.get("tokens")), int_value(item.get("cost_micro"))
        if category not in amounts or category in seen or tokens <= 0 or tokens != amounts[category] or cost < 0:
            fail("invalid pricing receipt line item")
        seen.add(category)
        line_cost += cost
        if item.get("priced"):
            rate = item.get("rate_per_million")
            if type(rate) not in (int, float) or not math.isfinite(rate) or rate < 0:
                fail("invalid pricing receipt rate")
            raw_costs.append(float(tokens) * rate)
            line_priced += tokens
        else:
            if item.get("rate_per_million") is not None or cost != 0:
                fail("unpriced receipt line has a priced amount")
            raw_costs.append(0.0)
            line_unpriced += tokens
    raw_total = sum(raw_costs)
    if not math.isfinite(raw_total) or raw_total < 0 or raw_total >= 2**63:
        fail("pricing receipt cost is outside int64 range")
    expected_costs = [int(value) for value in raw_costs]
    remainder = int(raw_total) - sum(expected_costs)
    order = sorted(
        (i for i, item in enumerate(items) if item.get("priced")),
        key=lambda i: (-(raw_costs[i] - expected_costs[i]), i),
    )
    for index in order[:remainder]:
        expected_costs[index] += 1
    if any(int_value(item.get("cost_micro")) != expected for item, expected in zip(items, expected_costs)):
        fail("pricing receipt line cost does not match captured rate")
    if (
        seen != {category for category, tokens in amounts.items() if tokens > 0}
        or line_priced != priced
        or line_unpriced != unpriced
        or line_cost != int_value(receipt.get("cost_micro"))
    ):
        fail("pricing receipt line items do not reconcile")
    return amounts


def reference_cost(amounts: dict[str, int], price: dict | None, receipt: bool) -> int:
    if price is None:
        return 0

    def rate(name: str, default: float = 0) -> float:
        value = price.get(name)
        value = default if value is None else value
        if type(value) not in (int, float) or not math.isfinite(value) or value < 0:
            fail(f"invalid {name} reference price")
        return float(value)

    prompt, completion = rate("prompt"), rate("completion")
    write = rate("cache_write", prompt)
    unknown_write = write
    if receipt and price.get("cache_write") is None:
        ttl_rates = [rate(name) for name in ("cache_write_5m", "cache_write_1h") if price.get(name) is not None]
        if ttl_rates:
            unknown_write = max(ttl_rates)
    rates = {
        "uncached_input": prompt,
        "cache_read": rate("cache_read", rate("cache")),
        "cache_write_5m": rate("cache_write_5m", write),
        "cache_write_1h": rate("cache_write_1h", write),
        "cache_write_unknown": unknown_write,
        "output": completion,
        "reasoning": completion,
        "unclassified": 0,
    }
    raw = sum(float(tokens) * rates[category] for category, tokens in amounts.items())
    if not math.isfinite(raw) or raw < 0 or raw >= 2**63:
        fail("reference cost is outside int64 range")
    return int(raw)


def check_cost_recompute(conn: sqlite3.Connection, prices_path: Path) -> None:
    if not prices_path.exists():
        warn(f"prices file missing: {prices_path}, skipping cost recompute")
        return
    catalog = json.loads(prices_path.read_text())
    prices = (catalog.get("prices") or {}) if "schema_version" in catalog else catalog
    prices = {model_price_key(model): value for model, value in prices.items()}
    aliases, conflicts = {}, set()
    channels = (catalog.get("oauth_model_alias") or {}).values() if "schema_version" in catalog else ()
    for channel in channels:
        for alias in channel:
            key, target = model_price_key(alias["alias"]), model_price_key(alias["name"])
            if not key or key in prices or target not in prices:
                continue
            if key in aliases and aliases[key] != target:
                conflicts.add(key)
            aliases[key] = target
    for key, target in aliases.items():
        if key not in conflicts:
            prices[key] = prices[target]
    columns = {row[1] for row in conn.execute("PRAGMA table_info(events)")}
    optional = [
        name if name in columns else "NULL"
        for name in ("cache_read_tokens", "cache_write_tokens", "metadata", "pricing_receipt")
    ]
    rows = conn.execute(
        "SELECT m.name,e.input_tokens,e.output_tokens,e.cached_tokens,e.total_tokens,e.ts_ns,e.cost_micro,"
        + ",".join("e." + name if name != "NULL" else name for name in optional)
        + " FROM events e JOIN dim_model m ON m.id=e.model_id"
    )
    checked = captured = repriced = missing = 0
    for model, it, ot, cached, total, ts_ns, stored, read, write, metadata, raw_receipt in rows:
        receipt = json.loads(raw_receipt) if raw_receipt else None
        if receipt is not None:
            amounts = validate_receipt(receipt, total)
            captured += 1
        else:
            detail = json.loads(metadata) if metadata else {}
            breakdown = detail.get("token_breakdown")
            if breakdown is not None:
                if breakdown.get("schema_version") != 2 or breakdown.get("quality") not in (
                    "complete",
                    "unclassified",
                    "inconsistent",
                ):
                    fail("unsupported token breakdown schema or quality")
                input_part, output_part = breakdown["input"], breakdown["output"]
                uncached, read, write = (
                    int_value(input_part.get(name))
                    for name in ("uncached_tokens", "cache_read_tokens", "cache_write_tokens")
                )
                output = int_value(output_part.get("total_tokens"))
                non_reasoning, reasoning = (
                    int_value(output_part.get("non_reasoning_tokens")),
                    int_value(output_part.get("reasoning_tokens")),
                )
                unclassified = int_value(breakdown.get("unclassified_tokens"))
                if (
                    min(uncached, read, write, output, non_reasoning, reasoning, unclassified) < 0
                    or uncached + read + write != int_value(input_part.get("total_tokens"))
                    or non_reasoning + reasoning != output
                    or int_value(input_part.get("total_tokens")) + output + unclassified != total
                    or int_value(breakdown.get("total_tokens")) != total
                    or (breakdown.get("quality") == "complete" and unclassified != 0)
                ):
                    fail("invalid token breakdown totals")
                if breakdown.get("quality") == "inconsistent":
                    uncached = read = write = output = 0
            else:
                read, write = int_value(read), int_value(write)
                if read == 0 and write == 0:
                    read = int_value(cached)
                uncached = it - read - write if read + write <= it else it
                output = ot
            amounts = {"uncached_input": uncached, "cache_read": read, "cache_write_unknown": write, "output": output}
        price = prices.get(model_price_key(model))
        if price is None and receipt is not None:
            price = prices.get(model_price_key(receipt.get("canonical_model", "")))
        if price is None:
            missing += 1
            continue
        expected = reference_cost(amounts, reference_price_at(price, ts_ns), receipt is not None)
        checked += 1
        if receipt is not None and stored == int_value(receipt.get("cost_micro")):
            continue
        if abs(expected - stored) > 1:
            if receipt is not None:
                warn(
                    f"model={model!r} stored derived cost={stored}, current catalog={expected}; "
                    "the prior repricing snapshot is unavailable"
                )
            else:
                fail(f"model={model!r} cost_micro mismatch: expected={expected}, stored={stored}")
        elif receipt is not None and stored != int_value(receipt.get("cost_micro")):
            repriced += 1
    if missing:
        warn(f"{missing} rows have no supplied reference price; current-catalog comparison is incomplete")
    ok(
        f"cost_micro recompute: {checked} reference comparisons, "
        f"{captured} immutable receipts, {repriced} repriced rows"
    )


def check_oauth_null_conservation(
    conn: sqlite3.Connection,
    complete_hours: set[int] | None = None,
) -> None:
    if complete_hours is None:
        complete_hours = check_hour_bucket_vs_events(conn)
    events = fetch_grouped_totals(conn, "events", bucket_expression("ts_ns", NS_PER_HOUR) + ",api_key_id")
    buckets = fetch_grouped_totals(conn, "hour_bucket", "bucket_ts_ns,api_key_id")
    events = {key: value for key, value in events.items() if key[0] in complete_hours and key[1] is None}
    buckets = {key: value for key, value in buckets.items() if key[0] in complete_hours and key[1] is None}
    compare_grouped_totals("OAuth NULL conservation", buckets, events)
    ok(f"OAuth NULL conservation: {sum(value[0] for value in events.values())} requests in fully retained hours")


def normalize_snapshot_payload(raw: object) -> dict:
    if not isinstance(raw, dict):
        fail("snapshot root must be an object")
    usage = raw.get("usage")
    if isinstance(usage, dict):
        return usage
    return raw


def build_snapshot_from_db(conn: sqlite3.Connection) -> dict:
    total = fetch_totals(conn, "hour_bucket")
    snapshot = {
        "total_requests": total[0],
        "success_count": total[1],
        "failure_count": total[2],
        "total_tokens": total[7],
        "apis": {},
        "requests_by_day": {},
        "requests_by_hour": {},
        "tokens_by_day": {},
        "tokens_by_hour": {},
    }

    for day, req, tokens in conn.execute(
        """
            SELECT strftime('%Y-%m-%d', bucket_ts_ns / 1000000000, 'unixepoch'),
                   SUM(requests),
                   SUM(total_tokens)
            FROM hour_bucket
            GROUP BY 1
            ORDER BY 1
            """
    ):
        snapshot["requests_by_day"][day] = int_value(req)
        snapshot["tokens_by_day"][day] = int_value(tokens)

    for hour, req, tokens in conn.execute(
        """
            SELECT strftime('%H', bucket_ts_ns / 1000000000, 'unixepoch'),
                   SUM(requests),
                   SUM(total_tokens)
            FROM hour_bucket
            GROUP BY 1
            ORDER BY 1
            """
    ):
        snapshot["requests_by_hour"][hour] = int_value(req)
        snapshot["tokens_by_hour"][hour] = int_value(tokens)

    rows = conn.execute(
        """
        SELECT events.ts_ns,
               (SELECT name FROM dim_model WHERE id = events.model_id),
               (SELECT name FROM dim_source WHERE id = events.source_id),
               (SELECT name FROM dim_auth_index WHERE id = events.auth_index_id),
               (SELECT name FROM dim_api_key WHERE id = events.api_key_id),
               events.input_tokens,
               events.output_tokens,
               events.reasoning_tokens,
               events.cached_tokens,
               events.total_tokens,
               events.failed
        FROM events
        ORDER BY events.ts_ns
        """
    ).fetchall()
    for row in rows:
        ts_ns, model, source, auth_index, api_key, it, ot, rt, ct, tt, failed = row
        api_name = api_key or "unknown"
        model_name = model or "unknown"
        apis = snapshot["apis"]
        api = apis.setdefault(api_name, {"total_requests": 0, "total_tokens": 0, "models": {}})
        models = api["models"]
        model_entry = models.setdefault(model_name, {"total_requests": 0, "total_tokens": 0, "details": []})
        detail = {
            "timestamp": ns_to_rfc3339_nano(ts_ns),
            "source": source or "",
            "auth_index": auth_index or "",
            "tokens": {
                "input_tokens": int_value(it),
                "output_tokens": int_value(ot),
                "reasoning_tokens": int_value(rt),
                "cached_tokens": int_value(ct),
                "total_tokens": int_value(tt),
            },
            "failed": bool(failed),
        }
        model_entry["details"].append(detail)
        model_entry["total_requests"] += 1
        model_entry["total_tokens"] += int_value(tt)
        api["total_requests"] += 1
        api["total_tokens"] += int_value(tt)
    return snapshot


def object_items(value: object):
    return value.items() if isinstance(value, dict) else ()


def int_map(value: object) -> dict:
    return {str(k): int_value(v) for k, v in object_items(value)}


def comparable_detail(detail: object) -> dict:
    if not isinstance(detail, dict):
        return {}
    tokens = detail.get("tokens") if isinstance(detail.get("tokens"), dict) else {}
    return {
        "timestamp": str(detail.get("timestamp", "")),
        "source": str(detail.get("source") or ""),
        "auth_index": str(detail.get("auth_index") or ""),
        "tokens": {
            key: int_value(tokens.get(key))
            for key in (
                "input_tokens",
                "output_tokens",
                "reasoning_tokens",
                "cached_tokens",
                "total_tokens",
            )
        },
        "failed": bool(detail.get("failed", False)),
    }


def normalize_snapshot_for_compare(snapshot: dict) -> dict:
    normalized = {
        "total_requests": int_value(snapshot.get("total_requests", snapshot.get("TotalRequests"))),
        "success_count": int_value(snapshot.get("success_count", snapshot.get("SuccessCount"))),
        "failure_count": int_value(snapshot.get("failure_count", snapshot.get("FailureCount"))),
        "total_tokens": int_value(snapshot.get("total_tokens", snapshot.get("TotalTokens"))),
        "requests_by_day": int_map(snapshot.get("requests_by_day")),
        "requests_by_hour": int_map(snapshot.get("requests_by_hour")),
        "tokens_by_day": int_map(snapshot.get("tokens_by_day")),
        "tokens_by_hour": int_map(snapshot.get("tokens_by_hour")),
        "apis": {},
    }
    for api_name, api_value in object_items(snapshot.get("apis")):
        if not isinstance(api_value, dict):
            continue
        api = {
            "total_requests": int_value(api_value.get("total_requests")),
            "total_tokens": int_value(api_value.get("total_tokens")),
            "models": {},
        }
        for model_name, model_value in object_items(api_value.get("models")):
            if not isinstance(model_value, dict):
                continue
            api["models"][str(model_name)] = {
                "total_requests": int_value(model_value.get("total_requests")),
                "total_tokens": int_value(model_value.get("total_tokens")),
                "details": [comparable_detail(detail) for detail in model_value.get("details", [])],
            }
        normalized["apis"][str(api_name)] = api
    return normalized


def first_snapshot_diff(left: object, right: object, path: str = "snapshot") -> str | None:
    if type(left) is not type(right):
        return f"{path}: type {type(left).__name__} != {type(right).__name__}"
    if isinstance(left, dict):
        if set(left) != set(right):
            return f"{path}: keys {sorted(set(left) ^ set(right))} differ"
        for key in sorted(left):
            diff = first_snapshot_diff(left[key], right[key], f"{path}.{key}")
            if diff:
                return diff
        return None
    if isinstance(left, list):
        if len(left) != len(right):
            return f"{path}: len {len(left)} != {len(right)}"
        for idx, (l_item, r_item) in enumerate(zip(left, right)):
            diff = first_snapshot_diff(l_item, r_item, f"{path}[{idx}]")
            if diff:
                return diff
        return None
    if left != right:
        return f"{path}: {left!r} != {right!r}"
    return None


def check_snapshot_alignment(conn: sqlite3.Connection, snapshot_path: Path) -> None:
    if not snapshot_path.exists():
        warn(f"snapshot file missing: {snapshot_path}, skipping snapshot alignment")
        return
    raw = json.loads(snapshot_path.read_text())
    snap = normalize_snapshot_for_compare(normalize_snapshot_payload(raw))
    db_snap = normalize_snapshot_for_compare(build_snapshot_from_db(conn))
    diff = first_snapshot_diff(snap, db_snap)
    if diff:
        fail(f"snapshot mismatch: {diff}")
    ok(f"snapshot vs db: requests={snap['total_requests']} tokens={snap['total_tokens']}")


def main() -> int:
    global warning_count
    warning_count = 0
    parser = argparse.ArgumentParser(description="CPA usage 离线交叉验证；未完成检查返回 2，数据不一致返回 1")
    parser.add_argument("--db", required=True, type=Path)
    parser.add_argument("--prices", type=Path, default=None)
    parser.add_argument("--snapshot", type=Path, default=None)
    args = parser.parse_args()

    if not args.db.exists():
        fail(f"db not found: {args.db}")
    conn = sqlite3.connect(args.db.resolve().as_uri() + "?mode=ro", uri=True)
    try:
        conn.execute("BEGIN")
        check_fingerprint_roundtrip(conn)
        complete_hours = check_hour_bucket_vs_events(conn)
        check_totals_conservation(conn, complete_hours)
        check_exact_summary(conn)
        check_summary_projections(conn)
        check_oauth_null_conservation(conn, complete_hours)
        if args.prices:
            check_cost_recompute(conn, args.prices)
        if args.snapshot:
            check_snapshot_alignment(conn, args.snapshot)
    finally:
        conn.close()
    if warning_count:
        print(f"CHECKS INCOMPLETE: {warning_count} warnings")
        return 2
    print("ALL CHECKS PASSED")
    return 0


if __name__ == "__main__":
    sys.exit(main())
