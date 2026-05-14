// Last compiled: 2026-04-28
// Author: pyro

package usage

import (
	"math"
	"sort"
	"time"
)

// FlatDetail is the canonical denormalised request record passed between the
// LoggerPlugin, the persister, and the SQLite store. Each field maps to a
// dim_* table or directly to the events row. Source and AuthIndex are kept
// separate so /usage/events can still answer "which credential file" while
// dim_credential collapses them into one user-visible label.
//
// Provider was added in schema v2 to disambiguate credentials that share a
// source string across different upstream vendors (Claude OAuth and Codex
// OAuth using the same Google email being the canonical example). Empty
// string means "unknown / pre-v2" — fingerprint() and the writers treat that
// as identical to the historical NULL so legacy rows imported from the JSON
// era keep their original deduplication identity.
type FlatDetail struct {
	Timestamp time.Time  `json:"timestamp"`
	Model     string     `json:"model"`
	Source    string     `json:"source"`     // credential file name
	AuthIndex string     `json:"auth_index"` // alternate credential identifier
	APIKey    string     `json:"api_key"`    // CPA access key used by the client
	Provider  string     `json:"provider"`   // upstream vendor (claude/codex/gemini/...)
	Tokens    tokenStats `json:"tokens"`
	Failed    bool       `json:"failed"`
}

// EventFilters specifies optional dimensions the dashboard layers on top of a
// time-range query. Multi-value filters are expressed as comma-separated
// strings to match the on-the-wire form sent by the management UI.
type EventFilters struct {
	Model  string
	Source string
	APIKey string
	Status string // "success", "failure", or "" (all)
	Search string
}

// QueryResult is the read-side payload Query produces and the management
// handler converts into JSON. It mirrors what summary.go used to expose so
// /usage/summary's response shape doesn't change with the SQLite swap.
type QueryResult struct {
	Totals                 summaryTotals
	ByModel                map[string]*summaryModelStats
	ByCredential           map[string]*summaryCredentialStats
	ByAPIKey               map[string]*summaryAPIKeyStats
	TimeSeries             []timePoint
	TimeSeriesByModel      map[string][]timePoint
	TimeSeriesByCredential map[string][]timePoint
	TimeSeriesByAPIKey     map[string][]timePoint
}

// timePoint is one bucket on a time series.
type timePoint struct {
	Time     string     `json:"time"`
	Requests int64      `json:"requests"`
	Success  int64      `json:"success"`
	Failure  int64      `json:"failure"`
	Tokens   tokenStats `json:"tokens"`
	Cost     float64    `json:"cost"`
}

// summaryTotals is the global aggregation block.
type summaryTotals struct {
	Requests int64      `json:"requests"`
	Success  int64      `json:"success"`
	Failure  int64      `json:"failure"`
	Tokens   tokenStats `json:"tokens"`
	Cost     float64    `json:"cost"`
}

// summaryModelStats is the per-model aggregation block.
type summaryModelStats struct {
	Requests int64      `json:"requests"`
	Success  int64      `json:"success"`
	Failure  int64      `json:"failure"`
	Tokens   tokenStats `json:"tokens"`
	Cost     float64    `json:"cost"`
}

// summaryCredentialStats keeps only success/failure: credentials are scoped
// for the health view, not for billing. Provider and Source are surfaced so
// the frontend can render "[Claude] alias" tags without re-parsing the map
// key — the v2 by_credential map keys are "provider:source" tuples, so the
// frontend needs the components separately to look up alias dictionaries
// (which are still keyed by raw source).
type summaryCredentialStats struct {
	Success  int64  `json:"success"`
	Failure  int64  `json:"failure"`
	Provider string `json:"provider,omitempty"`
	Source   string `json:"source,omitempty"`
}

// summaryAPIKeyStats is the per-API-key aggregation block.
type summaryAPIKeyStats struct {
	Requests int64      `json:"requests"`
	Success  int64      `json:"success"`
	Failure  int64      `json:"failure"`
	Tokens   tokenStats `json:"tokens"`
	Cost     float64    `json:"cost"`
}

// roundCost rounds USD costs to two decimal places before returning them in a
// JSON response — protects the UI from trailing-cent noise on aggregations.
func roundCost(v float64) float64 {
	return math.Round(v*100) / 100
}

// sortTimePoints orders a series ascending by Time. Used by the SQL query
// path to enforce a stable order on imported data even though SQLite's ORDER
// BY already does so for live writes.
func sortTimePoints(points []timePoint) {
	sort.Slice(points, func(i, j int) bool { return points[i].Time < points[j].Time })
}
