package helps

import (
	"reflect"
	"strings"
	"sync"
	"testing"

	claudeauth "github.com/Pyrokine/CLIProxyAPI/v6/internal/auth/claude"
	cliproxyauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
)

func TestStableClaudeCLIAccountUUID(t *testing.T) {
	first := StableClaudeCLIAccountUUID("stable-seed")
	if first == "" {
		t.Fatal("stable account UUID is empty")
	}
	if repeated := StableClaudeCLIAccountUUID("stable-seed"); repeated != first {
		t.Fatalf("repeated UUID = %q, want %q", repeated, first)
	}
	if distinct := StableClaudeCLIAccountUUID("distinct-seed"); distinct == first {
		t.Fatalf("distinct seed produced UUID %q", distinct)
	}
}

func TestClaudeFingerprintIdentityIsolation(t *testing.T) {
	for _, metadata := range []map[string]any{nil, {}, {"unchanged": "value"}} {
		auth := &cliproxyauth.Auth{ID: "synthetic-delegated", Metadata: metadata}
		before := auth.Clone()
		var wg sync.WaitGroup
		for range 20 {
			wg.Go(func() {
				local, err := PrepareClaudeCLIFingerprintAuth(auth, ClaudeCLIAuthIdentitySeed(auth), true)
				if err != nil {
					t.Error(err)
					return
				}
				if ClaudeCredentialAccountUUID(local) == "" || !claudeauth.HasCanonicalDeviceIDPool(claudeauth.ReadDeviceIDPool(&local.Metadata)) {
					t.Error("identity missing")
				}
				local.Metadata["local"] = true
			})
		}
		wg.Wait()
		if !reflect.DeepEqual(auth.Metadata, before.Metadata) {
			t.Fatalf("shared map changed: %#v", auth.Metadata)
		}
	}
}

func TestClaudeFingerprintDelegatedIdentitySurvivesTokenRotation(t *testing.T) {
	auth := &cliproxyauth.Auth{ID: "stable", Metadata: map[string]any{"access_token": "synthetic-old"}}
	first, err := PrepareClaudeCLIFingerprintAuth(auth, ClaudeCLIAuthIdentitySeed(auth), true)
	if err != nil {
		t.Fatal(err)
	}
	auth.Metadata["access_token"] = "synthetic-new"
	second, err := PrepareClaudeCLIFingerprintAuth(auth, ClaudeCLIAuthIdentitySeed(auth), true)
	if err != nil {
		t.Fatal(err)
	}
	if first.Metadata["account_uuid"] != second.Metadata["account_uuid"] || !reflect.DeepEqual(first.Metadata[claudeauth.DeviceIDsMetadataKey], second.Metadata[claudeauth.DeviceIDsMetadataKey]) {
		t.Fatal("token rotation changed identity")
	}
	if _, err = PrepareClaudeCLIFingerprintAuth(&cliproxyauth.Auth{}, "", true); err == nil {
		t.Fatal("missing seed was accepted")
	}
	if _, err = PrepareClaudeCLIFingerprintAuth(&cliproxyauth.Auth{}, "seed", false); err == nil {
		t.Fatal("OAuth identity was synthesized")
	}
	prepared := &cliproxyauth.Auth{Metadata: map[string]any{"account_uuid": "real-account", claudeauth.DeviceIDsMetadataKey: []string{strings.Repeat("a", 64)}}}
	if local, err := PrepareClaudeCLIFingerprintAuth(prepared, "", false); err != nil || local != prepared {
		t.Fatalf("prepared OAuth changed: %v", err)
	}
}
