// Package config provides the public SDK configuration API.
//
// It re-exports the server configuration types and helpers so external projects can
// embed CLIProxyAPI without importing internal packages.
package config

import internalconfig "github.com/Pyrokine/CLIProxyAPI/v6/internal/config"

type SDKConfig = internalconfig.SDKConfig

type Config = internalconfig.Config

type StreamingConfig = internalconfig.StreamingConfig
type TLSConfig = internalconfig.TLSConfig
type RemoteManagement = internalconfig.RemoteManagement
type AmpCode = internalconfig.AmpCode
type OAuthModelAlias = internalconfig.OAuthModelAlias
type PayloadConfig = internalconfig.PayloadConfig
type PayloadRule = internalconfig.PayloadRule
type PayloadFilterRule = internalconfig.PayloadFilterRule
type PayloadModelRule = internalconfig.PayloadModelRule

type GeminiKey = internalconfig.GeminiKey
type CodexKey = internalconfig.CodexKey
type ClaudeKey = internalconfig.ClaudeKey
type VertexCompatKey = internalconfig.VertexCompatKey
type VertexCompatModel = internalconfig.VertexCompatModel
type OpenAICompatibility = internalconfig.OpenAICompatibility
type OpenAICompatibilityAPIKey = internalconfig.OpenAICompatibilityAPIKey
type OpenAICompatibilityModel = internalconfig.OpenAICompatibilityModel

type TLS = internalconfig.TLSConfig

// noinspection GoCommentStart,GoUnusedConst,GoUnusedExportedFunction

const (
	DefaultPanelGitHubRepository = internalconfig.DefaultPanelGitHubRepository
)

// LoadConfig loads configuration from the specified file.
func LoadConfig(configFile string) (*Config, error) { return internalconfig.LoadConfig(configFile) }

// LoadConfigOptional loads configuration, optionally allowing a missing file.
// noinspection GoUnusedExportedFunction
func LoadConfigOptional(configFile string, optional bool) (*Config, error) {
	return internalconfig.LoadConfigOptional(configFile, optional)
}

// SaveConfigPreserveComments saves the configuration while preserving YAML comments.
// noinspection GoUnusedExportedFunction
func SaveConfigPreserveComments(configFile string, cfg *Config) error {
	return internalconfig.SaveConfigPreserveComments(configFile, cfg)
}

// SaveConfigPreserveCommentsUpdateNestedScalar updates a single nested scalar value.
// noinspection GoUnusedExportedFunction
func SaveConfigPreserveCommentsUpdateNestedScalar(configFile string, path []string, value string) error {
	return internalconfig.SaveConfigPreserveCommentsUpdateNestedScalar(configFile, path, value)
}

// NormalizeCommentIndentation fixes indentation of YAML comments.
// noinspection GoUnusedExportedFunction
func NormalizeCommentIndentation(data []byte) []byte {
	return internalconfig.NormalizeCommentIndentation(data)
}
