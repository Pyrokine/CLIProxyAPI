// Last compiled: 2026-03-23
// Author: pyro

package usage

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// DetailStore manages historical per-day detail files on disk.
// Details are never loaded into memory permanently — they are read on demand for event queries.
type DetailStore struct {
	baseDir string
}

// newDetailStore creates a DetailStore rooted at the given directory.
func newDetailStore(baseDir string) *DetailStore {
	return &DetailStore{baseDir: baseDir}
}

// Archive writes a day's details to the appropriate monthly directory.
// The file path is: baseDir/YYYY-MM/YYYY-MM-DD.json
func (d *DetailStore) Archive(date string, details []FlatDetail) error {
	if len(details) == 0 {
		return nil
	}

	// Parse date to derive month directory
	t, err := time.Parse("2006-01-02", date)
	if err != nil {
		return fmt.Errorf("invalid date %q: %w", date, err)
	}
	monthDir := filepath.Join(d.baseDir, t.Format("2006-01"))
	if err := os.MkdirAll(monthDir, 0o700); err != nil {
		return fmt.Errorf("create month dir %s: %w", monthDir, err)
	}

	filePath := filepath.Join(monthDir, date+".json")

	// If the file already exists, merge with existing details
	existing, _ := d.loadDay(date)
	if len(existing) > 0 {
		details = mergeDetails(existing, details)
	}

	data, err := json.MarshalIndent(
		detailFilePayload{
			Version: summaryVersion,
			Date:    date,
			Details: details,
		}, "", "  ",
	)
	if err != nil {
		return fmt.Errorf("marshal details for %s: %w", date, err)
	}
	return atomicWrite(filePath, data)
}

// detailFilePayload is the on-disk format for a daily detail file.
type detailFilePayload struct {
	Version int          `json:"version"`
	Date    string       `json:"date"`
	Details []FlatDetail `json:"details"`
}

// loadDay reads all details for the given date from disk.
func (d *DetailStore) loadDay(date string) ([]FlatDetail, error) {
	t, err := time.Parse("2006-01-02", date)
	if err != nil {
		return nil, fmt.Errorf("invalid date %q: %w", date, err)
	}
	monthDir := t.Format("2006-01")
	filePath := filepath.Join(d.baseDir, monthDir, date+".json")

	data, err := os.ReadFile(filePath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("read details %s: %w", filePath, err)
	}

	var payload detailFilePayload
	if err := json.Unmarshal(data, &payload); err != nil {
		return nil, fmt.Errorf("parse details %s: %w", filePath, err)
	}
	return payload.Details, nil
}

// QueryRange loads and filters details across multiple days for the given time range.
// Returns paginated results and total count.
//
// When filters are empty and sortField is "timestamp", an optional limitHint
// enables early-exit: loading stops once enough items are collected.
// When filters are non-empty, all matching days are always loaded to ensure
// correct total counts for pagination.
func (d *DetailStore) QueryRange(
	from, to time.Time,
	page, pageSize int,
	filters EventFilters,
	sortField string,
	sortDesc bool,
	limitHint ...int,
) ([]FlatDetail, int, error) {
	days := d.daysInRange(from, to)
	if len(days) == 0 {
		return nil, 0, nil
	}

	limit := 0
	if len(limitHint) > 0 && limitHint[0] > 0 {
		limit = limitHint[0]
	}

	// Early-exit is only safe when no filters are applied (every raw item
	// survives filtering, so raw count == post-filter count).
	hasFilters := filters.Model != "" || filters.Source != "" || filters.Search != ""
	canEarlyExit := limit > 0 && !hasFilters && (sortField == "timestamp" || sortField == "")

	// When sorting by timestamp, load days in the matching order for early-exit.
	if sortField == "timestamp" || sortField == "" {
		if sortDesc {
			reverseDays(days)
		}
	}

	var all []FlatDetail
	for _, day := range days {
		details, err := d.loadDay(day)
		if err != nil {
			continue // skip unreadable files
		}
		all = append(all, details...)

		if canEarlyExit && len(all) >= limit {
			break
		}
	}

	// Apply filters
	filtered := filterDetails(all, from, to, filters)
	sortDetails(filtered, sortField, sortDesc)

	total := len(filtered)
	start, end := paginate(total, page, pageSize)
	return filtered[start:end], total, nil
}

