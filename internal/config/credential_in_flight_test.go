package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"math"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestLoadConfigOptionalMissingFallbackAppliesCredentialInFlightDefaults(t *testing.T) {
	cfg, err := LoadConfigOptional(filepath.Join(t.TempDir(), "missing.yaml"), true)
	if err != nil {
		t.Fatalf("LoadConfigOptional() error = %v", err)
	}
	assertOptionalConfigFallback(t, cfg)
}

func TestLoadConfigOptionalEmptyFallbackAppliesCredentialInFlightDefaults(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadConfigOptional(path, true)
	if err != nil {
		t.Fatalf("LoadConfigOptional() error = %v", err)
	}
	assertOptionalConfigFallback(t, cfg)
}

func TestLoadConfigOptionalWhitespaceFallbackAppliesCredentialInFlightDefaults(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(" \t\n\r "), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadConfigOptional(path, true)
	if err != nil {
		t.Fatalf("LoadConfigOptional() error = %v", err)
	}
	assertOptionalConfigFallback(t, cfg)
}

func TestLoadConfigOptionalInvalidFallbackAppliesCredentialInFlightDefaults(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(":"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadConfigOptional(path, true)
	if err != nil {
		t.Fatalf("LoadConfigOptional() error = %v", err)
	}
	assertOptionalConfigFallback(t, cfg)
}

func TestLoadConfigOptionalInvalidWeightAppliesCredentialInFlightDefaults(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	content := []byte("port: 8317\ncodex-api-key:\n  - api-key: fixture\n    weight: invalid\n")
	if err := os.WriteFile(path, content, 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadConfigOptional(path, true)
	if err != nil {
		t.Fatalf("LoadConfigOptional() error = %v", err)
	}
	assertOptionalConfigFallback(t, cfg)
	if unchanged, err := os.ReadFile(path); err != nil || !bytes.Equal(unchanged, content) {
		t.Fatalf("optional configuration read changed the original file: %v", err)
	}
}

func assertOptionalConfigFallback(t *testing.T, cfg *Config) {
	t.Helper()
	if cfg.CredentialInFlight != DefaultCredentialInFlightConfig() {
		t.Fatalf("CredentialInFlight = %#v, want %#v", cfg.CredentialInFlight, DefaultCredentialInFlightConfig())
	}
	if err := cfg.CredentialInFlight.Validate(); err != nil {
		t.Fatalf("CredentialInFlight.Validate() error = %v", err)
	}
	if cfg.ErrorLogsMaxFiles != 0 || cfg.WebsocketAuth || cfg.CredentialConcurrency != (CredentialConcurrencyConfig{}) {
		t.Fatalf("fallback config changed existing empty-config defaults: %#v", cfg)
	}
}

func TestCredentialInFlightConfigContractFixture(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "home", "testdata", "credential_in_flight_contract.json"))
	if err != nil {
		t.Fatal(err)
	}
	fixture, err := decodeCredentialInFlightConfigFixture(raw)
	if err != nil {
		t.Fatal(err)
	}
	if fixture.Config != DefaultCredentialInFlightConfig() {
		t.Fatalf("default config = %#v, want %#v", DefaultCredentialInFlightConfig(), fixture.Config)
	}
	if err := fixture.Config.Validate(); err != nil {
		t.Fatalf("Validate() error = %v", err)
	}
	every, stale, retention, err := fixture.Config.Durations()
	if err != nil || every != 2*time.Second || stale != 10*time.Second || retention != time.Minute {
		t.Fatalf("durations=%v/%v/%v error=%v", every, stale, retention, err)
	}
	assertRequiredJSONKeys(t, raw, []string{"config", "part", "overflow"})
	assertRequiredJSONKeys(
		t, fixture.ConfigJSON, []string{
			"snapshot-interval", "stale-after", "max-part-bytes", "max-part-count", "max-revision-bytes",
			"max-aggregate-groups", "max-details", "max-string-bytes", "staging-retention",
		},
	)
	serialized, err := json.Marshal(fixture.Config)
	if err != nil {
		t.Fatal(err)
	}
	assertRequiredJSONKeys(
		t, serialized, []string{
			"snapshot-interval", "stale-after", "max-part-bytes", "max-part-count", "max-revision-bytes",
			"max-aggregate-groups", "max-details", "max-string-bytes", "staging-retention",
		},
	)
}

