package auth

import (
	"context"
	"testing"
)

func TestClaudeFingerprintProfilePresence(t *testing.T) {
	for _, tc := range []struct {
		name string
		auth *Auth
		want string
		set  bool
		bad  bool
	}{
		{name: "absent"},
		{name: "legacy", auth: &Auth{Metadata: map[string]any{"fingerprint-profile": " OAUTH-CLI "}}, want: "claude-code-cli", set: true},
		{name: "canonical clear", auth: &Auth{Metadata: map[string]any{"fingerprint_profile": "", "fingerprint-profile": "oauth-cli"}}, set: true},
		{name: "attribute clear", auth: &Auth{Attributes: map[string]string{"fingerprint_profile": ""}, Metadata: map[string]any{"fingerprint_profile": "claude-code-cli"}}, set: true},
		{name: "bad canonical", auth: &Auth{Metadata: map[string]any{"fingerprint_profile": "typo", "fingerprint-profile": "oauth-cli"}}, set: true, bad: true},
		{name: "null canonical", auth: &Auth{Metadata: map[string]any{"fingerprint_profile": nil}}, set: true, bad: true},
		{name: "bad attributes", auth: &Auth{Attributes: map[string]string{"fingerprint_profile": "typo"}}, set: true, bad: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			profile, set, err := ClaudeFingerprintProfile(tc.auth)
			if profile != tc.want || set != tc.set || (err != nil) != tc.bad {
				t.Fatalf("got (%q,%t,%v), want (%q,%t,error:%t)", profile, set, err, tc.want, tc.set, tc.bad)
			}
		})
	}
}

func TestApplyClaudeFingerprintProfileMetadataClearsStaleSources(t *testing.T) {
	auth := &Auth{
		Attributes: map[string]string{"fingerprint_profile": "claude-code-cli", "fingerprint-profile": "oauth-cli", "keep": "attribute"},
		Metadata:   map[string]any{"fingerprint_profile": "claude-code-cli", "fingerprint-profile": "oauth-cli", "keep": "metadata"},
	}
	if err := ApplyClaudeFingerprintProfileMetadata(auth, map[string]any{"fingerprint_profile": ""}); err != nil {
		t.Fatal(err)
	}
	if got, exists := auth.Attributes[AttributeClaudeFingerprintProfile]; !exists || got != "" {
		t.Fatalf("explicit clear missing from attributes: %v", auth.Attributes)
	}
	if got, exists := auth.Metadata[AttributeClaudeFingerprintProfile]; !exists || got != "" {
		t.Fatalf("explicit clear missing from metadata: %v", auth.Metadata)
	}
	if _, exists := auth.Metadata["fingerprint-profile"]; exists {
		t.Fatal("legacy metadata survived")
	}
	if _, exists := auth.Attributes["fingerprint-profile"]; exists {
		t.Fatal("legacy attribute survived")
	}
	if auth.Attributes["keep"] != "attribute" || auth.Metadata["keep"] != "metadata" {
		t.Fatal("unrelated fields changed")
	}
	if err := ApplyClaudeFingerprintProfileMetadata(auth, map[string]any{"fingerprint_profile": nil}); err == nil {
		t.Fatal("invalid update accepted")
	}
	if auth.Metadata[AttributeClaudeFingerprintProfile] != "" {
		t.Fatal("invalid update mutated metadata")
	}
}

func TestManagerRejectsInvalidClaudeFingerprintProfile(t *testing.T) {
	manager := NewManager(nil, nil, nil)
	auth := &Auth{ID: "synthetic", Provider: "claude", Metadata: map[string]any{"fingerprint_profile": true}}
	if _, err := manager.Register(context.Background(), auth); err == nil {
		t.Fatal("Register accepted invalid profile")
	}
	if _, exists := manager.GetByID(auth.ID); exists {
		t.Fatal("invalid registration was published")
	}
	auth.Metadata["fingerprint_profile"] = "claude-code-cli"
	if _, err := manager.Register(context.Background(), auth); err != nil {
		t.Fatal(err)
	}
	auth.Metadata["fingerprint_profile"] = nil
	if _, err := manager.Update(context.Background(), auth); err == nil {
		t.Fatal("Update accepted invalid profile")
	}
	current, _ := manager.GetByID(auth.ID)
	if current.Metadata["fingerprint_profile"] != "claude-code-cli" {
		t.Fatal("invalid update was published")
	}
}
