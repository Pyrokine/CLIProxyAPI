// Package config provides the public SDK configuration API.
//
// It re-exports the server configuration types and helpers so external projects can
// embed CLIProxyAPI without importing internal packages.
package config

import internalconfig "github.com/Pyrokine/CLIProxyAPI/v6/internal/config"

type SDKConfig = internalconfig.SDKConfig

type ClientConfig = internalconfig.ClientConfig
type CodexClientConfig = internalconfig.CodexClientConfig

type Config = internalconfig.Config

type StreamingConfig = internalconfig.StreamingConfig
type ClaudeCodeConfig = internalconfig.ClaudeCodeConfig
type ModelCatalogConfig = internalconfig.ModelCatalogConfig
type ModelCatalogModel = internalconfig.ModelCatalogModel
type TLSConfig = internalconfig.TLSConfig
type RemoteManagement = internalconfig.RemoteManagement
type HomeConfig = internalconfig.HomeConfig
type HomeTLSConfig = internalconfig.HomeTLSConfig
type OAuthModelAlias = internalconfig.OAuthModelAlias
type OAuthModelSetting = internalconfig.OAuthModelSetting
type PayloadConfig = internalconfig.PayloadConfig
type PayloadRule = internalconfig.PayloadRule
type PayloadFilterRule = internalconfig.PayloadFilterRule
type PayloadModelRule = internalconfig.PayloadModelRule

type GeminiKey = internalconfig.GeminiKey
type CodexKey = internalconfig.CodexKey
type XAIKey = internalconfig.XAIKey
type XAIModel = internalconfig.XAIModel
type MetaKey = internalconfig.MetaKey
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

// ResolveOAuthModelSetting selects the OAuth-only model metadata override for a model.
func ResolveOAuthModelSetting(settings []OAuthModelSetting, modelID, catalogName, modelName string) *OAuthModelSetting {
	return internalconfig.ResolveOAuthModelSetting(settings, modelID, catalogName, modelName)
}

// ParseConfigBytes parses a YAML configuration payload.
func ParseConfigBytes(data []byte) (*Config, error) { return internalconfig.ParseConfigBytes(data) }

// ApplyHomeRuntimeConfig disables local entry points that Home replaces at runtime.
func ApplyHomeRuntimeConfig(cfg *Config) { internalconfig.ApplyHomeRuntimeConfig(cfg) }

func SetRuntimeModelCatalog(models []ModelCatalogModel) {
	internalconfig.SetRuntimeModelCatalog(models)
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
