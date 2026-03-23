// Last compiled: 2026-03-10
// Author: pyro

package usage

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"
	"sync"
	"time"
)

// FlatDetail is a denormalized request detail record.
// Unlike the old nested API→Model→Detail structure, each record carries its own model name.
type FlatDetail struct {
	Timestamp time.Time  `json:"timestamp"`
	Model     string     `json:"model"`
	Source    string     `json:"source"`
	AuthIndex string     `json:"auth_index"`
	Tokens    tokenStats `json:"tokens"`
	Failed    bool       `json:"failed"`
}

// EventFilters specifies optional filters for querying event details.
type EventFilters struct {
	Model  string
	Source string
	Search string
}

// TodayStore manages the current day's request details in memory.
type TodayStore struct {
	mu      sync.RWMutex
	date    string // "2006-01-02" format
	details []FlatDetail
	path    string
}

// newTodayStore creates an empty TodayStore for the given date and file path.
func newTodayStore(date, path string) *TodayStore {
	return &TodayStore{
		date:    date,
		details: make([]FlatDetail, 0, 256),
		path:    path,
	}
}

// Date returns the current date key.
func (t *TodayStore) Date() string {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return t.date
}

// Append adds a detail record to today's store.
func (t *TodayStore) Append(detail FlatDetail) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.details = append(t.details, detail)
}

// rotate archives the current day's details and resets the store for a new day.
// Returns the previous day's details for archiving.
func (t *TodayStore) rotate(newDate string) (prevDate string, prevDetails []FlatDetail) {
	t.mu.Lock()
	defer t.mu.Unlock()

	prevDate = t.date
	prevDetails = t.details

	t.date = newDate
	t.details = make([]FlatDetail, 0, 256)
	return
}

// Details returns a copy of all details for today.
func (t *TodayStore) Details() []FlatDetail {
	t.mu.RLock()
	defer t.mu.RUnlock()

	result := make([]FlatDetail, len(t.details))
	copy(result, t.details)
	return result
}

// Len returns the number of details stored.
func (t *TodayStore) Len() int {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return len(t.details)
}

// Query returns a filtered, sorted, paginated slice of today's details.
// page is 1-indexed. Returns the matching slice and total count (before pagination).
func (t *TodayStore) Query(
	from, to time.Time,
	page, pageSize int,
	filters EventFilters,
	sortField string,
	sortDesc bool,
) ([]FlatDetail, int) {
	t.mu.RLock()
	defer t.mu.RUnlock()

	filtered := filterDetails(t.details, from, to, filters)
	sortDetails(filtered, sortField, sortDesc)

	total := len(filtered)
	start, end := paginate(total, page, pageSize)
	return filtered[start:end], total
}

// Save writes today's details to disk atomically.
func (t *TodayStore) Save() error {
	t.mu.RLock()
	defer t.mu.RUnlock()

	payload := todayPayload{
		Version: summaryVersion,
		Date:    t.date,
		SavedAt: time.Now().UTC(),
		Details: t.details,
	}

	data, err := json.MarshalIndent(payload, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal today store: %w", err)
	}
	return atomicWrite(t.path, data)
}

// todayPayload is the on-disk format for today.json.
type todayPayload struct {
	Version int          `json:"version"`
	Date    string       `json:"date"`
	SavedAt time.Time    `json:"saved_at"`
	Details []FlatDetail `json:"details"`
}

// loadTodayStore reads a TodayStore from the given JSON file.
// If the file date doesn't match today's date, the loaded details are returned
// separately as "stale" for archival, and the store is reset for today.
func loadTodayStore(path, todayDate string) (
	store *TodayStore,
	staleDate string,
	staleDetails []FlatDetail,
	err error,
) {
	store = newTodayStore(todayDate, path)

	data, readErr := os.ReadFile(path)
	if readErr != nil {
		if os.IsNotExist(readErr) {
			return store, "", nil, nil
		}
		return nil, "", nil, fmt.Errorf("read today store %s: %w", path, readErr)
	}

	var payload todayPayload
	if err := json.Unmarshal(data, &payload); err != nil {
		return nil, "", nil, fmt.Errorf("parse today store %s: %w", path, err)
	}

	if payload.Date == todayDate {
		// Same day — restore details
		store.details = payload.Details
		if store.details == nil {
			store.details = make([]FlatDetail, 0, 256)
		}
		return store, "", nil, nil
	}

	// Different day — these details belong to a previous date
	return store, payload.Date, payload.Details, nil
}

