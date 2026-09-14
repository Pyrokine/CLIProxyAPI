package artifact

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"strings"
	"testing"
)

func TestAccessSignerBindsPrincipalNamespaceAndVersion(t *testing.T) {
	key := sha256.Sum256([]byte("artifact-access-test-key"))
	signer, err := NewAccessSigner("instance-a", key)
	if err != nil {
		t.Fatalf("NewAccessSigner: %v", err)
	}
	artifactID := "img_" + strings.Repeat("a", 64) + ".png"
	token, err := signer.Sign(" creator ", artifactID)
	if err != nil {
		t.Fatalf("Sign: %v", err)
	}
	if !strings.HasPrefix(token, "v1.") {
		t.Fatalf("token = %q, want v1 prefix", token)
	}
	if !signer.Verify("creator", artifactID, token) {
		t.Fatal("creator token did not verify")
	}
	if signer.Verify("other", artifactID, token) {
		t.Fatal("token verified for another principal")
	}

	otherNamespaceSigner, err := NewAccessSigner("instance-b", key)
	if err != nil {
		t.Fatalf("NewAccessSigner other namespace: %v", err)
	}
	if otherNamespaceSigner.Verify("creator", artifactID, token) {
		t.Fatal("token verified in another namespace")
	}
	if signer.Verify("creator", artifactID, "v2."+strings.TrimPrefix(token, "v1.")) {
		t.Fatal("token verified with another version")
	}
	if signer.Verify("creator", artifactID, "v1."+base64.RawURLEncoding.EncodeToString([]byte("short"))) {
		t.Fatal("token verified with an invalid MAC length")
	}
}

func TestAccessSignerRejectsLegacyPrincipalKeyForgery(t *testing.T) {
	key := sha256.Sum256([]byte("artifact-access-test-key"))
	signer, err := NewAccessSigner("instance-a", key)
	if err != nil {
		t.Fatalf("NewAccessSigner: %v", err)
	}
	artifactID := "img_" + strings.Repeat("b", 64) + ".png"
	legacyMAC := hmac.New(sha256.New, []byte("attacker"))
	_, _ = legacyMAC.Write([]byte(artifactID))
	legacyToken := base64.RawURLEncoding.EncodeToString(legacyMAC.Sum(nil))
	if signer.Verify("attacker", artifactID, legacyToken) {
		t.Fatal("legacy principal-key token verified")
	}
}
