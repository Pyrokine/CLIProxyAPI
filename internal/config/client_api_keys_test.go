package config

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestClientAPIKeyProviderLoadsScalarObjectAndMixedEntries(t *testing.T) {
	for _, test := range []struct {
		name     string
		provider string
	}{
		{"scalar", "api-key-entries:\n        - synthetic-first-key\n        - synthetic-second-key\n"},
		{
			"object",
			"api-key-entries:\n        - api-key: synthetic-first-key\n          label: first\n        - api-" +
				"key: synthetic-second-key\n          priority: 7\n",
		},
		{
			"mixed",
			"api-key-entries:\n        - synthetic-first-key\n        - api-key: sy" +
				"nthetic-second-key\n          metadata: {region: test}\n",
		},
		{"legacy api-keys", "api-keys:\n        - synthetic-first-key\n        - synthetic-second-key\n"},
		{
			"aliased scalar",
			"api-key-entries:\n        - &first synthetic-first-key\n        - " +
				"*first\n        - api-key: " +
				"synthetic-second-key\n",
		},
	} {
		t.Run(
			test.name, func(t *testing.T) {
				source := "port: 8317\napi-keys: [synthetic-root-key, synthetic-first-key]\nauth:" +
					"\n  providers:\n    config-api-key:\n      " +
					test.provider
				want := []string{"synthetic-root-key", "synthetic-first-key", "synthetic-second-key"}
				parsed, err := ParseConfigBytes([]byte(source))
				if err != nil || !reflect.DeepEqual(parsed.APIKeys, want) {
					t.Fatalf("parsed keys=%v error=%v", parsed, err)
				}
				path := filepath.Join(t.TempDir(), "config.yaml")
				if err = os.WriteFile(path, []byte(source), 0600); err != nil {
					t.Fatal(err)
				}
				loaded, err := LoadConfig(path)
				if err != nil || !reflect.DeepEqual(loaded.APIKeys, want) {
					t.Fatalf("loaded keys=%v error=%v", loaded, err)
				}
				stored, err := os.ReadFile(path)
				if err != nil || string(stored) != source {
					t.Fatalf("loader changed scalar/object configuration: error=%v", err)
				}
			},
		)
	}
}
