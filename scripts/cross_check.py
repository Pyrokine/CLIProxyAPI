#!/usr/bin/env python3
# CPA usage 数据离线交叉验证(R-552)
from __future__ import annotations

import argparse
import datetime as dt
import hashlib
import json
import sqlite3
import sys
from pathlib import Path
from typing import Optional

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


def fingerprint(
        d: dict
) -> str:
    ts = d["timestamp"]
    if not ts.endswith("Z") and "+" not in ts:
        ts = ts + "Z"
    common = [
        d.get("api_key", "") or "",
        d.get("model", "") or "",
        ts,
        d.get("source", "") or "",
        d.get("auth_index", "") or "",
    ]
    if d.get("provider"):
        fields = common + [d["provider"]]
    else:
        fields = common
    fields += [
        "true" if d.get("failed") else "false",
        str(int(d.get("input_tokens", 0) or 0)),
        str(int(d.get("output_tokens", 0) or 0)),
        str(int(d.get("reasoning_tokens", 0) or 0)),
        str(int(d.get("cached_tokens", 0) or 0)),
        str(int(d.get("total_tokens", 0) or 0)),
    ]
    return hashlib.sha1("|".join(fields).encode()).hexdigest()


def fail(
        msg: str
) -> None:
    print(f"FAIL: {msg}")
    sys.exit(1)


def warn(
        msg: str
) -> None:
    print(f"WARN: {msg}")


def ok(
        msg: str
) -> None:
    print(f"OK:   {msg}")


def ns_to_rfc3339_nano(
        ts_ns: int
) -> str:
    sec, ns = divmod(int(ts_ns), 1_000_000_000)
    base = dt.datetime.fromtimestamp(sec, dt.timezone.utc).strftime("%Y-%m-%dT%H:%M:%S")
    if ns == 0:
        return base + "Z"
    frac = f"{ns:09d}".rstrip("0")
    return f"{base}.{frac}Z"


def int_value(
        value: object
) -> int:
    if value is None:
        return 0
    return int(value)


def totals_query(
        table: str
) -> str:
    return (
        "SELECT COALESCE(SUM(requests),0), COALESCE(SUM(success),0), COALESCE(SUM(failure),0), "
        "COALESCE(SUM(input_tokens),0), COALESCE(SUM(output_tokens),0), "
        "COALESCE(SUM(reasoning_tokens),0), COALESCE(SUM(cached_tokens),0), "
        f"COALESCE(SUM(total_tokens),0), COALESCE(SUM(cost_micro),0) FROM {table}"
    )


def fetch_totals(
        conn: sqlite3.Connection,
        table: str
) -> tuple[int, ...]:
    row = conn.execute(totals_query(table)).fetchone()
    return tuple(int_value(value) for value in row)


def fetch_grouped_totals(
        conn: sqlite3.Connection,
        table: str,
        dimension: str
) -> tuple[int, ...]:
    totals = [0] * len(SUMMARY_FIELDS)
    for row in conn.execute(totals_query(table) + f" GROUP BY {dimension}"):
        for index, value in enumerate(row):
            totals[index] += int_value(value)
    return tuple(totals)


def fetch_event_totals(
        conn: sqlite3.Connection
) -> tuple[int, ...]:
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
        right: tuple[int, ...]
) -> None:
    if left != right:
        detail = ", ".join(f"{field}: {l} != {r}" for field, l, r in zip(SUMMARY_FIELDS, left, right) if l != r)
        fail(f"{label} mismatch: {detail}")


