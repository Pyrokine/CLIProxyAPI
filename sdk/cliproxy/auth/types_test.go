package auth

import (
	"testing"
)

func TestToolPrefixDisabled(t *testing.T) {
	var a *Auth
	if a.ToolPrefixDisabled() {
		t.Error("nil auth should return false")
	}

	a = &Auth{}
	if a.ToolPrefixDisabled() {
		t.Error("empty auth should return false")
	}

	a = &Auth{Metadata: map[string]any{"tool_prefix_disabled": true}}
	if !a.ToolPrefixDisabled() {
		t.Error("should return true when set to true")
	}

	a = &Auth{Metadata: map[string]any{"tool_prefix_disabled": "true"}}
	if !a.ToolPrefixDisabled() {
		t.Error("should return true when set to string 'true'")
	}

	a = &Auth{Metadata: map[string]any{"tool-prefix-disabled": true}}
	if !a.ToolPrefixDisabled() {
		t.Error("should return true with kebab-case key")
	}

	a = &Auth{Metadata: map[string]any{"tool_prefix_disabled": false}}
	if a.ToolPrefixDisabled() {
		t.Error("should return false when set to false")
	}
}

func TestApplyCustomHeadersFromMetadata(t *testing.T) {
	t.Parallel()

	auth := &Auth{
		Metadata: map[string]any{
			"headers": map[string]any{
				"X-Test":          "value",
				" X-API-Version ": "2024-01-01",
				"empty":           "   ",
			},
		},
	}

	if err := ApplyCustomHeadersFromMetadata(auth); err != nil {
		t.Fatalf("ApplyCustomHeadersFromMetadata returned error: %v", err)
	}

	if auth.Attributes["header:X-Test"] != "value" {
		t.Fatalf("expected X-Test header to be applied, got %q", auth.Attributes["header:X-Test"])
	}
	if auth.Attributes["header:X-API-Version"] != "2024-01-01" {
		t.Fatalf("expected X-API-Version header to be applied, got %q", auth.Attributes["header:X-API-Version"])
	}
	if _, exists := auth.Attributes["header:empty"]; exists {
		t.Fatal("expected empty custom header value to be ignored")
	}
}

func TestApplyCustomHeadersFromMetadataRejectsRestrictedHeader(t *testing.T) {
	t.Parallel()

	for _, headerName := range []string{"Authorization", "Api-Key"} {
		t.Run(
			headerName, func(t *testing.T) {
				t.Parallel()
				auth := &Auth{
					Metadata: map[string]any{
						"headers": map[string]any{
							headerName: "secret",
						},
					},
				}

				if err := ApplyCustomHeadersFromMetadata(auth); err == nil {
					t.Fatal("expected restricted header error")
				}
				if _, exists := auth.Attributes["header:"+headerName]; exists {
					t.Fatal("restricted header should not be applied")
				}
			},
		)
	}
}

func TestEnsureIndexUsesCredentialIdentity(t *testing.T) {
	t.Parallel()

	geminiAuth := &Auth{
		Provider: "gemini",
		Attributes: map[string]string{
			"api_key": "shared-key",
			"source":  "config:gemini[abc123]",
		},
	}
	compatAuth := &Auth{
		Provider: "bohe",
		Attributes: map[string]string{
			"api_key":      "shared-key",
			"compat_name":  "bohe",
			"provider_key": "bohe",
			"source":       "config:bohe[def456]",
		},
	}
	geminiAltBase := &Auth{
		Provider: "gemini",
		Attributes: map[string]string{
			"api_key":  "shared-key",
			"base_url": "https://alt.example.com",
			"source":   "config:gemini[ghi789]",
		},
	}
	geminiDuplicate := &Auth{
		Provider: "gemini",
		Attributes: map[string]string{
			"api_key": "shared-key",
			"source":  "config:gemini[abc123-1]",
		},
	}

	geminiIndex := geminiAuth.EnsureIndex()
	compatIndex := compatAuth.EnsureIndex()
	altBaseIndex := geminiAltBase.EnsureIndex()
	duplicateIndex := geminiDuplicate.EnsureIndex()

	if geminiIndex == "" {
		t.Fatal("gemini index should not be empty")
	}
	if compatIndex == "" {
		t.Fatal("compat index should not be empty")
	}
	if altBaseIndex == "" {
		t.Fatal("alt base index should not be empty")
	}
	if duplicateIndex == "" {
		t.Fatal("duplicate index should not be empty")
	}
	if geminiIndex == compatIndex {
		t.Fatalf("shared api key produced duplicate auth_index %q", geminiIndex)
	}
	if geminiIndex == altBaseIndex {
		t.Fatalf("same provider/key with different base_url produced duplicate auth_index %q", geminiIndex)
	}
	if geminiIndex == duplicateIndex {
		t.Fatalf("duplicate config entries should be separated by source-derived seed, got %q", geminiIndex)
	}
}

func TestEnsureIndexMetaKeyStableAcrossConfigSources(t *testing.T) {
	makeAuth := func(key, baseURL, source string) *Auth {
		return &Auth{Provider: "meta", Attributes: map[string]string{
			"api_key": key, "base_url": baseURL, "source": source,
		}}
	}
	original := makeAuth("same-key", "https://api.meta.ai/v1", "config:meta[first]")
	reordered := makeAuth("same-key", "https://api.meta.ai/v1", "config:meta[second]")
	otherKey := makeAuth("different-key", "https://api.meta.ai/v1", "config:meta[first]")
	otherBase := makeAuth("same-key", "https://proxy.example.test/v1", "config:meta[first]")
	index := original.EnsureIndex()
	if index == "" || index != reordered.EnsureIndex() || index == otherKey.EnsureIndex() || index == otherBase.EnsureIndex() {
		t.Fatalf("Meta API key indexes must depend on credential identity, not config position: %q %q %q %q", index, reordered.Index, otherKey.Index, otherBase.Index)
	}
}

func TestEnsureIndexPrefersExplicitSeedWithoutChangingLegacyFileIdentity(t *testing.T) {
	t.Parallel()

	legacyA := (&Auth{Provider: "devin", FileName: "devin-shared.json"}).EnsureIndex()
	legacyB := (&Auth{Provider: "devin", FileName: "devin-shared.json"}).EnsureIndex()
	if legacyA == "" || legacyA != legacyB {
		t.Fatalf("legacy file indexes = %q/%q, want equal non-empty indexes", legacyA, legacyB)
	}

	seededA := (&Auth{
		Provider: "devin", FileName: "devin-shared.json",
		Attributes: map[string]string{AttributeAuthIndexSeed: "devin-user-a"},
	}).EnsureIndex()
	seededB := (&Auth{
		Provider: "devin", FileName: "devin-shared.json",
		Attributes: map[string]string{AttributeAuthIndexSeed: "devin-user-b"},
	}).EnsureIndex()
	seededACopy := (&Auth{
		Provider: "devin", FileName: "renamed.json",
		Attributes: map[string]string{AttributeAuthIndexSeed: " devin-user-a "},
	}).EnsureIndex()
	if seededA == "" || seededB == "" || seededA == seededB {
		t.Fatalf("explicit seed indexes = %q/%q, want distinct non-empty indexes", seededA, seededB)
	}
	if seededA != seededACopy {
		t.Fatalf("same explicit seed indexes = %q/%q, want equal indexes", seededA, seededACopy)
	}
	if seededA == legacyA || seededB == legacyA {
		t.Fatalf("explicit seed reused legacy file index %q", legacyA)
	}
}
