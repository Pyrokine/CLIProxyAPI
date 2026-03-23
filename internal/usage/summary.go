// Last compiled: 2026-03-10
// Author: pyro

package usage

import (
	"encoding/json"
	"fmt"
	"math"
	"os"
	"sync"
	"time"
)

// SummaryData holds all-time aggregated usage metrics, updated incrementally on each request.
// It is designed to be small (~100KB/year) and kept entirely in memory.
type SummaryData struct {
	mu sync.RWMutex

	Version      int                                `json:"version"`
	UpdatedAt    time.Time                          `json:"updated_at"`
	Totals       summaryTotals                      `json:"totals"`
	ByModel      map[string]*summaryModelStats      `json:"by_model"`
	ByCredential map[string]*summaryCredentialStats `json:"by_credential"`
	Daily        map[string]*daySummary             `json:"daily"`
}

// summaryTotals holds global aggregate counters.
type summaryTotals struct {
	Requests int64      `json:"requests"`
	Success  int64      `json:"success"`
	Failure  int64      `json:"failure"`
	Tokens   tokenStats `json:"tokens"`
	Cost     float64    `json:"cost"`
}

// summaryModelStats holds per-model aggregate counters.
type summaryModelStats struct {
	Requests int64      `json:"requests"`
	Success  int64      `json:"success"`
	Failure  int64      `json:"failure"`
	Tokens   tokenStats `json:"tokens"`
	Cost     float64    `json:"cost"`
}

// summaryCredentialStats holds per-credential success/failure counters.
type summaryCredentialStats struct {
	Success int64 `json:"success"`
	Failure int64 `json:"failure"`
}

// daySummary holds aggregate metrics for a single day.
type daySummary struct {
	Requests int64                       `json:"requests"`
	Success  int64                       `json:"success"`
	Failure  int64                       `json:"failure"`
	Tokens   int64                       `json:"tokens"`
	Cost     float64                     `json:"cost"`
	Hours    map[int]*hourSummary        `json:"hours"`
	Models   map[string]*dayModelSummary `json:"models"`
}

// hourSummary holds aggregate metrics for a single hour within a day.
type hourSummary struct {
	Requests int64   `json:"requests"`
	Tokens   int64   `json:"tokens"`
	Cost     float64 `json:"cost"`
}

// dayModelSummary holds per-model aggregate metrics for a single day.
type dayModelSummary struct {
	Requests int64                `json:"requests"`
	Success  int64                `json:"success"`
	Failure  int64                `json:"failure"`
	Tokens   int64                `json:"tokens"`
	Cost     float64              `json:"cost"`
	Hours    map[int]*hourSummary `json:"hours"`
}

// QueryResult holds the result of a time-range query on SummaryData.
type QueryResult struct {
	Totals            summaryTotals
	ByModel           map[string]*summaryModelStats
	ByCredential      map[string]*summaryCredentialStats
	TimeSeries        []timePoint
	TimeSeriesByModel map[string][]timePoint
}

// timePoint is a single bucket in a time series.
type timePoint struct {
	Time     string  `json:"time"`
	Requests int64   `json:"requests"`
	Tokens   int64   `json:"tokens"`
	Cost     float64 `json:"cost"`
}

const summaryVersion = 2

// newSummaryData creates an empty SummaryData ready for use.
func newSummaryData() *SummaryData {
	return &SummaryData{
		Version:      summaryVersion,
		ByModel:      make(map[string]*summaryModelStats),
		ByCredential: make(map[string]*summaryCredentialStats),
		Daily:        make(map[string]*daySummary),
	}
}