def check_fingerprint_roundtrip(
        conn: sqlite3.Connection
) -> None:
    rows = conn.execute(
        """
        SELECT events.fingerprint,
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
        (fp_db, api_key, model, ts_ns, source, auth_index, provider, failed,
         it, ot, rt, ct, tt) = row
        ts_iso = ns_to_rfc3339_nano(ts_ns)
        d = {
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
        conn: sqlite3.Connection
) -> None:
    total = fetch_totals(conn, "hour_bucket")
    if total[0] == 0:
        warn("hour_bucket empty, skipping conservation check")
        return
    for dimension in ("model_id", "credential_id", "api_key_id"):
        grouped = fetch_grouped_totals(conn, "hour_bucket", dimension)
        compare_totals(f"{dimension} conservation", grouped, total)
    ok(f"totals conservation: requests={total[0]} tokens={total[7]} cost_micro={total[8]}")


def check_hour_bucket_vs_events(
        conn: sqlite3.Connection
) -> None:
    ev = fetch_event_totals(conn)
    hb = fetch_totals(conn, "hour_bucket")
    compare_totals("hour_bucket vs events", hb, ev)
    ok(f"hour_bucket vs events: count={ev[0]} tokens={ev[7]} cost_micro={ev[8]}")


def check_cost_recompute(
        conn: sqlite3.Connection,
        prices_path: Path
) -> None:
    if not prices_path.exists():
        warn(f"prices file missing: {prices_path}, skipping cost recompute")
        return
    prices = json.loads(prices_path.read_text())
    rows = conn.execute(
        """
        SELECT m.name, e.input_tokens, e.output_tokens, e.cached_tokens, e.cost_micro
        FROM events e
                 JOIN dim_model m ON m.id = e.model_id
        """
    ).fetchall()
    diff = 0
    sample = []
    for model, it, ot, ct, stored in rows:
        p = prices.get(model)
        if not p:
            continue
        expected = int(it * p.get("prompt", 0) + ot * p.get("completion", 0) + ct * p.get("cache", 0))
        if abs(expected - stored) > 1:
            diff += 1
            if len(sample) < 3:
                sample.append((model, expected, stored))
    if diff:
        for s in sample:
            print(f"  model={s[0]} expected={s[1]} stored={s[2]}")
        fail(f"cost_micro recompute mismatches: {diff}")
    ok(f"cost_micro recompute: 0 mismatches over {len(rows)} priced rows")


def check_oauth_null_conservation(
        conn: sqlite3.Connection
) -> None:
    ev_null = conn.execute("SELECT COUNT(*) FROM events WHERE api_key_id IS NULL").fetchone()[0]
    hb_null = conn.execute("SELECT COALESCE(SUM(requests),0) FROM hour_bucket WHERE api_key_id IS NULL").fetchone()[0]
    if ev_null != hb_null:
        fail(f"OAuth NULL conservation: events={ev_null} hour_bucket={hb_null}")
    ok(f"OAuth NULL conservation: {ev_null}={hb_null}")


def normalize_snapshot_payload(
        raw: object
) -> dict:
    if not isinstance(raw, dict):
        fail("snapshot root must be an object")
    usage = raw.get("usage")
    if isinstance(usage, dict):
        return usage
    return raw


def build_snapshot_from_db(
        conn: sqlite3.Connection
) -> dict:
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


def object_items(
        value: object
):
    return value.items() if isinstance(value, dict) else ()


def int_map(
        value: object
) -> dict:
    return {str(k): int_value(v) for k, v in object_items(value)}


def comparable_detail(
        detail: object
) -> dict:
    if not isinstance(detail, dict):
        return {}
    tokens = detail.get("tokens") if isinstance(detail.get("tokens"), dict) else {}
    return {
        "timestamp": str(detail.get("timestamp", "")),
        "source": str(detail.get("source") or ""),
        "auth_index": str(detail.get("auth_index") or ""),
        "tokens": {key: int_value(tokens.get(key)) for key in (
            "input_tokens",
            "output_tokens",
            "reasoning_tokens",
            "cached_tokens",
            "total_tokens",
        )},
        "failed": bool(detail.get("failed", False)),
    }


def normalize_snapshot_for_compare(
        snapshot: dict
) -> dict:
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


def first_snapshot_diff(
        left: object,
        right: object,
        path: str = "snapshot"
) -> Optional[str]:
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


def check_snapshot_alignment(
        conn: sqlite3.Connection,
        snapshot_path: Path
) -> None:
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
    parser = argparse.ArgumentParser(description="CPA usage 离线交叉验证")
    parser.add_argument("--db", required=True, type=Path)
    parser.add_argument("--prices", type=Path, default=None)
    parser.add_argument("--snapshot", type=Path, default=None)
    args = parser.parse_args()

    if not args.db.exists():
        fail(f"db not found: {args.db}")
    conn = sqlite3.connect(f"file:{args.db}?mode=ro", uri=True)
    try:
        check_fingerprint_roundtrip(conn)
        check_totals_conservation(conn)
        check_hour_bucket_vs_events(conn)
        check_oauth_null_conservation(conn)
        if args.prices:
            check_cost_recompute(conn, args.prices)
        if args.snapshot:
            check_snapshot_alignment(conn, args.snapshot)
    finally:
        conn.close()
    print("ALL CHECKS PASSED")
    return 0


if __name__ == "__main__":
    sys.exit(main())
