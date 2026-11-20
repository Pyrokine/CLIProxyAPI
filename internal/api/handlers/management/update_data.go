package management

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/store"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/usage"
)

func parseUpdateDataFormats(raw string) (map[string][3]int, error) {
	formats := make(map[string][3]int, 3)
	for component := range strings.SplitSeq(raw, ";") {
		name, values, ok := strings.Cut(component, "=")
		if !ok || (name != "management" && name != "usage" && name != "postgres") {
			return nil, fmt.Errorf("executable data format declaration is invalid")
		}
		if _, duplicate := formats[name]; duplicate {
			return nil, fmt.Errorf("executable repeats the %s data format", name)
		}
		parts := strings.Split(values, ":")
		if len(parts) != 3 {
			return nil, fmt.Errorf("executable %s data format range is invalid", name)
		}
		var limits [3]int
		for index, value := range parts {
			number, err := strconv.Atoi(value)
			if err != nil || number <= 0 {
				return nil, fmt.Errorf("executable %s data format number is invalid", name)
			}
			limits[index] = number
		}
		if limits[0] > limits[1] || limits[2] < limits[0] || limits[2] > limits[1] {
			return nil, fmt.Errorf("executable %s data format range is inconsistent", name)
		}
		formats[name] = limits
	}
	return formats, nil
}

func validateUpdateDataFormats(session updateGuardSession) error {
	previous, err := parseUpdateDataFormats(session.PreviousIdentity.DataFormats)
	if err != nil {
		return fmt.Errorf("read rollback data compatibility: %w", err)
	}
	target, err := parseUpdateDataFormats(session.TargetIdentity.DataFormats)
	if err != nil {
		return fmt.Errorf("read target data compatibility: %w", err)
	}
	if len(session.DataFormats) == 0 || session.DataFormats["management"] <= 0 {
		return fmt.Errorf("update session has no confirmed database formats")
	}
	for name, version := range session.DataFormats {
		old, oldOK := previous[name]
		next, nextOK := target[name]
		if !oldOK || !nextOK || version <= 0 || old[2] != version || version < next[0] || version > next[1] {
			return fmt.Errorf("target or rollback executable cannot read the current %s database format", name)
		}
		if next[2] < old[0] || next[2] > old[1] {
			return fmt.Errorf(
				"rollback executable cannot read the target %s database format; explicit migration is required", name,
			)
		}
		if next[2] != version {
			return fmt.Errorf(
				"automatic update would change the %s database format; explicit migration is required", name,
			)
		}
	}
	return nil
}

// updateDataReadiness checks the same persisted data used after the service restarts.
func (h *Handler) updateDataReadiness(ctx context.Context) (map[string]int, error) {
	ctx, cancel := context.WithTimeout(ctx, updateGuardHTTPTimeout)
	defer cancel()
	if h == nil || h.stateStore == nil || h.stateStore.db == nil {
		return nil, fmt.Errorf("management data is unavailable")
	}
	var version string
	if err := h.stateStore.db.QueryRowContext(
		ctx, "SELECT value FROM meta WHERE key='schema_version'",
	).Scan(&version); err != nil {
		return nil, fmt.Errorf("read management data format: %w", err)
	}
	if version != managementStateSchemaVersion {
		return nil, fmt.Errorf("management data format is incompatible")
	}
	rows, err := h.stateStore.db.QueryContext(
		ctx,
		"SELECT entry_key, auth_index, credential_revision, generation, state_revision, tombstone, status, "+
			"data FROM quota_entries_v2 LIMIT 1",
	)
	if err != nil {
		return nil, fmt.Errorf("read management quota data: %w", err)
	}
	for rows.Next() {
		var key, authIndex, revision, status string
		var generation, stateRevision, tombstone int64
		var data []byte
		if err = rows.Scan(
			&key, &authIndex, &revision, &generation, &stateRevision, &tombstone, &status, &data,
		); err != nil {
			_ = rows.Close()
			return nil, fmt.Errorf("decode management quota data: %w", err)
		}
	}
	err = rows.Err()
	closeErr := rows.Close()
	if err != nil || closeErr != nil {
		return nil, fmt.Errorf("finish management quota query: %w", errors.Join(err, closeErr))
	}
	managementVersion, err := strconv.Atoi(version)
	if err != nil {
		return nil, fmt.Errorf("decode management data format: %w", err)
	}
	formats := map[string]int{"management": managementVersion}
	if h.usagePersister != nil && h.usagePersister.IsReady() {
		version, err := h.usagePersister.SchemaVersion(ctx)
		if err != nil {
			return nil, fmt.Errorf("read usage data format: %w", err)
		}
		if version != usage.CurrentSchemaVersion {
			return nil, fmt.Errorf("usage data format is incompatible")
		}
		now := time.Now()
		if _, _, err := h.usagePersister.QueryEventsContext(
			ctx, now.Add(-time.Minute), now, usage.EventFilters{}, 1, 1, "timestamp", true,
		); err != nil {
			return nil, fmt.Errorf("read usage event data: %w", err)
		}
		usageVersion, err := strconv.Atoi(version)
		if err != nil {
			return nil, fmt.Errorf("decode usage data format: %w", err)
		}
		formats["usage"] = usageVersion
	} else {
		cfg := h.configSnapshot()
		dataDir := ""
		if cfg != nil {
			dataDir = usage.ResolveDataDir(cfg.UsageDataDir, h.configFilePath)
		}
		if dataDir != "" {
			if _, err := os.Stat(filepath.Join(dataDir, "events.db")); err == nil {
				return nil, fmt.Errorf("existing usage data is unavailable")
			} else if !os.IsNotExist(err) {
				return nil, fmt.Errorf("inspect usage data: %w", err)
			}
		}
	}
	if postgres, ok := h.tokenStore.(*store.PostgresStore); ok {
		if err := postgres.CheckUpdateReadiness(ctx); err != nil {
			return nil, err
		}
		formats["postgres"] = store.PostgresDataFormatVersion
	}
	return formats, nil
}