// Record incrementally updates all dimensions with a single request detail.
func (s *SummaryData) Record(detail FlatDetail, cost float64) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.UpdatedAt = time.Now().UTC()
	totalTokens := detail.Tokens.TotalTokens

	// Global totals
	s.Totals.Requests++
	if detail.Failed {
		s.Totals.Failure++
	} else {
		s.Totals.Success++
	}
	s.Totals.Tokens.InputTokens += detail.Tokens.InputTokens
	s.Totals.Tokens.OutputTokens += detail.Tokens.OutputTokens
	s.Totals.Tokens.CachedTokens += detail.Tokens.CachedTokens
	s.Totals.Tokens.ReasoningTokens += detail.Tokens.ReasoningTokens
	s.Totals.Tokens.TotalTokens += totalTokens
	s.Totals.Cost += cost

	// By model
	ms := s.ByModel[detail.Model]
	if ms == nil {
		ms = new(summaryModelStats)
		s.ByModel[detail.Model] = ms
	}
	ms.Requests++
	if detail.Failed {
		ms.Failure++
	} else {
		ms.Success++
	}
	ms.Tokens.InputTokens += detail.Tokens.InputTokens
	ms.Tokens.OutputTokens += detail.Tokens.OutputTokens
	ms.Tokens.CachedTokens += detail.Tokens.CachedTokens
	ms.Tokens.ReasoningTokens += detail.Tokens.ReasoningTokens
	ms.Tokens.TotalTokens += totalTokens
	ms.Cost += cost

	// By credential
	credKey := detail.Source
	if credKey == "" {
		credKey = detail.AuthIndex
	}
	if credKey != "" {
		cs := s.ByCredential[credKey]
		if cs == nil {
			cs = new(summaryCredentialStats)
			s.ByCredential[credKey] = cs
		}
		if detail.Failed {
			cs.Failure++
		} else {
			cs.Success++
		}
	}

	// Daily → hourly
	dayKey := detail.Timestamp.UTC().Format("2006-01-02")
	hourKey := detail.Timestamp.UTC().Hour()

	ds := s.Daily[dayKey]
	if ds == nil {
		ds = &daySummary{
			Hours:  make(map[int]*hourSummary),
			Models: make(map[string]*dayModelSummary),
		}
		s.Daily[dayKey] = ds
	}
	ds.Requests++
	if detail.Failed {
		ds.Failure++
	} else {
		ds.Success++
	}
	ds.Tokens += totalTokens
	ds.Cost += cost

	hs := ds.Hours[hourKey]
	if hs == nil {
		hs = new(hourSummary)
		ds.Hours[hourKey] = hs
	}
	hs.Requests++
	hs.Tokens += totalTokens
	hs.Cost += cost

	// Daily → model → hourly
	dms := ds.Models[detail.Model]
	if dms == nil {
		dms = &dayModelSummary{
			Hours: make(map[int]*hourSummary),
		}
		ds.Models[detail.Model] = dms
	}
	dms.Requests++
	if detail.Failed {
		dms.Failure++
	} else {
		dms.Success++
	}
	dms.Tokens += totalTokens
	dms.Cost += cost

	mhs := dms.Hours[hourKey]
	if mhs == nil {
		mhs = new(hourSummary)
		dms.Hours[hourKey] = mhs
	}
	mhs.Requests++
	mhs.Tokens += totalTokens
}

// Query returns aggregated results for the given time range.
// It filters Daily entries by [from, to] and builds time series.
func (s *SummaryData) Query(from, to time.Time) QueryResult {
	s.mu.RLock()
	defer s.mu.RUnlock()

	fromDay := from.UTC().Format("2006-01-02")
	toDay := to.UTC().Format("2006-01-02")

	result := QueryResult{
		ByModel:           make(map[string]*summaryModelStats),
		ByCredential:      make(map[string]*summaryCredentialStats),
		TimeSeriesByModel: make(map[string][]timePoint),
	}

	// If the query spans the full data range, use pre-aggregated totals
	fullRange := fromDay <= s.earliestDay() && toDay >= s.latestDay()
	if fullRange {
		result.Totals = s.Totals
		for model, ms := range s.ByModel {
			copy := *ms
			result.ByModel[model] = &copy
		}
		for cred, cs := range s.ByCredential {
			copy := *cs
			result.ByCredential[cred] = &copy
		}
	}

	// Build time series from daily entries
	for dayKey, ds := range s.Daily {
		if dayKey < fromDay || dayKey > toDay {
			continue
		}

		if !fullRange {
			// Accumulate totals from matching days
			result.Totals.Requests += ds.Requests
			result.Totals.Success += ds.Success
			result.Totals.Failure += ds.Failure
			result.Totals.Tokens.TotalTokens += ds.Tokens
			result.Totals.Cost += ds.Cost

			// Accumulate ByModel from daily model data
			for model, dms := range ds.Models {
				ms := result.ByModel[model]
				if ms == nil {
					ms = new(summaryModelStats)
					result.ByModel[model] = ms
				}
				ms.Requests += dms.Requests
				ms.Success += dms.Success
				ms.Failure += dms.Failure
				ms.Tokens.TotalTokens += dms.Tokens
				ms.Cost += dms.Cost
			}
		}

		// Day-level time point
		result.TimeSeries = append(
			result.TimeSeries, timePoint{
				Time:     dayKey + "T00:00:00Z",
				Requests: ds.Requests,
				Tokens:   ds.Tokens,
				Cost:     roundCost(ds.Cost),
			},
		)

		// Per-model day-level time points
		for model, dms := range ds.Models {
			result.TimeSeriesByModel[model] = append(
				result.TimeSeriesByModel[model], timePoint{
					Time:     dayKey + "T00:00:00Z",
					Requests: dms.Requests,
					Tokens:   dms.Tokens,
				},
			)
		}
	}

	sortTimePoints(result.TimeSeries)
	for model := range result.TimeSeriesByModel {
		sortTimePoints(result.TimeSeriesByModel[model])
	}

	if !fullRange {
		result.Totals.Cost = roundCost(result.Totals.Cost)
	}

	return result
}

