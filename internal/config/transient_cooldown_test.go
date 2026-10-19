package config

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

func TestTransientCooldownConfigUsesFlatPathAndPreservesSentinels(t *testing.T) {
	for _, seconds := range []int{-10, -1, 0, 5, 60} {
		body := []byte(fmt.Sprintf("transient-error-cooldown-seconds: %d\n", seconds))
		parsed, err := ParseConfigBytes(body)
		if err != nil || parsed.TransientErrorCooldownSeconds != seconds {
			t.Fatalf("ParseConfigBytes(%d) = %+v, err = %v", seconds, parsed, err)
		}
		path := filepath.Join(t.TempDir(), "config.yaml")
		if err := os.WriteFile(path, body, 0o600); err != nil {
			t.Fatal(err)
		}
		loaded, err := LoadConfig(path)
		if err != nil || loaded.TransientErrorCooldownSeconds != seconds {
			t.Fatalf("LoadConfig(%d) = %+v, err = %v", seconds, loaded, err)
		}
	}
	for _, body := range []string{"{}", "transient-error-cooldown-seconds: null"} {
		cfg, err := ParseConfigBytes([]byte(body))
		if err != nil || cfg.TransientErrorCooldownSeconds != 0 {
			t.Fatalf("default cooldown = %+v, err = %v", cfg, err)
		}
	}
	for _, body := range []string{
		"transient-error-cooldown-seconds: 1.5\n",
		"value: &seconds 1.5\ntransient-error-cooldown-seconds: *seconds\n",
		"defaults: &settings {transient-error-cooldown-seconds: 1.5}\n<<: *settings\n",
	} {
		if _, err := ParseConfigBytes([]byte(body)); err == nil {
			t.Fatalf("fractional cooldown seconds were accepted: %s", body)
		}
		path := filepath.Join(t.TempDir(), "config.yaml")
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := LoadConfig(path); err == nil {
			t.Fatalf("file loading accepted fractional seconds: %s", body)
		}
		optional, err := LoadConfigOptional(path, true)
		if err != nil || optional == nil {
			t.Fatalf("optional file loading did not enter standby: config = %+v, err = %v", optional, err)
		}
		if optional.Port != 0 || optional.TransientErrorCooldownSeconds != 0 {
			t.Fatalf("optional invalid config retained server settings: %+v", optional)
		}
		if err := optional.CredentialInFlight.Validate(); err != nil {
			t.Fatalf("optional standby has invalid in-flight defaults: %v", err)
		}
	}
	for _, body := range []string{
		"value: &seconds -1\ntransient-error-cooldown-seconds: *seconds\n",
		"defaults: &settings {transient-error-cooldown-seconds: -1}\n<<: *settings\n",
	} {
		cfg, err := ParseConfigBytes([]byte(body))
		if err != nil || cfg.TransientErrorCooldownSeconds != -1 {
			t.Fatalf("integer alias = %+v, err = %v", cfg, err)
		}
	}
}
