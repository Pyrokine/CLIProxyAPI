package quota

import (
	"encoding/json"
	"strings"
	"sync"
	"time"
)

// CredentialType identifies which provider a credential belongs to.
type CredentialType string

const (
	TypeClaude      CredentialType = "claude"
	TypeCodex       CredentialType = "codex"
	TypeGeminiCli   CredentialType = "gemini-cli"
	TypeAntigravity CredentialType = "antigravity"
	TypeKimi        CredentialType = "kimi"
	TypeXAI         CredentialType = "xai"
	TypeDevin       CredentialType = "devin"
)

// EntryKey returns the provider-aware credential key used by legacy single-identity providers.
func EntryKey(credType CredentialType, fileName string) string {
	trimmedType := strings.TrimSpace(string(credType))
	trimmedFileName := strings.TrimSpace(fileName)
	if trimmedType == "" || trimmedFileName == "" {
		return ""
	}
	return trimmedType + "::" + trimmedFileName
}

// BindingEntryKey preserves legacy keys and scopes multi-identity Devin files by auth index.
func BindingEntryKey(credType CredentialType, fileName, authIndex string) string {
	base := EntryKey(credType, fileName)
	if base == "" || credType != TypeDevin {
		return base
	}
	authIndex = strings.TrimSpace(authIndex)
	if authIndex == "" {
		return base
	}
	return base + "::" + authIndex
}

// FetchCompleteness describes whether a provider result contains every requested quota field.
type FetchCompleteness string

const (
	FetchComplete FetchCompleteness = "complete"
	FetchPartial  FetchCompleteness = "partial"
)

// Freshness describes whether cached quota data is current for its credential binding.
type Freshness string

const (
	FreshnessFresh    Freshness = "fresh"
	freshnessStale    Freshness = "stale"
	freshnessPartial  Freshness = "partial"
	FreshnessOutdated Freshness = "outdated"
)

// FetchResult carries one provider quota observation before scheduler publication.
type FetchResult struct {
	Data               []byte
	Source             string
	ObservedAt         time.Time
	FetchedAt          time.Time
	Completeness       FetchCompleteness
	AuthoritativeEmpty bool
	PartialError       string
}

// RefreshTarget identifies one provider credential binding for a refresh attempt.
type RefreshTarget struct {
	EntryKey   string         `json:"entry_key"`
	FileName   string         `json:"file_name"`
	Type       CredentialType `json:"type"`
	AuthIndex  string         `json:"auth_index,omitempty"`
	Generation uint64         `json:"generation"`
	Status     Status         `json:"status"`
	Disabled   bool           `json:"disabled,omitempty"`
}

// refreshOutcome describes the terminal result of one target refresh.
type refreshOutcome string

const (
	RefreshOutcomeSucceeded    refreshOutcome = "succeeded"
	RefreshOutcomeFailed       refreshOutcome = "failed"
	RefreshOutcomeCancelled    refreshOutcome = "cancelled"
	RefreshOutcomeInconclusive refreshOutcome = "inconclusive"
)

// RefreshResult is returned by the context-aware single-target refresh primitive.
type RefreshResult struct {
	Target          RefreshTarget  `json:"target"`
	Outcome         refreshOutcome `json:"outcome"`
	Freshness       Freshness      `json:"freshness,omitempty"`
	SuccessfulEmpty bool           `json:"successful_empty,omitempty"`
	Error           string         `json:"error,omitempty"`
	StartedAt       time.Time      `json:"started_at"`
	CompletedAt     time.Time      `json:"completed_at"`
	Shared          bool           `json:"shared_execution,omitempty"`
}

// ProbeOutcome describes a read-only provider probe result.
type ProbeOutcome string

const (
	ProbeOutcomePassed       ProbeOutcome = "passed"
	ProbeOutcomeFailed       ProbeOutcome = "failed"
	ProbeOutcomeInconclusive ProbeOutcome = "inconclusive"
	ProbeOutcomeOutdated     ProbeOutcome = "outdated"
)

