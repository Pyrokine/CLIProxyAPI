# Usage SQLite capacity

English | [中文](usage-sqlite-capacity_CN.md)

## Scope

CLIProxyAPI stores detailed usage data and aggregate buckets in SQLite. This guide explains capacity planning,
retention, backup, and migration for operators

The formulas are planning aids, not disk-size or performance guarantees. Check the configuration example and Management
API behavior included with the release you run

## Physical files

The default SQLite store uses three files under `usage-data/`:

- `events.db`: main database
- `events.db-wal`: write-ahead log
- `events.db-shm`: shared-memory sidecar

The Management API endpoint `/usage/db-size` reports their combined size. Capacity checks must include all three files,
not only `events.db`

## Structured event reads

Canonical events remain in schema 9, including their original `metadata` and immutable `pricing_receipt` values.
The `event_fields` table stores one lightweight row per retained event for SQL filters, search, ordering, actual speed,
and compact-list fields. Both tables change atomically through SQLite triggers during live writes, imports, repricing,
and deletion. A cost-only update preserves the saved observation fields

The derived layout has its own version, `event_fields_version=1`, while the canonical format declaration stays
`usage=1:9:9`. A compatible schema-9 binary can continue using the current database; the persistent triggers also
maintain
structured rows for writes made by that binary. The existing Python SQLite 3.31.1 administration readers can load the
schema

Startup creates the lightweight table, indexes, and triggers without rewriting historical events or rebuilding their
summaries. Background upgrade transactions convert at most 128 historical rows and commit their cursor together with
the rows. New writes continue while the history is upgraded. Until completion, queries project the requested fields
from canonical events in one read snapshot, with exact totals and unchanged search semantics. Completed conversion
switches reads to the lightweight table

The authenticated read-only endpoint `GET /v0/management/usage/storage-upgrade` returns `status`
(`idle`, `running`, `complete`, or `error`), `running`, and exact `processed_events`, `total_events`, and `percent`.
Counts include current retained events, including live writes and deletions, from one database snapshot.
The `started_at`, `updated_at`, `completed_at`, and `error` values saved in `meta` remain available after restart.
If saving an error fails, the active process still reports it and logs the failed save.
Times that an older binary never recorded are omitted. Cost recalculation remains a separate task;
`GET /v0/management/usage/recalculate-costs` returns its recorded `started_at`, `updated_at`, and optional
`completed_at`

At each pending history page, the upgrade checks the current combined database/WAL/SHM size and reserves
1% of the configured size limit, up to 64 MiB, for live writes. Reaching that reserve or failing to measure capacity
pauses the upgrade with an error, preserving the committed cursor and rows. Resolve the condition and restart
to resume. Live traffic can continue consuming the remaining reserve; monitor `/usage/db-size` while the upgrade is
paused

Inspect the committed cursor in a read-only SQLite connection:

```sql
SELECT key, value FROM meta
WHERE key IN ('schema_version', 'event_fields_version', 'event_fields_cursor');
```

`event_fields_cursor=complete` marks completed history conversion. An integer cursor is the last committed event ID;
an empty cursor means conversion has not committed its first page. Shutdown cancels the upgrade before draining queued
usage writes. Restart resumes from the committed cursor. An upgrade error keeps original events and completed pages,
logs the error, and leaves unconverted rows readable; resolve the reported condition and restart the same database

Keep the canonical backup when upgrading or reverting the binary. Reverting to a compatible schema-9 binary uses the
current database, so records accepted after upgrade remain available. Portable backup v3/v4 and sealed event exports
keep their existing contracts

## Capacity model

Use these dimensions when estimating an import or a long retention period:

- `N`: retained event rows after duplicate fingerprints are ignored, plus one structured `event_fields` row per event
- `H`: distinct UTC-hour buckets by model, credential, API key, and provider
- `D`: distinct UTC-day buckets by model, credential, API key, and provider
- `S`: exact summary rows at `1 ns`, 10 min, and 1 h, including the auth-index dimension
- `P`: query projection rows for all/model/credential/API-key groupings at 1 h, 1 d, and fixed 30 d UTC windows
- dimension rows for distinct models, credentials, API keys, auth indexes, sources, and providers

```text
SQLite rows = N events + N event_fields + H hour_bucket + D day_bucket + S summary_bucket + P summary_projection + dimension rows
S <= 3 * N
```

Historical hour/day buckets can outlive retained events. Their query projections follow the source buckets in the same
write, import, repricing, and cleanup transactions. Include both summary tables and their indexes in the storage
estimate;
measure `/usage/db-size` after startup backfill

For JSON migration planning, use the parsed request-detail count as the upper bound for `N` unless duplicate
fingerprints are known in advance. JSON byte size is not a reliable row-count multiplier because supported input formats
have different shapes