// reverseDays reverses a slice of day strings in place.
func reverseDays(days []string) {
	for i, j := 0, len(days)-1; i < j; i, j = i+1, j-1 {
		days[i], days[j] = days[j], days[i]
	}
}

// cleanPreview returns info about files that would be deleted by cleanBefore, without actually deleting them.
func (d *DetailStore) cleanPreview(cutoff time.Time) []cleanPreviewFile {
	cutoffDay := cutoff.UTC().Format("2006-01-02")
	var result []cleanPreviewFile

	d.forEachDayFile(func(monthDir, dayDate string) {
		if dayDate < cutoffDay {
			filePath := filepath.Join(monthDir, dayDate+".json")
			info, err := os.Stat(filePath)
			if err != nil {
				return
			}
			result = append(result, cleanPreviewFile{Date: dayDate, SizeBytes: info.Size()})
		}
	})

	sort.Slice(result, func(i, j int) bool { return result[i].Date < result[j].Date })
	return result
}

// cleanPreviewFile describes a file that would be cleaned.
type cleanPreviewFile struct {
	Date      string `json:"date"`
	SizeBytes int64  `json:"size_bytes"`
}

// cleanBefore deletes all daily detail files older than the cutoff date.
func (d *DetailStore) cleanBefore(cutoff time.Time) error {
	if _, err := os.Stat(d.baseDir); err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("read usage data dir: %w", err)
	}

	cutoffDay := cutoff.UTC().Format("2006-01-02")
	monthsToCheck := make(map[string]struct{})

	d.forEachDayFile(func(monthDir, dayDate string) {
		if dayDate < cutoffDay {
			_ = os.Remove(filepath.Join(monthDir, dayDate+".json"))
			monthsToCheck[monthDir] = struct{}{}
		}
	})

	// Remove empty month directories
	for monthDir := range monthsToCheck {
		remaining, _ := os.ReadDir(monthDir)
		if len(remaining) == 0 {
			_ = os.Remove(monthDir)
		}
	}
	return nil
}

// listDays returns all archived date keys sorted ascending.
func (d *DetailStore) listDays() []string {
	var days []string
	d.forEachDayFile(func(_, dayDate string) {
		days = append(days, dayDate)
	})
	sort.Strings(days)
	return days
}

// forEachDayFile iterates over all daily JSON files in the YYYY-MM/YYYY-MM-DD.json hierarchy.
func (d *DetailStore) forEachDayFile(fn func(monthDir, dayDate string)) {
	entries, err := os.ReadDir(d.baseDir)
	if err != nil {
		return
	}
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		monthName := entry.Name()
		if len(monthName) != 7 || monthName[4] != '-' {
			continue
		}
		monthDir := filepath.Join(d.baseDir, monthName)
		dayEntries, err := os.ReadDir(monthDir)
		if err != nil {
			continue
		}
		for _, dayEntry := range dayEntries {
			name := dayEntry.Name()
			if before, ok := strings.CutSuffix(name, ".json"); ok {
				fn(monthDir, before)
			}
		}
	}
}

// daysInRange returns sorted date keys from the archive that fall within [from, to].
func (d *DetailStore) daysInRange(from, to time.Time) []string {
	fromDay := from.UTC().Format("2006-01-02")
	toDay := to.UTC().Format("2006-01-02")

	all := d.listDays()
	var result []string
	for _, day := range all {
		if day >= fromDay && day <= toDay {
			result = append(result, day)
		}
	}
	return result
}

// mergeDetails appends newDetails to existing, skipping duplicates by timestamp+model.
func mergeDetails(existing, newDetails []FlatDetail) []FlatDetail {
	seen := make(map[string]struct{}, len(existing))
	for _, d := range existing {
		key := d.Timestamp.UTC().Format(time.RFC3339Nano) + "|" + d.Model + "|" + d.Source
		seen[key] = struct{}{}
	}

	merged := make([]FlatDetail, len(existing), len(existing)+len(newDetails))
	copy(merged, existing)
	for _, d := range newDetails {
		key := d.Timestamp.UTC().Format(time.RFC3339Nano) + "|" + d.Model + "|" + d.Source
		if _, exists := seen[key]; !exists {
			merged = append(merged, d)
			seen[key] = struct{}{}
		}
	}
	return merged
}