// filterDetails returns details matching the time range and filters.
func filterDetails(details []FlatDetail, from, to time.Time, filters EventFilters) []FlatDetail {
	result := make([]FlatDetail, 0, len(details))
	hasTimeFilter := !from.IsZero() || !to.IsZero()
	hasModelFilter := filters.Model != ""
	hasSourceFilter := filters.Source != ""
	hasSearch := filters.Search != ""
	searchLower := strings.ToLower(filters.Search)

	for _, d := range details {
		if hasTimeFilter {
			if !from.IsZero() && d.Timestamp.Before(from) {
				continue
			}
			if !to.IsZero() && d.Timestamp.After(to) {
				continue
			}
		}
		if hasModelFilter && d.Model != filters.Model {
			continue
		}
		if hasSourceFilter && d.Source != filters.Source {
			continue
		}
		if hasSearch {
			if !strings.Contains(strings.ToLower(d.Model), searchLower) &&
				!strings.Contains(strings.ToLower(d.Source), searchLower) &&
				!strings.Contains(strings.ToLower(d.AuthIndex), searchLower) {
				continue
			}
		}
		result = append(result, d)
	}
	return result
}

// sortDetails sorts details in place by the given field.
func sortDetails(details []FlatDetail, field string, desc bool) {
	if len(details) <= 1 {
		return
	}

	sort.Slice(
		details, func(i, j int) bool {
			var less bool
			switch field {
			case "model":
				less = details[i].Model < details[j].Model
			case "tokens":
				less = details[i].Tokens.TotalTokens < details[j].Tokens.TotalTokens
			default: // "timestamp"
				less = details[i].Timestamp.Before(details[j].Timestamp)
			}
			if desc {
				return !less
			}
			return less
		},
	)
}

// paginate returns start and end indices for the given page.
// When pageSize <= 0, returns the full range (no pagination).
func paginate(total, page, pageSize int) (start, end int) {
	if pageSize <= 0 {
		return 0, total
	}
	if page <= 0 {
		page = 1
	}
	start = (page - 1) * pageSize
	if start >= total {
		return total, total
	}
	end = min(start+pageSize, total)
	return
}

// FilterAndSort applies time range filtering, event filters, and sorting to a slice of FlatDetail.
func FilterAndSort(
	details []FlatDetail,
	from, to time.Time,
	filters EventFilters,
	sortField, sortOrder string,
) []FlatDetail {
	filtered := filterDetails(details, from, to, filters)
	sortDetails(filtered, sortField, sortOrder == "desc")
	return filtered
}

// MergeSorted merges two pre-sorted FlatDetail slices into a single sorted slice.
// Both inputs must already be sorted by the same field and direction.
func MergeSorted(a, b []FlatDetail, sortField string, desc bool) []FlatDetail {
	if len(a) == 0 {
		return b
	}
	if len(b) == 0 {
		return a
	}

	result := make([]FlatDetail, 0, len(a)+len(b))
	i, j := 0, 0
	for i < len(a) && j < len(b) {
		if lessDetail(a[i], b[j], sortField, desc) {
			result = append(result, a[i])
			i++
		} else {
			result = append(result, b[j])
			j++
		}
	}
	result = append(result, a[i:]...)
	result = append(result, b[j:]...)
	return result
}

// lessDetail returns true when x should appear before y in the sorted output.
func lessDetail(x, y FlatDetail, field string, desc bool) bool {
	var less bool
	switch field {
	case "model":
		less = x.Model < y.Model
	case "tokens":
		less = x.Tokens.TotalTokens < y.Tokens.TotalTokens
	default: // "timestamp"
		less = x.Timestamp.Before(y.Timestamp)
	}
	if desc {
		return !less
	}
	return less
}
