package main

import (
	"testing"

	coreauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
)

func TestCatalogCredentialDetectionAllowsPublicPlanLabels(t *testing.T) {
	auth := &coreauth.Auth{
		Attributes: map[string]string{
			"plan_type":      "enterprise",
			"header:X-Token": "synthetic-attribute-secret",
		},
		Metadata: map[string]any{
			"plan_type": "enterprise", "access_token": "synthetic-access-secret",
			"refresh_token": "synthetic-refresh-secret",
		},
	}
	secrets := credentialSecrets(auth, "synthetic-explicit-secret")
	if containsCredentialMaterial(
		[]byte(`{"models":[{"slug":"gpt-fixture","description":"enterprise subscription"}]}`), secrets,
	) {
		t.Fatal("public subscription text treated as credential material")
	}
	for _, secret := range []string{
		"synthetic-access-secret", "synthetic-refresh-secret", "synthetic-attribute-secret", "synthetic-" +
			"explicit-secret",
	} {
		if !containsCredentialMaterial([]byte(secret), secrets) {
			t.Fatalf("credential detection lost %q", secret)
		}
	}
}
