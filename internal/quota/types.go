package quota

import (
	"encoding/json"
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
)

// QuotaStatus represents the current state of a credential's quota.
type QuotaStatus string

// Entry status values.
const (
	StatusIdle          QuotaStatus = "idle"
	StatusLoading       QuotaStatus = "loading"
	StatusSuccess       QuotaStatus = "success"
	StatusError         QuotaStatus = "error"
	StatusBanned        QuotaStatus = "banned"
	StatusQuotaExceeded QuotaStatus = "quota_exceeded"
)

// Entry holds the cached quota state for a single credential.
type Entry struct {
	mu           sync.RWMutex
	FileName     string          `json:"file_name"`
	Type         CredentialType  `json:"type"`
	AuthIndex    string          `json:"auth_index,omitempty"` // credential identifier for api-call proxy
	Status       QuotaStatus     `json:"status"`
	LastRefresh  *time.Time      `json:"last_refresh"`
	NextRefresh  *time.Time      `json:"next_refresh"`
	Error        string          `json:"error,omitempty"`
	FailureCount int             `json:"failure_count"`
	Data         json.RawMessage `json:"data,omitempty"`
	Disabled     bool            `json:"disabled,omitempty"`
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

// StatusResponse is the response for GET /quota/status.
type StatusResponse struct {
	Enabled         bool              `json:"enabled"`
	IntervalSeconds int               `json:"interval_seconds"`
	Credentials     map[string]*Entry `json:"credentials"`
	UpdatedAt       time.Time         `json:"updated_at"`
}