// QueryHourly returns hourly-granularity time series for the given time range.
func (s *SummaryData) QueryHourly(from, to time.Time) QueryResult {
	s.mu.RLock()
	defer s.mu.RUnlock()

	fromDay := from.UTC().Format("2006-01-02")
	toDay := to.UTC().Format("2006-01-02")

	result := QueryResult{
		ByModel:           make(map[string]*summaryModelStats),
		ByCredential:      make(map[string]*summaryCredentialStats),
		TimeSeriesByModel: make(map[string][]timePoint),
	}

	for dayKey, ds := range s.Daily {
		if dayKey < fromDay || dayKey > toDay {
			continue
		}

		result.Totals.Requests += ds.Requests
		result.Totals.Success += ds.Success
		result.Totals.Failure += ds.Failure
		result.Totals.Tokens.TotalTokens += ds.Tokens
		result.Totals.Cost += ds.Cost

		for hour, hs := range ds.Hours {
			timeStr := fmt.Sprintf("%sT%02d:00:00Z", dayKey, hour)
			result.TimeSeries = append(
				result.TimeSeries, timePoint{
					Time:     timeStr,
					Requests: hs.Requests,
					Tokens:   hs.Tokens,
					Cost:     roundCost(hs.Cost),
				},
			)
		}

		for model, dms := range ds.Models {
			// Accumulate ByModel
			ms := result.ByModel[model]
			if ms == nil {
				ms = new(summaryModelStats)
				result.ByModel[model] = ms
			}
			ms.Requests += dms.Requests
			ms.Success += dms.Success
			ms.Failure += dms.Failure
			ms.Tokens.TotalTokens += dms.Tokens
			ms.Cost += dms.Cost

			for hour, mhs := range dms.Hours {
				timeStr := fmt.Sprintf("%sT%02d:00:00Z", dayKey, hour)
				result.TimeSeriesByModel[model] = append(
					result.TimeSeriesByModel[model], timePoint{
						Time:     timeStr,
						Requests: mhs.Requests,
						Tokens:   mhs.Tokens,
					},
				)
			}
		}
	}

	sortTimePoints(result.TimeSeries)
	for model := range result.TimeSeriesByModel {
		sortTimePoints(result.TimeSeriesByModel[model])
	}

	// Use pre-aggregated by_model/by_credential for full-range queries
	fullRange := fromDay <= s.earliestDay() && toDay >= s.latestDay()
	if fullRange {
		result.Totals = s.Totals
		for m, ms := range s.ByModel {
			copy := *ms
			result.ByModel[m] = &copy
		}
		for c, cs := range s.ByCredential {
			copy := *cs
			result.ByCredential[c] = &copy
		}
	}

	result.Totals.Cost = roundCost(result.Totals.Cost)
	return result
}

// cleanBefore removes daily entries older than the cutoff date from the summary.
func (s *SummaryData) cleanBefore(cutoff time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()

	cutoffDay := cutoff.UTC().Format("2006-01-02")
	for dayKey := range s.Daily {
		if dayKey < cutoffDay {
			delete(s.Daily, dayKey)
		}
	}
}

func (s *SummaryData) earliestDay() string {
	earliest := "9999-99-99"
	for day := range s.Daily {
		if day < earliest {
			earliest = day
		}
	}
	return earliest
}

func (s *SummaryData) latestDay() string {
	latest := ""
	for day := range s.Daily {
		if day > latest {
			latest = day
		}
	}
	return latest
}

func roundCost(v float64) float64 {
	return math.Round(v*100) / 100
}

func sortTimePoints(points []timePoint) {
	if len(points) <= 1 {
		return
	}
	// Simple insertion sort — typically small slices
	for i := 1; i < len(points); i++ {
		for j := i; j > 0 && points[j].Time < points[j-1].Time; j-- {
			points[j], points[j-1] = points[j-1], points[j]
		}
	}
}

// loadSummary reads a SummaryData from the given JSON file.
func loadSummary(path string) (*SummaryData, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return newSummaryData(), nil
		}
		return nil, fmt.Errorf("read summary %s: %w", path, err)
	}

	s := newSummaryData()
	if err := json.Unmarshal(data, s); err != nil {
		return nil, fmt.Errorf("parse summary %s: %w", path, err)
	}

	// Ensure maps are initialized
	if s.ByModel == nil {
		s.ByModel = make(map[string]*summaryModelStats)
	}
	if s.ByCredential == nil {
		s.ByCredential = make(map[string]*summaryCredentialStats)
	}
	if s.Daily == nil {
		s.Daily = make(map[string]*daySummary)
	}
	for _, ds := range s.Daily {
		if ds.Hours == nil {
			ds.Hours = make(map[int]*hourSummary)
		}
		if ds.Models == nil {
			ds.Models = make(map[string]*dayModelSummary)
		}
		for _, dms := range ds.Models {
			if dms.Hours == nil {
				dms.Hours = make(map[int]*hourSummary)
			}
		}
	}

	return s, nil
}

// saveSummary writes the SummaryData to disk atomically.
func saveSummary(path string, s *SummaryData) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.UpdatedAt = time.Now().UTC()
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal summary: %w", err)
	}
	return atomicWrite(path, data)
}
