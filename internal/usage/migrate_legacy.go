// Last compiled: 2026-04-27
// Author: pyro

package usage

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	log "github.com/sirupsen/logrus"
)

// migrateMetaKey marks the database as having absorbed every legacy JSON file
// under baseDir. Once present the migration becomes a no-op on every restart.
const migrateMetaKey = "migrated_from_json"

// MigrationStatus reports the persisted migration marker. From=="" means no
// migration has been recorded yet.
type MigrationStatus struct {
	From string
	At   time.Time
}

// CheckMigrationStatus reads the meta row that flags a completed JSON import.
func CheckMigrationStatus(ctx context.Context, store *Store) (MigrationStatus, error) {
	var status MigrationStatus
	if store == nil || store.db == nil {
		return status, errors.New("usage: nil store")
	}
	var raw string
	err := store.db.QueryRowContext(ctx, "SELECT value FROM meta WHERE key = ?", migrateMetaKey).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return status, nil
	}
	if err != nil {
		return status, fmt.Errorf("query migration meta: %w", err)
	}
	parts := strings.SplitN(raw, "@", 2)
	status.From = parts[0]
	if len(parts) == 2 {
		if t, err := time.Parse(time.RFC3339, parts[1]); err == nil {
			status.At = t
		}
	}
	return status, nil
}

// MigrateLegacyToSQLite imports every JSON file the v1/v2 era left behind in
// baseDir into the store. Old JSON files are left in place — events.db is the
// authoritative data source after migration, and meta.migrated_from_json
// guarantees subsequent restarts skip the rescan.
//
// Idempotent: a successful run records meta.migrated_from_json, and subsequent
// invocations short-circuit immediately without scanning disk.
//
// On parse failure the migration aborts with the failing file's path attached,
// the meta row is NOT written, and the caller can decide whether to retry or
// surface the failure.
func MigrateLegacyToSQLite(ctx context.Context, store *Store, priceFn PriceFunc, baseDir string) error {
	if store == nil || store.db == nil {
		return errors.New("usage: nil store")
	}
	if baseDir == "" {
		return errors.New("usage: empty base dir")
	}

	status, err := CheckMigrationStatus(ctx, store)
	if err != nil {
		return fmt.Errorf("check migration status: %w", err)
	}
	if status.From != "" {
		log.Infof(
			"usage: migration already done (%s @ %s), skipping",
			status.From, status.At.Format(time.RFC3339),
		)
		return nil
	}

	paths, err := collectLegacyPaths(baseDir)
	if err != nil {
		return fmt.Errorf("scan legacy files: %w", err)
	}
	if len(paths) == 0 {
		log.Infof("usage: no legacy JSON files found under %s", baseDir)
	} else {
		log.Infof("usage: migrating %d legacy files from %s", len(paths), baseDir)
	}

	var totalAdded, totalSkipped int64
	for _, p := range paths {
		result, err := ImportFile(ctx, store, priceFn, p)
		if err != nil {
			return fmt.Errorf("import %s: %w", p, err)
		}
		totalAdded += result.Added
		totalSkipped += result.Skipped
		log.Infof(
			"usage: imported %s (format=%s, added=%d, skipped=%d)",
			filepath.Base(p), result.Format, result.Added, result.Skipped,
		)
	}

	// Record the marker even when zero files were found — that way a fresh
	// install never re-scans baseDir on every restart.
	metaValue := fmt.Sprintf("v1+v2-json@%s", time.Now().UTC().Format(time.RFC3339))
	if _, err := store.db.ExecContext(
		ctx,
		"INSERT INTO meta(key, value) VALUES (?, ?) "+
			"ON CONFLICT(key) DO UPDATE SET value = excluded.value",
		migrateMetaKey, metaValue,
	); err != nil {
		return fmt.Errorf("write migration meta: %w", err)
	}

	// Truncate WAL so the on-disk db is compact right after the big import.
	if _, err := store.db.ExecContext(ctx, "PRAGMA wal_checkpoint(TRUNCATE)"); err != nil {
		log.Warnf("usage: wal_checkpoint failed: %v", err)
	}

	log.Infof(
		"usage: migration complete (files=%d, added=%d, skipped=%d)",
		len(paths), totalAdded, totalSkipped,
	)
	return nil
}

var (
	archiveFileRegexp = regexp.MustCompile(`^usage-archive-\d{4}-\d{2}\.json$`)
	monthDirRegexp    = regexp.MustCompile(`^\d{4}-\d{2}$`)
)

// collectLegacyPaths returns every JSON file we know how to migrate, in an
// order that mirrors the natural arrival sequence (oldest archives first,
// today.json last) so SQLite caches warm naturally.
func collectLegacyPaths(baseDir string) ([]string, error) {
	entries, err := os.ReadDir(baseDir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("read base dir: %w", err)
	}

	var paths []string

	// 1) v1 usage-statistics.json
	if p := filepath.Join(baseDir, "usage-statistics.json"); fileExists(p) {
		paths = append(paths, p)
	}

	// 2) v1 usage-archive-YYYY-MM.json
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		if archiveFileRegexp.MatchString(entry.Name()) {
			paths = append(paths, filepath.Join(baseDir, entry.Name()))
		}
	}

	// 3) v2 detail/YYYY-MM/YYYY-MM-DD.json
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		if !monthDirRegexp.MatchString(entry.Name()) {
			continue
		}
		monthDir := filepath.Join(baseDir, entry.Name())
		dayEntries, err := os.ReadDir(monthDir)
		if err != nil {
			log.Warnf("usage: read %s: %v", monthDir, err)
			continue
		}
		for _, dayEntry := range dayEntries {
			if dayEntry.IsDir() {
				continue
			}
			name := dayEntry.Name()
			if strings.HasSuffix(name, ".json") {
				paths = append(paths, filepath.Join(monthDir, name))
			}
		}
	}

	// 4) v2 today.json
	if p := filepath.Join(baseDir, "today.json"); fileExists(p) {
		paths = append(paths, p)
	}

	return paths, nil
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}
