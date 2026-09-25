package auth

import (
	"context"
	"testing"
)

type metadataSetterStorage struct {
	Cookie   string `json:"cookie"`
	metadata map[string]any
}

func (*metadataSetterStorage) SaveTokenToFile(string) error { return nil }

func (s *metadataSetterStorage) SetMetadata(metadata map[string]any) {
	s.metadata = metadata
}

func TestMergeExistingAuthMetadataPreservesCredentialSettings(t *testing.T) {
	storage := &metadataSetterStorage{Cookie: "new-cookie"}
	target := &Auth{
		Provider: "claude",
		Storage:  storage,
		Metadata: map[string]any{
			"type":         "claude",
			"access_token": "new-token",
		},
	}
	existing := map[string]any{
		"type":                "claude",
		"access_token":        "old-token",
		"refresh_token":       "old-refresh",
		"cookie":              "old-cookie",
		"disabled":            true,
		"weight":              float64(7),
		"proxy-url":           "http://proxy.example",
		"prefix":              "team",
		"headers":             map[string]any{"X-Team": "blue"},
		"disable-cooling":     false,
		"request-retry":       float64(0),
		"fingerprint-profile": "claude-code-cli",
		"note":                "keep me",
	}

	if errMerge := MergeExistingAuthMetadata(target, existing); errMerge != nil {
		t.Fatalf("MergeExistingAuthMetadata() error = %v", errMerge)
	}
	if target.Metadata["access_token"] != "new-token" {
		t.Fatalf("access_token = %#v, want newly acquired token", target.Metadata["access_token"])
	}
	if _, exists := target.Metadata["refresh_token"]; exists {
		t.Fatalf("old refresh token was merged: %#v", target.Metadata["refresh_token"])
	}
	if _, exists := target.Metadata["cookie"]; exists || storage.Cookie != "new-cookie" {
		t.Fatalf("new storage credential was replaced: metadata=%#v cookie=%q", target.Metadata, storage.Cookie)
	}
	if !target.Disabled || target.Status != StatusDisabled {
		t.Fatalf("disabled/status = %t/%s", target.Disabled, target.Status)
	}
	if target.ProxyURL != "http://proxy.example" || target.Prefix != "team" {
		t.Fatalf("proxy/prefix = %q/%q", target.ProxyURL, target.Prefix)
	}
	if target.Attributes[AttributeWeight] != "7" || target.Attributes["header:X-Team"] != "blue" {
		t.Fatalf("projected attributes = %#v", target.Attributes)
	}
	if target.Metadata["disable_cooling"] != false || target.Metadata["request_retry"] != float64(0) {
		t.Fatalf("cooling/retry metadata = %#v", target.Metadata)
	}
	if target.Metadata[AttributeClaudeFingerprintProfile] != "claude-code-cli" || target.Metadata["note"] != "keep me" {
		t.Fatalf("profile/custom metadata = %#v", target.Metadata)
	}
	for _, legacyKey := range []string{"proxy-url", "disable-cooling", "request-retry", "fingerprint-profile"} {
		if _, exists := target.Metadata[legacyKey]; exists {
			t.Fatalf("legacy key %q was retained: %#v", legacyKey, target.Metadata)
		}
	}
	if storage.metadata["note"] != "keep me" {
		t.Fatalf("storage metadata = %#v", storage.metadata)
	}
}

func TestMergeExistingAuthMetadataKeepsExplicitDisabledState(t *testing.T) {
	target := &Auth{Disabled: true, Status: StatusDisabled, Metadata: map[string]any{"disabled": false}}
	if errMerge := MergeExistingAuthMetadata(target, map[string]any{"disabled": true}); errMerge != nil {
		t.Fatal(errMerge)
	}
	if target.Disabled || target.Status != StatusActive || target.Metadata["disabled"] != false {
		t.Fatalf(
			"explicit disabled state changed: disabled=%t status=%s metadata=%#v",
			target.Disabled,
			target.Status,
			target.Metadata,
		)
	}
}

func TestAuthCreationIntentIsExplicit(t *testing.T) {
	//lint:ignore SA1012 This assertion verifies the documented nil-context query contract.
	if HasAuthCreationIntent(nil) || HasAuthCreationIntent(context.Background()) {
		t.Fatal("creation intent should be absent by default")
	}
	//lint:ignore SA1012 This assertion verifies the documented nil-context normalization contract.
	if !HasAuthCreationIntent(WithAuthCreationIntent(nil)) {
		t.Fatal("creation intent marker was not preserved")
	}
}
