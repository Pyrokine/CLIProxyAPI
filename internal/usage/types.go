package usage

import (
	"encoding/json"
	"net/http"
	"time"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/logging"
	coreusage "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/usage"
)

// CostStatus describes whether a request cost is complete, partial, or unavailable.
type CostStatus string

const (
	costStatusComplete CostStatus = "complete"
	costStatusPartial  CostStatus = "partial"
	CostStatusUnpriced CostStatus = "unpriced"
)

type costCoverageCount struct {
	Requests int64 `json:"requests"`
	Tokens   int64 `json:"tokens"`
}

type costCoverage struct {
	Complete costCoverageCount `json:"complete"`
	Partial  costCoverageCount `json:"partial"`
	Unpriced costCoverageCount `json:"unpriced"`
}

// FlatDetail is the canonical denormalized request record passed between the
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
	Timestamp           time.Time                  `json:"timestamp"`
	Model               string                     `json:"model"`
	Source              string                     `json:"source"`     // credential file name
	AuthIndex           string                     `json:"auth_index"` // alternate credential identifier
	APIKey              string                     `json:"api_key"`    // stable irreversible client key identity
	ReservationID       string                     `json:"-"`
	Provider            string                     `json:"provider"` // upstream vendor (claude/codex/gemini/...)
	LatencyMs           int64                      `json:"latency_ms,omitempty"`
	RequestID           string                     `json:"request_id,omitempty"`
	RequestLogState     logging.RequestLogState    `json:"request_log_state,omitempty"`
	RequestLogLocation  logging.RequestLogLocation `json:"request_log_location,omitempty"`
	RequestLogFileName  string                     `json:"request_log_file_name,omitempty"`
	RequestLogHomeIP    string                     `json:"request_log_home_ip,omitempty"`
	RequestLogReason    string                     `json:"request_log_reason,omitempty"`
	RequestLogRevision  int64                      `json:"request_log_revision,omitempty"`
	RequestLogUpdatedAt time.Time                  `json:"request_log_updated_at,omitzero"`
	TimeToFirstByteMs   int64                      `json:"time_to_first_byte_ms,omitempty"`
	TotalDurationMs     int64                      `json:"total_duration_ms,omitempty"`
	Completed           bool                       `json:"completed"`
	Generate            bool                       `json:"generate"`
	ClientIP            string                     `json:"client_ip,omitempty"`
	XForwardedFor       string                     `json:"x_forwarded_for,omitempty"`
	UserAgent           string                     `json:"user_agent,omitempty"`
	MetadataRecorded    bool                       `json:"metadata_recorded,omitempty"`
	MetadataTruncated   bool                       `json:"metadata_truncated,omitempty"`
	ReasoningEffort     string                     `json:"reasoning_effort,omitempty"`
	ServiceTier         string                     `json:"service_tier,omitempty"`
	RequestServiceTier  string                     `json:"request_service_tier,omitempty"`
	ResponseServiceTier string                     `json:"response_service_tier,omitempty"`
	ExecutorType        string                     `json:"executor_type,omitempty"`
	Thinking            *Thinking                  `json:"thinking,omitempty"`
	ResponseHeaders     http.Header                `json:"response_headers,omitempty"`
	AccountingVersion   int                        `json:"accounting_version,omitempty"`
	TokenBreakdown      coreusage.TokenBreakdown   `json:"token_breakdown,omitempty"`
	Tokens              tokenStats                 `json:"tokens"`
	PricingReceipt      *PricingReceipt            `json:"pricing_receipt,omitempty"`
	Cost                float64                    `json:"cost"`
	CostStatus          CostStatus                 `json:"cost_status"`
	CostReason          string                     `json:"cost_reason,omitempty"`
	Failed              bool                       `json:"failed"`
	cacheWrite5mTokens  int64
	cacheWrite1hTokens  int64
	generateRecorded    bool
	metadataTrusted     bool
	fingerprint         [eventFingerprintBytes]byte
	fingerprintTrusted  bool
	costMicro           int64
	costTrusted         bool
}

// UnmarshalJSON preserves generation for legacy events that predate the field.
func (d *FlatDetail) UnmarshalJSON(data []byte) error {
	type flatDetailAlias FlatDetail
	*d = FlatDetail{}
	decoded := struct {
		Generate *bool `json:"generate"`
		*flatDetailAlias
	}{flatDetailAlias: (*flatDetailAlias)(d)}
	if err := json.Unmarshal(data, &decoded); err != nil {
		return err
	}
	d.Generate = decoded.Generate == nil || *decoded.Generate
	d.generateRecorded = decoded.Generate != nil
	return nil
}

// Thinking describes the requested thinking intensity for a usage event.
type Thinking struct {
	Intensity string `json:"intensity,omitempty"`
	Mode      string `json:"mode,omitempty"`
	Level     string `json:"level,omitempty"`
	Budget    *int   `json:"budget,omitempty"`
}

// EventFilters specifies optional dimensions the dashboard layers on top of a
// time-range query. Model and Source retain the legacy comma-separated form.
// APIKeys carries repeated api_key query parameters so values containing commas
// remain unambiguous; APIKey retains compatibility with older callers.
type EventFilters struct {
	Model   string
	Source  string
	APIKey  string
	APIKeys []string
	Status  string // "success", "failure", or "" (all)
	Search  string
}

// QueryResult is the read-side payload Query produces and the management
// handler converts into JSON. It mirrors what summary.go used to expose so
// /usage/summary's response shape doesn't change with the SQLite swap.
type QueryResult struct {
	Totals                 summaryTotals
	ByModel                map[string]*summaryModelStats
	ByCredential           map[string]*summaryCredentialStats
	ByAPIKey               map[string]*summaryAPIKeyStats
	ByAuthIndex            map[string]*summaryAPIKeyStats
	TimeSeries             []timePoint
	TimeSeriesByModel      map[string][]timePoint
	TimeSeriesByCredential map[string][]timePoint
	TimeSeriesByAPIKey     map[string][]timePoint
	TimeSeriesByAuthIndex  map[string][]timePoint
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

type latencyStats struct {
	TotalMs     int64
	SampleCount int64
}

// summaryTotals is the global aggregation block.
type summaryTotals struct {
	Requests     int64        `json:"requests"`
	Success      int64        `json:"success"`
	Failure      int64        `json:"failure"`
	Tokens       tokenStats   `json:"tokens"`
	Cost         float64      `json:"cost"`
	CostCoverage costCoverage `json:"cost_coverage"`
	Latency      latencyStats `json:"latency"`
}

// summaryModelStats is the per-model aggregation block.
type summaryModelStats struct {
	Requests int64        `json:"requests"`
	Success  int64        `json:"success"`
	Failure  int64        `json:"failure"`
	Tokens   tokenStats   `json:"tokens"`
	Cost     float64      `json:"cost"`
	Latency  latencyStats `json:"latency"`
}

// summaryCredentialStats keeps only success/failure: credentials are scoped
// for the health view, not for billing. Provider and Source are surfaced so
// the frontend can render "[Claude] alias" tags without parsing the map
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
	Requests int64        `json:"requests"`
	Success  int64        `json:"success"`
	Failure  int64        `json:"failure"`
	Tokens   tokenStats   `json:"tokens"`
	Cost     float64      `json:"cost"`
	Latency  latencyStats `json:"latency"`
}
