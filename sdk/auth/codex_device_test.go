package auth

import (
	"encoding/base64"
	"encoding/json"
	"testing"

	codexauth "github.com/Pyrokine/CLIProxyAPI/v6/internal/auth/codex"
)

func TestCodexBuildAuthRecordStoresPlanTypeAttribute(t *testing.T) {
	authenticator := NewCodexAuthenticator()
	authSvc := &codexauth.Auth{}
	authBundle := &codexauth.AuthBundle{
		TokenData: codexauth.TokenData{
			IDToken:      codexDeviceTestJWT(t, "free", "account-123", "1798761600"),
			AccessToken:  "access-token",
			RefreshToken: "refresh-token",
			AccountID:    "account-123",
			Email:        "codex@example.com",
			Expire:       "2099-01-01T00:00:00Z",
		},
		LastRefresh: "2026-01-01T00:00:00Z",
	}

	record, err := authenticator.buildAuthRecord(authSvc, authBundle)
	if err != nil {
		t.Fatalf("buildAuthRecord() error = %v", err)
	}
	if record == nil {
		t.Fatal("buildAuthRecord() returned nil record")
	}
	if got := record.Attributes["plan_type"]; got != "free" {
		t.Fatalf("plan_type attribute = %q, want free", got)
	}
	if got := record.Attributes["chatgpt_subscription_active_until"]; got != "1798761600" {
		t.Fatalf("chatgpt_subscription_active_until attribute = %q, want 1798761600", got)
	}
}

func codexDeviceTestJWT(t *testing.T, planType string, accountID string, subscriptionActiveUntil string) string {
	t.Helper()
	encode := func(value any) string {
		data, err := json.Marshal(value)
		if err != nil {
			t.Fatalf("marshal jwt part: %v", err)
		}
		return base64.RawURLEncoding.EncodeToString(data)
	}
	header := map[string]any{"alg": "none", "typ": "JWT"}
	payload := map[string]any{
		"email": "codex@example.com",
		"https://api.openai.com/auth": map[string]any{
			"chatgpt_account_id":                accountID,
			"chatgpt_plan_type":                 planType,
			"chatgpt_subscription_active_until": subscriptionActiveUntil,
		},
	}
	return encode(header) + "." + encode(payload) + "."
}