// ProbeResult is returned by the read-only single-target probe primitive.
type ProbeResult struct {
	Target      RefreshTarget   `json:"target"`
	Outcome     ProbeOutcome    `json:"outcome"`
	Error       string          `json:"error,omitempty"`
	StartedAt   time.Time       `json:"started_at"`
	CompletedAt time.Time       `json:"completed_at"`
	Data        json.RawMessage `json:"-"`
}

// Status represents the current state of a credential's quota.
type Status string

// Entry status values.
const (
	StatusIdle          Status = "idle"
	statusLoading       Status = "loading"
	StatusSuccess       Status = "success"
	StatusError         Status = "error"
	StatusBanned        Status = "banned"
	StatusQuotaExceeded Status = "quota_exceeded"
)

// Entry holds the cached quota state for a single credential.
type Entry struct {
	mu                 sync.RWMutex
	disabledConfigured bool
	credentialRevision string
	FileName           string          `json:"file_name"`
	Type               CredentialType  `json:"type"`
	AuthIndex          string          `json:"auth_index,omitempty"` // credential identifier for api-call proxy
	Generation         uint64          `json:"generation"`
	StateRevision      uint64          `json:"state_revision"`
	Status             Status          `json:"status"`
	Freshness          Freshness       `json:"freshness"`
	Source             string          `json:"source,omitempty"`
	ObservedAt         *time.Time      `json:"observed_at,omitempty"`
	FetchedAt          *time.Time      `json:"fetched_at,omitempty"`
	LastSuccessAt      *time.Time      `json:"last_success_at,omitempty"`
	LastError          string          `json:"last_error,omitempty"`
	LastErrorAt        *time.Time      `json:"last_error_at,omitempty"`
	RetryAt            *time.Time      `json:"retry_at,omitempty"`
	SuccessfulEmpty    bool            `json:"successful_empty,omitempty"`
	LastRefresh        *time.Time      `json:"last_refresh"`
	NextRefresh        *time.Time      `json:"next_refresh"`
	Error              string          `json:"error,omitempty"`
	FailureCount       int             `json:"failure_count"`
	Data               json.RawMessage `json:"data,omitempty"`
	Disabled           bool            `json:"disabled,omitempty"`
}

type EntryState struct {
	FileName           string          `json:"file_name"`
	Type               CredentialType  `json:"type"`
	AuthIndex          string          `json:"auth_index,omitempty"`
	CredentialRevision string          `json:"credential_revision,omitempty"`
	Generation         uint64          `json:"generation"`
	StateRevision      uint64          `json:"state_revision"`
	Tombstone          bool            `json:"tombstone,omitempty"`
	Status             Status          `json:"status"`
	Freshness          Freshness       `json:"freshness"`
	Source             string          `json:"source,omitempty"`
	ObservedAt         *time.Time      `json:"observed_at,omitempty"`
	FetchedAt          *time.Time      `json:"fetched_at,omitempty"`
	LastSuccessAt      *time.Time      `json:"last_success_at,omitempty"`
	LastError          string          `json:"last_error,omitempty"`
	LastErrorAt        *time.Time      `json:"last_error_at,omitempty"`
	RetryAt            *time.Time      `json:"retry_at,omitempty"`
	SuccessfulEmpty    bool            `json:"successful_empty,omitempty"`
	LastRefresh        *time.Time      `json:"last_refresh"`
	NextRefresh        *time.Time      `json:"next_refresh"`
	Error              string          `json:"error,omitempty"`
	FailureCount       int             `json:"failure_count"`
	Data               json.RawMessage `json:"data,omitempty"`
	Disabled           bool            `json:"disabled,omitempty"`
}

func (e *Entry) snapshot() EntryState {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return e.snapshotLocked()
}