func TestCredentialInFlightConfigFixtureRejectsInvalidJSON(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "home", "testdata", "credential_in_flight_contract.json"))
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name string
		raw  []byte
	}{
		{
			name: "unknown config field",
			raw: bytes.Replace(
				raw, []byte(`"snapshot-interval": "2s"`),
				[]byte(`"snapshot-interval": "2s", "secret": "secret"`), 1,
			),
		},
		{name: "trailing JSON", raw: append(append([]byte{}, raw...), []byte(` {"config": {}}`)...)},
	} {
		t.Run(
			test.name, func(t *testing.T) {
				if _, err := decodeCredentialInFlightConfigFixture(test.raw); err == nil {
					t.Fatal("decodeCredentialInFlightConfigFixture() error = nil")
				}
			},
		)
	}
}

func TestCredentialInFlightConfigDurationBounds(t *testing.T) {
	for _, test := range []struct {
		name, stale, every string
		valid              bool
	}{
		{name: "exact three intervals", every: "1s", stale: "3s", valid: true},
		{name: "below three intervals", every: "1s", stale: "2999999999ns", valid: false},
		{
			name: "near duration maximum", every: time.Duration(math.MaxInt64 / 2).String(),
			stale: time.Duration(math.MaxInt64).String(), valid: false,
		},
	} {
		t.Run(
			test.name, func(t *testing.T) {
				cfg := DefaultCredentialInFlightConfig()
				cfg.SnapshotInterval, cfg.StaleAfter = test.every, test.stale
				if err := cfg.Validate(); (err == nil) != test.valid {
					t.Fatalf("Validate() error = %v, want valid = %t", err, test.valid)
				}
			},
		)
	}
}

func TestCredentialInFlightConfigRejectsUnsafeBounds(t *testing.T) {
	cfg := DefaultCredentialInFlightConfig()
	cfg.StaleAfter = "5s"
	if err := cfg.Validate(); err == nil {
		t.Fatal("Validate() error = nil, want stale-after error")
	}
	cfg = DefaultCredentialInFlightConfig()
	cfg.MaxRevisionBytes = 16*1024*1024 + 1
	if err := cfg.Validate(); err == nil {
		t.Fatal("Validate() error = nil, want hard revision bound error")
	}
	cfg = DefaultCredentialInFlightConfig()
	cfg.MaxPartBytes = math.MaxInt
	if err := cfg.Validate(); err == nil {
		t.Fatal("Validate() error = nil, want overflow-safe part bound error")
	}
}

type credentialInFlightConfigFixture struct {
	Config     CredentialInFlightConfig `json:"config"`
	ConfigJSON json.RawMessage          `json:"-"`
}

func decodeCredentialInFlightConfigFixture(raw []byte) (credentialInFlightConfigFixture, error) {
	var fixture credentialInFlightConfigFixture
	var document struct {
		Config   json.RawMessage `json:"config"`
		Part     json.RawMessage `json:"part"`
		Overflow json.RawMessage `json:"overflow"`
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&document); err != nil {
		return fixture, err
	}
	if err := decoder.Decode(&struct{}{}); err == nil {
		return fixture, errors.New("unexpected trailing JSON")
	} else if err != io.EOF {
		return fixture, err
	}
	decoder = json.NewDecoder(bytes.NewReader(document.Config))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&fixture.Config); err != nil {
		return fixture, err
	}
	if err := decoder.Decode(&struct{}{}); err == nil {
		return fixture, errors.New("unexpected trailing config JSON")
	} else if err != io.EOF {
		return fixture, err
	}
	fixture.ConfigJSON = document.Config
	return fixture, nil
}

func assertRequiredJSONKeys(t *testing.T, raw json.RawMessage, required []string) {
	t.Helper()
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		t.Fatalf("json.Unmarshal() error = %v", err)
	}
	for _, key := range required {
		if _, ok := fields[key]; !ok {
			t.Fatalf("required JSON key %q is missing", key)
		}
	}
}
