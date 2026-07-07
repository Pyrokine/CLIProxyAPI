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
)

// EntryKey returns the provider-aware credential key used by quota status and persistence.
func EntryKey(credType CredentialType, fileName string) string {
	trimmedType := strings.TrimSpace(string(credType))
	trimmedFileName := strings.TrimSpace(fileName)
	if trimmedType == "" || trimmedFileName == "" {
		return ""
	}
	return trimmedType + "::" + trimmedFileName
}

// Status represents the current state of a credential's quota.
type Status string

// Entry status values.
const (
	StatusIdle          Status = "idle"
	StatusLoading       Status = "loading"
	StatusSuccess       Status = "success"
	StatusError         Status = "error"
	StatusBanned        Status = "banned"
	StatusQuotaExceeded Status = "quota_exceeded"
)

// Entry holds the cached quota state for a single credential.
type Entry struct {
	mu                 sync.RWMutex
	disabledConfigured bool
	FileName           string          `json:"file_name"`
	Type               CredentialType  `json:"type"`
	AuthIndex          string          `json:"auth_index,omitempty"` // credential identifier for api-call proxy
	Status             Status          `json:"status"`
	LastRefresh        *time.Time      `json:"last_refresh"`
	NextRefresh        *time.Time      `json:"next_refresh"`
	Error              string          `json:"error,omitempty"`
	FailureCount       int             `json:"failure_count"`
	Data               json.RawMessage `json:"data,omitempty"`
	Disabled           bool            `json:"disabled,omitempty"`
}

type EntryState struct {
	FileName     string          `json:"file_name"`
	Type         CredentialType  `json:"type"`
	AuthIndex    string          `json:"auth_index,omitempty"`
	Status       Status          `json:"status"`
	LastRefresh  *time.Time      `json:"last_refresh"`
	NextRefresh  *time.Time      `json:"next_refresh"`
	Error        string          `json:"error,omitempty"`
	FailureCount int             `json:"failure_count"`
	Data         json.RawMessage `json:"data,omitempty"`
	Disabled     bool            `json:"disabled,omitempty"`
}

func (e *Entry) Snapshot() EntryState {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return e.snapshotLocked()
}

func (e *Entry) snapshotLocked() EntryState {
	state := EntryState{
		FileName:     e.FileName,
		Type:         e.Type,
		AuthIndex:    e.AuthIndex,
		Status:       e.Status,
		Error:        e.Error,
		FailureCount: e.FailureCount,
		Disabled:     e.Disabled,
	}
	if e.LastRefresh != nil {
		last := *e.LastRefresh
		state.LastRefresh = &last
	}
	if e.NextRefresh != nil {
		next := *e.NextRefresh
		state.NextRefresh = &next
	}
	if e.Data != nil {
		state.Data = append(json.RawMessage(nil), e.Data...)
	}
	return state
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