Reserve space for WAL growth, page changes, and indexes:

```text
max-db-size-mb >= ceil(estimated_sqlite_bytes * 1.2 / 1048576)
```

The 20% factor is a starting point. Measure `/usage/db-size` during a representative import before selecting a
production limit

## Import and export limits

Portable usage imports and exports have a 100 MiB limit in current builds. Oversized input is rejected rather than
truncated

Use the available time, model, API key, or credential filters to split large exports into smaller ranges. Keep enough
free space for the database, WAL, temporary import work, and the exported archive

## Size cap and retention

`usage-retention.max-db-size-mb` applies to the combined `events.db`, WAL, and SHM size. Set it to `0` only when another
capacity control is in place

When the configured cap is reached, new detailed usage writes and imports are refused, and `/usage/db-size` reports
warning and capped state. The database is not split automatically across multiple SQLite files

Use `usage-retention.days` to remove old request details, raise the size cap after measuring available storage, or
export and archive older ranges outside CLIProxyAPI. Aggregate counters can have different retention behavior from
request details, so verify the release configuration before relying on deletion for a specific storage target

## Backup and migration

Do not copy only `events.db` while the service is writing. A live database may have committed data in the WAL file

Use one of these methods:

1. Use the Management API export and import flow for portable backups
2. Stop writers before copying the complete SQLite file set
3. Use SQLite online backup tooling that is compatible with the deployed environment

Portable backups are authenticated and encrypted. Import normally uses the same management credential as export. When
moving data between instances with different management credentials, provide the same dedicated `X-Usage-Backup-Key`
value for export and import. Transfer it separately from the archive, then remove it from shell history and logs

Portable backup v4 preserves immutable request-time pricing receipts. The importer remains compatible with v3 archives,
whose historical events have no request-time receipt. Import and historical repricing may update the current-catalog
derived amount, but cannot alter a stored receipt

After a migration, check the import result, `/usage/db-size`, the configured cap, and a representative usage summary
before deleting the source backup

## Change the data directory

Save `usage-data-dir` through the Management API, then restart the process to apply it. Saving a change records the
source
and destination in the existing management state store. If several directory changes are saved before restarting, the
migration keeps the original source and the final destination

Startup migrates the data before opening the destination database. The migration record is cleared after SQLite becomes
ready. A migration or database-open failure leaves that record available for retry and emits a warning; usage is
collected
in memory for that process lifetime. Keep the previous files until the destination data has been checked

## Check an offline database

Run the cross-check from the repository root against a stopped database or a consistent SQLite backup:

```bash
python3 -I scripts/cross_check.py --db /path/to/events.db --prices /path/to/model-prices.json
```

The script opens one read-only SQLite snapshot. It checks fingerprints, hourly model/credential/API-key/provider
allocation, OAuth requests with a NULL API key, and exact summaries at `1 ns`, 10 min, and 1 h across five identity
dimensions
and 19 metrics. Query projections are compared independently with their source hour/day buckets. A projection marked
invalid uses the original query path; the checker verifies its source-member count and reports its cached metrics as
unverified. `--prices` enables cost and receipt checks and accepts a flat price map or the model catalog with its
`prices`
field. `--snapshot /path/to/snapshot.json` adds comparison with a legacy JSON usage snapshot

A stored request-time receipt keeps its captured price when the current catalog changes. The cost check accepts a valid
immutable receipt amount or an amount recomputed from the supplied catalog. When an older derived amount matches neither
and its price snapshot is unavailable, that cost remains unverified

| Exit code | Output              | Meaning                                                                                      |
|-----------|---------------------|----------------------------------------------------------------------------------------------|
| `0`       | `ALL CHECKS PASSED` | All requested checks completed without warnings                                              |
| `1`       | `FAIL:`             | A data inconsistency was detected                                                            |
| `2`       | `CHECKS INCOMPLETE` | Historical details, exact summaries, or reference prices were insufficient to finish a check |

Historical hour/day aggregates can outlive request details. Exact summaries are backfilled from retained events. A query
that needs unavailable historical precision returns HTTP `409` with `summary_precision_unavailable`; select a supported
whole-bucket range or retain the corresponding details. The offline checker reports missing precision as incomplete

## Operating checklist

- Include `events.db`, WAL, and SHM when measuring storage
- Estimate `N`, `H`, and `D` before large imports
- Keep portable import and export files within the supported limit
- Reserve at least 20% working headroom, then adjust from measurements
- Monitor `/usage/db-size` warning and capped state
- Configure retention or external archival before storage is exhausted
- Never place API keys, management credentials, or backup keys in committed files or diagnostic logs
- Verify imported counts and summaries before removing the previous copy