func (e *Entry) snapshotLocked() EntryState {
	state := EntryState{
		FileName:           e.FileName,
		Type:               e.Type,
		AuthIndex:          e.AuthIndex,
		CredentialRevision: e.credentialRevision,
		Generation:         e.Generation,
		StateRevision:      e.StateRevision,
		Status:             e.Status,
		Freshness:          e.Freshness,
		Source:             e.Source,
		LastError:          e.LastError,
		SuccessfulEmpty:    e.SuccessfulEmpty,
		Error:              e.Error,
		FailureCount:       e.FailureCount,
		Disabled:           e.Disabled,

		ObservedAt:    cloneEntryTime(e.ObservedAt),
		FetchedAt:     cloneEntryTime(e.FetchedAt),
		LastSuccessAt: cloneEntryTime(e.LastSuccessAt),
		LastErrorAt:   cloneEntryTime(e.LastErrorAt),
		RetryAt:       cloneEntryTime(e.RetryAt),
		LastRefresh:   cloneEntryTime(e.LastRefresh),
		NextRefresh:   cloneEntryTime(e.NextRefresh)}
	if e.Data != nil {
		state.Data = append(json.RawMessage(nil), e.Data...)
	}
	return state
}

func cloneEntryTime(value *time.Time) *time.Time {
	if value == nil {
		return nil
	}
	cloned := *value
	return &cloned
}

// shouldSkipAutoRefresh reports whether auto-refresh (scheduler loop or bulk RefreshNow)
// must skip this entry. Disabled/banned/quota-exceeded accounts continue to fail and
// may aggravate the underlying issue, so we only poll them via explicit manual refresh.
func (e *Entry) shouldSkipAutoRefresh() bool {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return e.Disabled || e.Status == StatusBanned || e.Status == StatusQuotaExceeded
}

// Config holds the quota refresh scheduler configuration.
type Config struct {
	Enabled     bool `yaml:"enabled" json:"enabled"`
	Interval    int  `yaml:"interval" json:"interval"`         // seconds, default 600
	MaxInterval int  `yaml:"max-interval" json:"max-interval"` // seconds, default 1800
}

// DefaultConfig returns the default quota refresh configuration.
// Enabled defaults to false (R-095): auto-polling provider endpoints risks triggering anti-abuse detection.
// Frontend shows a confirmation dialog on opt-in (GlobalSettings.tsx:67).
func DefaultConfig() Config {
	return Config{
		Enabled:     false,
		Interval:    600,
		MaxInterval: 1800,
	}
}

// ProviderStatusSummary holds provider-level quota polling status.
type ProviderStatusSummary struct {
	Provider        string     `json:"provider"`
	CredentialCount int        `json:"credential_count"`
	Idle            int        `json:"idle"`
	Loading         int        `json:"loading"`
	Success         int        `json:"success"`
	Error           int        `json:"error"`
	Banned          int        `json:"banned"`
	QuotaExceeded   int        `json:"quota_exceeded"`
	Fresh           int        `json:"fresh"`
	Stale           int        `json:"stale"`
	Partial         int        `json:"partial"`
	Outdated        int        `json:"outdated"`
	SuccessfulEmpty int        `json:"successful_empty"`
	Disabled        int        `json:"disabled"`
	FailureCount    int        `json:"failure_count"`
	LatestRefreshAt *time.Time `json:"latest_refresh_at,omitempty"`
	NextRefreshAt   *time.Time `json:"next_refresh_at,omitempty"`
}

// StatusResponse is the response for GET /quota/status.
type StatusResponse struct {
	Enabled         bool                             `json:"enabled"`
	IntervalSeconds int                              `json:"interval_seconds"`
	Credentials     map[string]*Entry                `json:"credentials"`
	ByProvider      map[string]ProviderStatusSummary `json:"by_provider"`
	UpdatedAt       time.Time                        `json:"updated_at"`
}
