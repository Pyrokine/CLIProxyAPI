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

## Capacity model

Use these dimensions when estimating an import or a long retention period:

- `N`: accepted event rows after duplicate fingerprints are ignored
- `H`: distinct UTC-hour buckets by model, credential, API key, and provider
- `D`: distinct UTC-day buckets by model, credential, API key, and provider
- dimension rows for distinct models, credentials, API keys, sources, and providers

```text
SQLite rows = N events + H hour_bucket + D day_bucket + dimension rows
H <= min(N, hours * models * credentials * api_keys * providers)
D <= min(N, days  * models * credentials * api_keys * providers)
```

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

## Operating checklist

- Include `events.db`, WAL, and SHM when measuring storage
- Estimate `N`, `H`, and `D` before large imports
- Keep portable import and export files within the supported limit
- Reserve at least 20% working headroom, then adjust from measurements
- Monitor `/usage/db-size` warning and capped state
- Configure retention or external archival before storage is exhausted
- Never place API keys, management credentials, or backup keys in committed files or diagnostic logs
- Verify imported counts and summaries before removing the previous copy
