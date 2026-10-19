package util

import (
	"net/url"
	"testing"
)

func TestMaskSensitiveQueryHidesOAuthRedirectSecrets(t *testing.T) {
	masked := MaskSensitiveQuery("code=long-authorization-code&state=nonce-state-12345&%63ode=encoded-secret&source=plugin")
	values, err := url.ParseQuery(masked)
	if err != nil {
		t.Fatal(err)
	}
	if codes := values["code"]; len(codes) != 2 || codes[0] != "***" || codes[1] != "***" {
		t.Fatalf("masked codes = %v", codes)
	}
	if values.Get("state") != "***" || values.Get("source") != "plugin" {
		t.Fatalf("masked query = %q", masked)
	}
}
