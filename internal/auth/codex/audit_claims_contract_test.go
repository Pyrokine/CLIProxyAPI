package codex

import (
	"encoding/base64"
	"testing"
)

func TestAuditJWTOrganizationsClaim(t *testing.T) {
	payload := `{"email":"synthetic@example.invalid","https://api.openai.com/auth":{"chatgpt_account_id":"` +
		`synthetic-account","organizations":[{"id":"synthetic-org","is_default":true,"role":"owner","title":"` +
		`synthetic"}]}}`
	claims, err := ParseJWTToken("e30." + base64.RawURLEncoding.EncodeToString([]byte(payload)) + ".synthetic")
	if err != nil {
		t.Fatal(err)
	}
	if len(claims.AuthInfo.Organizations) != 1 || claims.AuthInfo.Organizations[0].ID != "synthetic-org" ||
		!claims.AuthInfo.Organizations[0].IsDefault ||
		claims.AuthInfo.ChatgptAccountID != "synthetic-account" {
		t.Fatalf("claims=%+v", claims.AuthInfo)
	}
}
