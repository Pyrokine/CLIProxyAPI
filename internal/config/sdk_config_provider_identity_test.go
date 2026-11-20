package config

import (
	"reflect"
	"testing"
)

func TestAPIKeyProviderPolicyPreservesLegacyNamesAndExplicitCompatibilityKeys(t *testing.T) {
	providers := []string{"kimi", "openai-compatible-kimi", "openai-compatible-custom", "other"}
	for _, sample := range []struct {
		name    string
		allowed []string
		want    []string
	}{
		{"legacy configured channel", []string{"custom"}, []string{"openai-compatible-custom"}},
		{"legacy shared provider name", []string{"kimi"}, []string{"kimi", "openai-compatible-kimi"}},
		{"explicit compatibility identity", []string{"openai-compatible-kimi"}, []string{"openai-compatible-kimi"}},
		{"case and whitespace", []string{" CUSTOM "}, []string{"openai-compatible-custom"}},
		{"unrelated provider stays denied", []string{"not-custom"}, []string{}},
		{"wildcard", []string{"*"}, providers},
	} {
		t.Run(
			sample.name, func(t *testing.T) {
				cfg := &SDKConfig{
					APIKeyRules: map[string]APIKeyRule{"synthetic-policy-key": {Providers: sample.allowed}},
				}
				if actual := cfg.FilterAPIKeyProviders("synthetic-policy-key", providers); !reflect.DeepEqual(
					actual, sample.want,
				) {
					t.Fatalf("allowed=%v filtered=%v expected=%v", sample.allowed, actual, sample.want)
				}
			},
		)
	}
}
