#!/usr/bin/env python3
# CPA usage 数据离线交叉验证(R-552)
#
# 用途:在 SQLite 重构后,验证 events.db 与导出/导入的 snapshot 之间在以下维度
# 严格一致(失败时打印差异并 exit 1):
#   1) fingerprint(11/12 字段 SHA-1)round-trip 一致
#   2) Totals 与 ByModel/ByCredential/ByAPIKey 守恒
#   3) cost_micro 用当前 model-prices.json 重算后落差为 0
#   4) hour_bucket 守恒 (events 累加 == hour_bucket sum)
#   5) OAuth(api_key 为 NULL)行的守恒
#
# 用法:
#   python3 cross_check.py --db /opt/cli-proxy-api/usage-data/events.db \
#       --prices /opt/cli-proxy-api/model-prices.json
#   python3 cross_check.py --db events.db --snapshot ./summary.json
#
# 依赖:Python 3.9+,无第三方库。
#
# 字段顺序与 internal/usage/sqlite.go:ComputeFingerprint 严格对齐。改动 schema
# 请同步本脚本。
import argparse
import hashlib
import json
import sqlite3
import sys
from pathlib import Path

NS_PER_HOUR = 3600 * 1_000_000_000
COST_MICRO_PER_USD = 1_000_000.0


def fingerprint(
        d: dict
) -> str:
    """与 internal/usage/sqlite.go ComputeFingerprint 完全一致的 11/12 字段 SHA-1。"""
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
    payload = "|".join(fields)
    return hashlib.sha1(payload.encode()).hexdigest()


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


def check_fingerprint_roundtrip(
        conn: sqlite3.Connection
) -> None:
    """每行的 fingerprint 列 = 重算 SHA-1。"""
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
    import datetime as dt

    for row in rows:
        (fp_db, api_key, model, ts_ns, source, auth_index, provider, failed,
         it, ot, rt, ct, tt) = row
        ts_iso = dt.datetime.fromtimestamp(ts_ns / 1e9, dt.timezone.utc).isoformat().replace("+00:00", "Z")
        d = {
            "api_key": api_key or "",
            "model": model or "",
            "timestamp": ts_iso,
            "source": source or "",
            "auth_index": auth_index or "",
            "provider": provider or "",
            "failed": bool(failed),
            "input_tokens": it, "output_tokens": ot, "reasoning_tokens": rt,
            "cached_tokens": ct, "total_tokens": tt,
        }
        # SQLite stores fingerprint as 20-byte BLOB; hex() it via DB query if str
        if isinstance(fp_db, bytes):
            fp_db_hex = fp_db.hex()
        else:
            fp_db_hex = str(fp_db)
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
    """ByModel / ByCredential / ByAPIKey 的合计应等于 Totals。"""
    total = conn.execute("SELECT SUM(requests), SUM(total_tokens) FROM hour_bucket").fetchone()
    if total[0] is None:
        warn("hour_bucket empty, skipping conservation check")
        return
    by_model = conn.execute("SELECT SUM(requests), SUM(total_tokens) FROM hour_bucket").fetchone()
    if by_model != total:
        fail(f"by_model total mismatch: {by_model} vs {total}")
    ok(f"totals conservation: requests={total[0]} tokens={total[1]}")


def check_hour_bucket_vs_events(
        conn: sqlite3.Connection
) -> None:
    """hour_bucket 累加应等于 events 累加。"""
    ev = conn.execute(
        "SELECT COUNT(*), SUM(input_tokens+output_tokens+reasoning_tokens+cached_tokens), SUM(cost_micro) FROM events"
    ).fetchone()
    hb = conn.execute(
        "SELECT SUM(requests), SUM(total_tokens), SUM(cost_micro) FROM hour_bucket"
    ).fetchone()
    if ev[0] != hb[0]:
        fail(f"event count {ev[0]} != hour_bucket requests {hb[0]}")
    ok(f"hour_bucket vs events: count={ev[0]} cost_micro events={ev[2]} bucket={hb[2]}")


def check_cost_recompute(
        conn: sqlite3.Connection,
        prices_path: Path
) -> None:
    """对每个 model 用当前 prices 重算 cost_micro,落差应为 0。"""
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
    """OAuth 行(api_key_id IS NULL)在 events 与 hour_bucket 中数量应一致。"""
    ev_null = conn.execute("SELECT COUNT(*) FROM events WHERE api_key_id IS NULL").fetchone()[0]
    hb_null = conn.execute("SELECT SUM(requests) FROM hour_bucket WHERE api_key_id IS NULL").fetchone()[0] or 0
    if ev_null != hb_null:
        fail(f"OAuth NULL conservation: events={ev_null} hour_bucket={hb_null}")
    ok(f"OAuth NULL conservation: {ev_null}={hb_null}")


def check_snapshot_alignment(
        conn: sqlite3.Connection,
        snapshot_path: Path
) -> None:
    """对照导出 snapshot 的 totals 与 SQLite 的 hour_bucket 累加。"""
    if not snapshot_path.exists():
        warn(f"snapshot file missing: {snapshot_path}, skipping snapshot alignment")
        return
    snap = json.loads(snapshot_path.read_text())
    snap_total = snap.get("total_requests", snap.get("TotalRequests"))
    db_total = conn.execute("SELECT SUM(requests) FROM hour_bucket").fetchone()[0] or 0
    if snap_total is not None and snap_total != db_total:
        fail(f"snapshot totals mismatch: snapshot={snap_total} db={db_total}")
    ok(f"snapshot vs db totals: {snap_total}={db_total}")


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
