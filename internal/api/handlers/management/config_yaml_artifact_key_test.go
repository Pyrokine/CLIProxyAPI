package management

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/artifact"
)

func TestDeriveArtifactAccessSigningKeyPersistsAndSeparatesNamespaces(t *testing.T) {
	t.Setenv("WRITABLE_PATH", t.TempDir())
	configPath := filepath.Join(t.TempDir(), "config.yaml")
	firstNamespace := artifact.InstanceNamespace(configPath)
	firstKey, err := DeriveArtifactAccessSigningKey(configPath, firstNamespace)
	if err != nil {
		t.Fatalf("DeriveArtifactAccessSigningKey first: %v", err)
	}
	rebuiltKey, err := DeriveArtifactAccessSigningKey(configPath, firstNamespace)
	if err != nil {
		t.Fatalf("DeriveArtifactAccessSigningKey rebuilt: %v", err)
	}
	if firstKey != rebuiltKey {
		t.Fatal("same instance namespace derived a different signing key")
	}
	firstSigner, err := artifact.NewAccessSigner(firstNamespace, firstKey)
	if err != nil {
		t.Fatalf("NewAccessSigner first: %v", err)
	}
	rebuiltSigner, err := artifact.NewAccessSigner(firstNamespace, rebuiltKey)
	if err != nil {
		t.Fatalf("NewAccessSigner rebuilt: %v", err)
	}
	artifactID := "img_" + strings.Repeat("a", 64) + ".png"
	token, err := firstSigner.Sign("principal", artifactID)
	if err != nil {
		t.Fatalf("Sign: %v", err)
	}
	if !rebuiltSigner.Verify("principal", artifactID, token) {
		t.Fatal("rebuilt signer rejected the previous token")
	}

	secondNamespace := artifact.InstanceNamespace(filepath.Join(t.TempDir(), "config.yaml"))
	secondKey, err := DeriveArtifactAccessSigningKey(configPath, secondNamespace)
	if err != nil {
		t.Fatalf("DeriveArtifactAccessSigningKey second namespace: %v", err)
	}
	if firstKey == secondKey {
		t.Fatal("different instance namespaces derived the same signing key")
	}
	secondSigner, err := artifact.NewAccessSigner(secondNamespace, secondKey)
	if err != nil {
		t.Fatalf("NewAccessSigner second namespace: %v", err)
	}
	if secondSigner.Verify("principal", artifactID, token) {
		t.Fatal("different namespace accepted the previous token")
	}
}
