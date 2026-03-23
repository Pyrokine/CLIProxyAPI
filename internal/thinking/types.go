// Package thinking provides unified thinking configuration processing.
//
// This package offers a unified interface for parsing, validating, and applying
// thinking configurations across various AI providers (Claude, Gemini, OpenAI, iFlow).
package thinking

import "github.com/Pyrokine/CLIProxyAPI/v6/internal/registry"

// mode represents the type of thinking configuration mode.
type mode int

const (
	// ModeBudget indicates using a numeric budget (corresponds to suffix "(1000)" etc.)
	ModeBudget mode = iota
	// ModeLevel indicates using a discrete level (corresponds to suffix "(high)" etc.)
	ModeLevel
	// ModeNone indicates thinking is disabled (corresponds to suffix "(none)" or budget=0)
	ModeNone
	// ModeAuto indicates automatic/dynamic thinking (corresponds to suffix "(auto)" or budget=-1)
	ModeAuto
)

// String returns the string representation of Mode.
func (m mode) String() string {
	switch m {
	case ModeBudget:
		return "budget"
	case ModeLevel:
		return "level"
	case ModeNone:
		return "none"
	case ModeAuto:
		return "auto"
	default:
		return "unknown"
	}
}

// Level represents a discrete thinking level.
type Level string

const (
	// LevelNone disables thinking
	LevelNone Level = "none"
	// LevelAuto enables automatic/dynamic thinking
	LevelAuto Level = "auto"
	// levelMinimal sets minimal thinking effort
	levelMinimal Level = "minimal"
	// levelLow sets low thinking effort
	levelLow Level = "low"
	// levelMedium sets medium thinking effort
	levelMedium Level = "medium"
	// LevelHigh sets high thinking effort
	LevelHigh Level = "high"
	// LevelXHigh sets extra-high thinking effort
	LevelXHigh Level = "xhigh"
)

// Config represents a unified thinking configuration.
//
// This struct is used to pass thinking configuration information between components.
// Depending on Mode, either Budget or Level field is effective:
//   - ModeNone: Budget=0, Level is ignored
//   - ModeAuto: Budget=-1, Level is ignored
//   - ModeBudget: Budget is a positive integer, Level is ignored
//   - ModeLevel: Budget is ignored, Level is a valid level
type Config struct {
	// Mode specifies the configuration mode
	Mode mode
	// Budget is the thinking budget (token count), only effective when Mode is ModeBudget.
	// Special values: 0 means disabled, -1 means automatic
	Budget int
	// Level is the thinking level, only effective when Mode is ModeLevel
	Level Level
}

// SuffixResult represents the result of parsing a model name for thinking suffix.
//
// A thinking suffix is specified in the format model-name(value), where value
// can be a numeric budget (e.g., "16384") or a level name (e.g., "high").
type SuffixResult struct {
	// ModelName is the model name with the suffix removed.
	// If no suffix was found, this equals the original input.
	ModelName string

	// HasSuffix indicates whether a valid suffix was found.
	HasSuffix bool

	// RawSuffix is the content inside the parentheses, without the parentheses.
	// Empty string if HasSuffix is false.
	RawSuffix string
}

// ProviderApplier defines the interface for provider-specific thinking configuration application.
//
// Types implementing this interface are responsible for converting a unified Config
// into provider-specific format and applying it to the request body.
//
// Implementation requirements:
//   - Apply method must be idempotent
//   - Must not modify the input config or modelInfo
//   - Returns a modified copy of the request body
//   - Returns appropriate Error for unsupported configurations
type ProviderApplier interface {
	// Apply applies the thinking configuration to the request body.
	//
	// Parameters:
	//   - body: Original request body JSON
	//   - config: Unified thinking configuration
	//   - modelInfo: Model registry information containing ThinkingSupport properties
	//
	// Returns:
	//   - Modified request body JSON
	//   - Error if the configuration is invalid or unsupported
	Apply(body []byte, config Config, modelInfo *registry.ModelInfo) ([]byte, error)
}
