package access

// Config groups request authentication providers.
type Config struct {
	// Providers lists configured authentication providers.
	Providers []ProviderEntry `yaml:"providers,omitempty" json:"providers,omitempty"`
}

// ProviderEntry describes a request authentication provider entry.
type ProviderEntry struct {
	// Name is the instance identifier for the provider.
	Name string `yaml:"name" json:"name"`

	// Type selects the provider implementation registered via the SDK.
	Type string `yaml:"type" json:"type"`

	// SDK optionally names a third-party SDK module providing this provider.
	SDK string `yaml:"sdk,omitempty" json:"sdk,omitempty"`

	// APIKeys lists inline keys for providers that require them.
	APIKeys []string `yaml:"api-keys,omitempty" json:"api-keys,omitempty"`

	// Config passes provider-specific options to the implementation.
	Config map[string]any `yaml:"config,omitempty" json:"config,omitempty"`
}

const (
	// ProviderTypeConfigAPIKey is the built-in provider validating inline API keys.
	ProviderTypeConfigAPIKey = "config-api-key"

	// DefaultAccessProviderName is applied when no provider name is supplied.
	DefaultAccessProviderName = "config-inline"
)

// MakeInlineAPIKeyProvider constructs an inline API key provider configuration.
// It returns nil when no keys are supplied.
// noinspection GoUnusedExportedFunction
func MakeInlineAPIKeyProvider(keys []string) *ProviderEntry {
	if len(keys) == 0 {
		return nil
	}
	provider := &ProviderEntry{
		Name:    DefaultAccessProviderName,
		Type:    ProviderTypeConfigAPIKey,
		APIKeys: append([]string(nil), keys...),
	}
	return provider
}
