package auth

import (
	"encoding/base64"
	"encoding/json"
	"testing"
)

func testCodexBindingToken(t *testing.T, accountID, planType string, expiresAt int64) string {
	t.Helper()
	payload, err := json.Marshal(
		map[string]any{
			"exp": expiresAt,
			"https://api.openai.com/auth": map[string]any{
				"chatgpt_account_id": accountID,
				"chatgpt_plan_type":  planType,
			},
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	return "header." + base64.RawURLEncoding.EncodeToString(payload) + ".signature"
}

func TestCredentialBindingSHA256PreservesSameRefreshOwner(t *testing.T) {
	current := &Auth{
		Provider: "codex",
		Metadata: map[string]any{
			"refresh_token": "owner-refresh",
			"access_token":  "access-a",
			"id_token":      testCodexBindingToken(t, "account-a", "plus", 1),
			"last_refresh":  "2026-09-01T00:00:00Z",
			"note":          "before",
		},
		ProxyURL: "http://proxy-a.example",
	}
	updated := current.Clone()
	updated.Metadata["access_token"] = "access-b"
	updated.Metadata["id_token"] = testCodexBindingToken(t, "account-a", "plus", 2)
	updated.Metadata["last_refresh"] = "2026-09-02T00:00:00Z"
	updated.Metadata["note"] = "after"
	updated.ProxyURL = "http://proxy-b.example"

	currentBinding := credentialBindingSHA256(current)
	updatedBinding := credentialBindingSHA256(updated)
	if currentBinding == "" || currentBinding != updatedBinding {
		t.Fatalf("same owner bindings = %q, %q", currentBinding, updatedBinding)
	}
}

func TestCodexRefreshTokenRotationKeepsStableBinding(t *testing.T) {
	current := &Auth{
		Provider: "codex",
		Metadata: map[string]any{
			"refresh_token": "owner-refresh-a",
			"access_token":  "access-a",
			"id_token":      testCodexBindingToken(t, "account-a", "plus", 1),
		},
	}
	updated := current.Clone()
	updated.Metadata["refresh_token"] = "owner-refresh-b"
	updated.Metadata["access_token"] = "access-b"
	updated.Metadata["id_token"] = testCodexBindingToken(t, "account-a", "plus", 2)

	if credentialBindingSHA256(current) != credentialBindingSHA256(updated) {
		t.Fatal("stable Codex owner binding changed after token rotation")
	}
	if !persistentAuthBindingChanged(current, updated) {
		t.Fatal("explicit credential replacement did not replace the binding")
	}
	if refreshedAuthBindingChanged(current, updated) {
		t.Fatal("automatic token refresh replaced the stable owner binding")
	}
}

func TestCredentialBindingSHA256ChangesWithOwnerOrEntitlement(t *testing.T) {
	base := &Auth{
		Provider: "codex",
		Metadata: map[string]any{
			"refresh_token": "owner-a-refresh",
			"access_token":  "access-a",
			"id_token":      testCodexBindingToken(t, "account-a", "plus", 1),
		},
	}
	tests := []struct {
		name   string
		update func(*Auth)
	}{
		{
			name: "account",
			update: func(auth *Auth) {
				auth.Metadata["id_token"] = testCodexBindingToken(t, "account-b", "plus", 2)
			},
		},
		{
			name: "plan",
			update: func(auth *Auth) {
				auth.Metadata["id_token"] = testCodexBindingToken(t, "account-a", "pro", 2)
			},
		},
	}
	for _, tt := range tests {
		t.Run(
			tt.name, func(t *testing.T) {
				updated := base.Clone()
				tt.update(updated)
				if credentialBindingSHA256(base) == credentialBindingSHA256(updated) {
					t.Fatal("binding did not change")
				}
			},
		)
	}
}

func TestCredentialBindingSHA256UsesAccessTokenWithoutRefreshOwner(t *testing.T) {
	current := &Auth{Provider: "claude", Metadata: map[string]any{"access_token": "access-a"}}
	updated := current.Clone()
	updated.Metadata["access_token"] = "access-b"
	if credentialBindingSHA256(current) == credentialBindingSHA256(updated) {
		t.Fatal("access-only credential binding did not change")
	}

	codex := &Auth{
		Provider: "codex", Metadata: map[string]any{
			"access_token": "access-a",
			"id_token":     testCodexBindingToken(t, "account-a", "plus", 1),
		},
	}
	updatedCodex := codex.Clone()
	updatedCodex.Metadata["access_token"] = "access-b"
	if !persistentAuthBindingChanged(codex, updatedCodex) {
		t.Fatal("access-only Codex credential replacement did not change the binding")
	}

	apiKey := &Auth{Provider: "openai-compatibility", Attributes: map[string]string{AttributeAPIKey: "key-a"}}
	updatedAPIKey := apiKey.Clone()
	updatedAPIKey.Attributes[AttributeAPIKey] = "key-b"
	if credentialBindingSHA256(apiKey) == credentialBindingSHA256(updatedAPIKey) {
		t.Fatal("API key binding did not change")
	}
}

func TestPersistentAuthBindingChangedUsesExactBaseURL(t *testing.T) {
	current := &Auth{
		Provider: "openai-compatibility",
		Attributes: map[string]string{
			"base_url":      "https://example.com/v1/Account",
			AttributeAPIKey: "key",
		},
	}
	updated := current.Clone()
	updated.Attributes["base_url"] = "https://example.com/v1/account"
	if !persistentAuthBindingChanged(current, updated) {
		t.Fatal("case-sensitive upstream path change did not replace the binding")
	}
}
