package management

import (
	"errors"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestConfigYAMLWithManagementSecretReplacesPlaintext(t *testing.T) {
	const plain = "plain-management-secret"
	const hashed = "$2a$10$012345678901234567890u12345678901234567890123456789012"
	body := []byte("remote-management:\n  allow-remote: true\n  secret-key: " + plain + "\n")
	persisted, err := configYAMLWithManagementSecret(body, hashed)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(persisted), plain) {
		t.Fatalf("persisted YAML contains plaintext secret:\n%s", persisted)
	}
	var parsed struct {
		RemoteManagement struct {
			SecretKey string `yaml:"secret-key"`
		} `yaml:"remote-management"`
	}
	if err = yaml.Unmarshal(persisted, &parsed); err != nil {
		t.Fatal(err)
	}
	if parsed.RemoteManagement.SecretKey != hashed {
		t.Fatalf("persisted secret = %q, want hash", parsed.RemoteManagement.SecretKey)
	}
}

func TestConfigYAMLWithManagementSecretRejectsAliasedMapping(t *testing.T) {
	body := []byte("shared: &management\n  secret-key: plain\nremote-management: *management\n")
	_, err := configYAMLWithManagementSecret(body, "hashed")
	if !errors.Is(err, errConfigYAMLManagementSecretAlias) {
		t.Fatalf("configYAMLWithManagementSecret() error = %v", err)
	}
}

func TestConfigYAMLWithManagementSecretRejectsAliasedKeys(t *testing.T) {
	for name, body := range map[string][]byte{
		"remote management key": []byte("remote-name: &remote-name remote-management\n*remote-name:\n  secret-key: plain\n"),
		"secret key":            []byte("secret-name: &secret-name secret-key\nremote-management:\n  *secret-name: plain\n"),
	} {
		t.Run(
			name, func(t *testing.T) {
				_, err := configYAMLWithManagementSecret(body, "hashed")
				if !errors.Is(err, errConfigYAMLManagementSecretAlias) {
					t.Fatalf("configYAMLWithManagementSecret() error = %v", err)
				}
			},
		)
	}
}

func TestConfigYAMLWithManagementSecretRejectsMergedSecret(t *testing.T) {
	body := []byte("shared: &management\n  secret-key: plain\nremote-management:\n  <<: *management\n")
	_, err := configYAMLWithManagementSecret(body, "hashed")
	if !errors.Is(err, errConfigYAMLManagementSecretAlias) {
		t.Fatalf("configYAMLWithManagementSecret() error = %v", err)
	}
}

func TestConfigYAMLWithManagementSecretAllowsUnrelatedAlias(t *testing.T) {
	body := []byte("shared-host: &host 127.0.0.1\nhost: *host\nremote-management:\n  secret-key: plain\n")
	persisted, err := configYAMLWithManagementSecret(body, "hashed")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(persisted), "secret-key: plain") || !strings.Contains(
		string(persisted), "secret-key: hashed",
	) {
		t.Fatalf("persisted YAML did not replace direct secret:\n%s", persisted)
	}
}
