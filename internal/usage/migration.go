// Last compiled: 2026-03-10
// Author: pyro

package usage

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	log "github.com/sirupsen/logrus"
)

// MigrateV1ToV2 detects and migrates legacy usage-statistics.json (v1 format) to the new
// directory-based v2 format (summary.json + today.json + per-day archives).
//
// It also processes any legacy usage-archive-YYYY-MM.json files.
//
// After successful migration, the old file is renamed to .bak.
//
// Parameters:
//   - oldFilePath: path to the legacy usage-statistics.json
//   - baseDir: the new usage-data/ directory
//   - pricesFn: optional function to look up model prices for cost calculation
func MigrateV1ToV2(
	oldFilePath, baseDir string,
	pricesFn func(model string) (prompt, completion, cache float64, ok bool),
) error {
	// Check if old file exists
	if _, err := os.Stat(oldFilePath); os.IsNotExist(err) {
		return nil // Nothing to migrate
	}

	log.Infof("usage: migrating v1 data from %s to %s", oldFilePath, baseDir)

	if err := os.MkdirAll(baseDir, 0o700); err != nil {
		return fmt.Errorf("create usage data dir: %w", err)
	}

	summary := newSummaryData()
	detailStore := newDetailStore(baseDir)
	todayDate := time.Now().UTC().Format("2006-01-02")

	// Process main file
	if err := migrateFile(oldFilePath, summary, detailStore, todayDate, pricesFn); err != nil {
		return fmt.Errorf("migrate main file: %w", err)
	}

	// Process legacy archive files
	dir := filepath.Dir(oldFilePath)
	entries, _ := os.ReadDir(dir)
	for _, entry := range entries {
		name := entry.Name()
		if !strings.HasPrefix(name, archivePrefix) || !strings.HasSuffix(name, ".json") {
			continue
		}
		archivePath := filepath.Join(dir, name)
		if err := migrateFile(archivePath, summary, detailStore, todayDate, pricesFn); err != nil {
			log.Warnf("usage: failed to migrate archive %s: %v", archivePath, err)
			continue
		}
		// Rename processed archive to .bak
		if err := os.Rename(archivePath, archivePath+".bak"); err != nil {
			log.Warnf("usage: failed to rename archive %s: %v", archivePath, err)
		}
	}

	// Save the migrated summary
	summaryPath := filepath.Join(baseDir, "summary.json")
	if err := saveSummary(summaryPath, summary); err != nil {
		return fmt.Errorf("save migrated summary: %w", err)
	}

	// Rename old main file to .bak
	if err := os.Rename(oldFilePath, oldFilePath+".bak"); err != nil {
		log.Warnf("usage: failed to rename old file %s: %v", oldFilePath, err)
	}

	log.Infof("usage: migration complete — %d total requests in summary", summary.Totals.Requests)
	return nil
}

// migrateFile reads a legacy persistedPayload file and feeds all details into summary + detailStore.
func migrateFile(
	filePath string,
	summary *SummaryData,
	detailStore *DetailStore,
	todayDate string,
	pricesFn func(model string) (prompt, completion, cache float64, ok bool),
) error {
	data, err := os.ReadFile(filePath)
	if err != nil {
		return fmt.Errorf("read %s: %w", filePath, err)
	}

	var payload persistedPayload
	if err := json.Unmarshal(data, &payload); err != nil {
		return fmt.Errorf("parse %s: %w", filePath, err)
	}

	// Group details by day for archival
	byDay := make(map[string][]FlatDetail)

	for apiKey, apiSnapshot := range payload.Usage.APIs {
		for modelName, modelSnapshot := range apiSnapshot.Models {
			for _, detail := range modelSnapshot.Details {
				flat := FlatDetail{
					Timestamp: detail.Timestamp,
					Model:     modelName,
					Source:    detail.Source,
					AuthIndex: detail.AuthIndex,
					Tokens:    normaliseTokenStats(detail.Tokens),
					Failed:    detail.Failed,
				}

				// Calculate cost if price function is available
				var cost float64
				if pricesFn != nil {
					prompt, completion, cache, ok := pricesFn(modelName)
					if ok {
						cost = float64(flat.Tokens.InputTokens)*prompt/tokenPriceUnit +
							float64(flat.Tokens.OutputTokens)*completion/tokenPriceUnit +
							float64(flat.Tokens.CachedTokens)*cache/tokenPriceUnit
					}
				}

				summary.Record(flat, cost)

				dayKey := detail.Timestamp.UTC().Format("2006-01-02")
				byDay[dayKey] = append(byDay[dayKey], flat)

				_ = apiKey // apiKey is preserved in Source/AuthIndex, not needed separately
			}
		}
	}

	// Archive each day's details (except today, which goes to today.json)
	for day, details := range byDay {
		if day == todayDate {
			// Today's details will be loaded into TodayStore by the caller
			todayPath := filepath.Join(detailStore.baseDir, "today.json")
			payload := todayPayload{
				Version: summaryVersion,
				Date:    todayDate,
				SavedAt: time.Now().UTC(),
				Details: details,
			}
			payloadData, err := json.MarshalIndent(payload, "", "  ")
			if err != nil {
				log.Warnf("usage: failed to marshal today data during migration: %v", err)
				continue
			}
			if err := atomicWrite(todayPath, payloadData); err != nil {
				log.Warnf("usage: failed to write today.json during migration: %v", err)
			}
			continue
		}
		if err := detailStore.Archive(day, details); err != nil {
			log.Warnf("usage: failed to archive day %s during migration: %v", day, err)
		}
	}

	return nil
}

const tokenPriceUnit = 1_000_000.0
