package management

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	log "github.com/sirupsen/logrus"
	"gopkg.in/yaml.v3"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/usage"
)

type UsageDataDirMigration struct {
	From string `json:"from"`
	To   string `json:"to"`
}

func usageDataMigrationKey(configFilePath string) (string, error) {
	path, err := filepath.Abs(configFilePath)
	return "usage_data_dir_migration:" + filepath.Clean(path), err
}

// PendingUsageDataDirMigration returns only the migration bound to this startup destination.
func PendingUsageDataDirMigration(ctx context.Context, configFilePath, to string) (*UsageDataDirMigration, error) {
	store, err := openManagementStateStore(configFilePath)
	if err != nil || store == nil {
		return nil, err
	}
	defer func() {
		if err := store.Close(); err != nil {
			log.WithError(err).Warn("failed to close usage directory migration state")
		}
	}()
	key, err := usageDataMigrationKey(configFilePath)
	if err != nil {
		return nil, err
	}
	var raw []byte
	if err := store.db.QueryRowContext(ctx, "SELECT value FROM state_items WHERE key=?", key).Scan(&raw); errors.Is(
		err, sql.ErrNoRows,
	) {
		return nil, nil
	} else if err != nil {
		return nil, err
	}
	var pending UsageDataDirMigration
	if err := json.Unmarshal(raw, &pending); err != nil {
		return nil, err
	}
	if pending.From == "" || pending.To == "" {
		return nil, fmt.Errorf("usage directory migration has an empty path")
	}
	destination, err := filepath.Abs(to)
	if err != nil {
		return nil, err
	}
	if pending.To != filepath.Clean(destination) {
		return nil, nil
	}
	return &pending, nil
}

// CompleteUsageDataDirMigration clears the exact successfully consumed record.
func CompleteUsageDataDirMigration(ctx context.Context, configFilePath string, pending UsageDataDirMigration) error {
	store, err := openManagementStateStore(configFilePath)
	if err != nil {
		return err
	}
	if store == nil {
		return fmt.Errorf("management state unavailable")
	}
	defer func() {
		if err := store.Close(); err != nil {
			log.WithError(err).Warn("failed to close usage directory migration state")
		}
	}()
	key, err := usageDataMigrationKey(configFilePath)
	if err != nil {
		return err
	}
	raw, err := json.Marshal(pending)
	if err != nil {
		return err
	}
	result, err := store.db.ExecContext(ctx, "DELETE FROM state_items WHERE key=? AND value=?", key, raw)
	if err != nil {
		return err
	}
	count, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if count != 1 {
		return fmt.Errorf("usage directory migration changed before completion")
	}
	return nil
}

func (h *Handler) prepareUsageDataDirMigration(candidate *config.Config) (func() error, error) {
	noop := func() error { return nil }
	if h == nil || h.cfg == nil || candidate == nil || h.configFilePath == "" {
		return noop, nil
	}
	from, err := filepath.Abs(usage.ResolveDataDir(h.cfg.UsageDataDir, h.configFilePath))
	if err != nil {
		return nil, err
	}
	to, err := filepath.Abs(usage.ResolveDataDir(candidate.UsageDataDir, h.configFilePath))
	if err != nil {
		return nil, err
	}
	if from == to {
		return noop, nil
	}
	if h.stateStore == nil || h.stateStore.db == nil {
		return nil, fmt.Errorf("management state unavailable for usage directory migration")
	}
	key, err := usageDataMigrationKey(h.configFilePath)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var previous []byte
	err = h.stateStore.db.QueryRowContext(ctx, "SELECT value FROM state_items WHERE key=?", key).Scan(&previous)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}
	if len(previous) > 0 {
		var pending UsageDataDirMigration
		if err := json.Unmarshal(previous, &pending); err != nil {
			return nil, err
		}
		if pending.To == from {
			from = pending.From
		}
	}
	raw, err := json.Marshal(UsageDataDirMigration{From: from, To: to})
	if err != nil {
		return nil, err
	}
	var result sql.Result
	if len(previous) == 0 {
		result, err = h.stateStore.db.ExecContext(
			ctx, "INSERT INTO state_items(key,value,updated_at) VALUES(?,?,?) ON CONFLICT(key) DO NOTHING", key, raw,
			time.Now().UTC().Format(time.RFC3339Nano),
		)
	} else {
		result, err = h.stateStore.db.ExecContext(
			ctx, "UPDATE state_items SET value=?,updated_at=? WHERE key=? AND value=?", raw,
			time.Now().UTC().Format(time.RFC3339Nano), key, previous,
		)
	}
	if err != nil {
		return nil, err
	}
	count, err := result.RowsAffected()
	if err != nil {
		return nil, err
	}
	if count != 1 {
		return nil, fmt.Errorf("usage directory migration changed during configuration save")
	}
	return func() error {
		// A directory-sync error can follow a successful atomic rename.
		if data, errRead := os.ReadFile(h.configFilePath); errRead == nil {
			var current struct {
				UsageDataDir string `yaml:"usage-data-dir"`
			}
			if yaml.Unmarshal(data, &current) == nil {
				publishedTo, err := filepath.Abs(usage.ResolveDataDir(current.UsageDataDir, h.configFilePath))
				if err == nil && publishedTo == to {
					return nil
				}
			}
		} else if !errors.Is(errRead, os.ErrNotExist) {
			return fmt.Errorf("inspect configuration before migration rollback: %w", errRead)
		}
		rollbackCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		var result sql.Result
		var err error
		if len(previous) == 0 {
			result, err = h.stateStore.db.ExecContext(
				rollbackCtx, "DELETE FROM state_items WHERE key=? AND value=?", key, raw,
			)
		} else {
			result, err = h.stateStore.db.ExecContext(
				rollbackCtx, "UPDATE state_items SET value=? WHERE key=? AND value=?", previous, key, raw,
			)
		}
		if err != nil {
			return err
		}
		count, err := result.RowsAffected()
		if err != nil {
			return err
		}
		if count != 1 {
			return fmt.Errorf("usage directory migration changed during configuration rollback")
		}
		return nil
	}, nil
}
