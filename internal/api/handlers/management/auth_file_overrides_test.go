package management

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/auth/codex"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/watcher/synthesizer"
	sdkauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/auth"
	coreauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
)

func TestAuthFileCredentialOverridesPersistAndReload(t *testing.T) {
	for _, withStorage := range []bool{false, true} {
		for _, tc := range []struct {
			name, fields                          string
			cooling                               *bool
			retry                                 *int
			coolingChanged, retryChanged, invalid bool
		}{
			{"missing", `"note":"kept"`, new(true), new(3), false, false, false},
			{"false_zero", `"disable_cooling":false,"request_retry":0`, new(false), new(0), true, true, false},
			{"true_positive", `"disable_cooling":true,"request_retry":2`, new(true), new(2), true, true, false},
			{"legacy", `"disable-cooling":false,"request-retry":0`, new(false), new(0), true, true, false},
			{"clear", `"disable_cooling":null,"request_retry":null`, nil, nil, true, true, false},
			{"legacy_clear", `"disable-cooling":null,"request-retry":null`, nil, nil, true, true, false},
			{"negative", `"request_retry":-1`, new(true), nil, false, true, false},
			{"canonical_first", `"disable_cooling":false,"disable-cooling":true,"request_retry":0,"request-retry":2`, new(false), new(0), true, true, false},
			{"canonical_clear", `"disable_cooling":null,"disable-cooling":true,"request_retry":null,"request-retry":2`, nil, nil, true, true, false},
			{"canonical_negative", `"request_retry":-1,"request-retry":2`, new(true), nil, false, true, false},
			{"invalid_cooling_string", `"disable_cooling":"false"`, new(true), new(3), false, false, true},
			{"invalid_cooling_number", `"disable_cooling":0`, new(true), new(3), false, false, true},
			{"invalid_retry_fraction", `"disable_cooling":false,"request_retry":0.5`, new(true), new(3), false, false, true},
			{"invalid_retry_string", `"request_retry":"2"`, new(true), new(3), false, false, true},
			{"invalid_retry_overflow", `"request_retry":9223372036854775808`, new(true), new(3), false, false, true},
		} {
			t.Run(fmt.Sprintf("storage_%t/%s", withStorage, tc.name), func(t *testing.T) {
				dir := t.TempDir()
				path := filepath.Join(dir, "synthetic.json")
				store := sdkauth.NewFileTokenStore()
				store.SetBaseDir(dir)
				manager := coreauth.NewManager(store, nil, nil)
				auth := &coreauth.Auth{ID: "synthetic.json", FileName: "synthetic.json", Provider: "codex", Status: coreauth.StatusActive,
					Attributes: map[string]string{"path": path},
					Metadata:   map[string]any{"type": "codex", "note": "untouched", "access_token": "synthetic-token", "disable_cooling": true, "disable-cooling": true, "request_retry": 3, "request-retry": 4},
				}
				if withStorage {
					auth.Storage = &codex.TokenStorage{AccessToken: "synthetic-token"}
				}
				if _, err := manager.Register(context.Background(), auth); err != nil {
					t.Fatal(err)
				}
				h := &Handler{authManager: manager}
				before, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				response := runConfigListRequest(h.PatchAuthFileFields, http.MethodPatch, "/auth-files/fields", `{"name":"synthetic.json",`+tc.fields+`}`)
				status := http.StatusOK
				if tc.invalid {
					status = http.StatusBadRequest
				}
				if response.Code != status {
					t.Fatalf("PATCH: %d %s, want %d", response.Code, response.Body.String(), status)
				}
				data, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				if tc.invalid && !bytes.Equal(before, data) {
					t.Fatal("invalid PATCH changed persisted auth")
				}
				var persisted map[string]any
				if err := json.Unmarshal(data, &persisted); err != nil {
					t.Fatal(err)
				}
				current, ok := manager.GetByID(auth.ID)
				if !ok {
					t.Fatal("auth missing")
				}
				reloaded, err := synthesizer.SynthesizeAuthFile(&synthesizer.SynthesisContext{AuthDir: dir, Config: &config.Config{}, Now: time.Unix(100, 0)}, path, data)
				if err != nil || len(reloaded) != 1 {
					t.Fatalf("reload: count=%d err=%v", len(reloaded), err)
				}
				for _, value := range []*coreauth.Auth{current, {Metadata: persisted}, reloaded[0]} {
					cooling, hasCooling := value.DisableCoolingOverride()
					retry, hasRetry := value.RequestRetryOverride()
					if hasCooling != (tc.cooling != nil) || (tc.cooling != nil && cooling != *tc.cooling) || hasRetry != (tc.retry != nil) || (tc.retry != nil && retry != *tc.retry) {
						t.Fatalf("overrides: cooling=(%t,%t) retry=(%d,%t)", cooling, hasCooling, retry, hasRetry)
					}
					if tc.coolingChanged {
						if _, ok := value.Metadata["disable-cooling"]; ok {
							t.Fatal("legacy cooling remains")
						}
						if _, ok := value.Metadata["disable_cooling"]; ok && tc.cooling == nil {
							t.Fatal("cleared cooling remains")
						}
					}
					if tc.retryChanged {
						if _, ok := value.Metadata["request-retry"]; ok {
							t.Fatal("legacy retry remains")
						}
						if _, ok := value.Metadata["request_retry"]; ok && tc.retry == nil {
							t.Fatal("cleared retry remains")
						}
					}
					if value.Metadata["access_token"] != "synthetic-token" {
						t.Fatal("unrelated synthetic token changed")
					}
				}
				entry := h.buildAuthFileEntry(current)
				if entry == nil {
					t.Fatal("auth not projected to management list")
				}
				cooling, hasCooling := entry["disable_cooling"]
				retry, hasRetry := entry["request_retry"]
				if hasCooling != (tc.cooling != nil) || (tc.cooling != nil && cooling != *tc.cooling) || hasRetry != (tc.retry != nil) || (tc.retry != nil && retry != *tc.retry) {
					t.Fatalf("management overrides: cooling=(%v,%t) retry=(%v,%t)", cooling, hasCooling, retry, hasRetry)
				}
				if _, ok := entry["access_token"]; ok {
					t.Fatal("management list exposed token")
				}
				info, err := os.Stat(path)
				if err != nil {
					t.Fatal(err)
				}
				if info.Mode().Perm() != 0o600 {
					t.Fatalf("auth permissions=%o", info.Mode().Perm())
				}
			})
		}
	}
}

func TestAuthFileModelAliasesAndErrorRulesPersistAndReload(t *testing.T) {
	for _, withStorage := range []bool{false, true} {
		t.Run(fmt.Sprintf("storage_%t", withStorage), func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "synthetic.json")
			store := sdkauth.NewFileTokenStore()
			store.SetBaseDir(dir)
			manager := coreauth.NewManager(store, nil, nil)
			auth := &coreauth.Auth{
				ID: "synthetic.json", FileName: "synthetic.json", Provider: "codex", Status: coreauth.StatusActive,
				Attributes: map[string]string{"path": path},
				Metadata:   map[string]any{"type": "codex", "access_token": "synthetic-token"},
			}
			if withStorage {
				auth.Storage = &codex.TokenStorage{AccessToken: "synthetic-token"}
			}
			if _, err := manager.Register(t.Context(), auth); err != nil {
				t.Fatal(err)
			}
			h := &Handler{authManager: manager}
			patch := func(fields string, status int) {
				t.Helper()
				resp := runConfigListRequest(h.PatchAuthFileFields, http.MethodPatch, "/auth-files/fields",
					`{"name":"synthetic.json",`+fields+`}`)
				if resp.Code != status {
					t.Fatalf("PATCH %s: status=%d body=%s", fields, resp.Code, resp.Body.String())
				}
			}
			patch(`"model_aliases":[{"name":"gpt-5","alias":"local-gpt","fork":true,"force-mapping":true}],"request_scoped_errors":[{"status":429,"match":["quota"],"action":"stop","future_key":"preserved"}]`, http.StatusOK)
			current, found := manager.GetByID(auth.ID)
			if !found || current == nil {
				t.Fatal("credential missing after update")
			}
			aliases := coreauth.OAuthModelAliasesFromAttributes(current.Attributes)
			if len(aliases) != 1 || aliases[0].Alias != "local-gpt" || !aliases[0].ForceMapping {
				t.Fatalf("runtime aliases = %#v", aliases)
			}
			if _, ok := current.Metadata["request_scoped_errors"]; !ok {
				t.Fatal("runtime request rules missing")
			}
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			var persisted map[string]any
			if err := json.Unmarshal(data, &persisted); err != nil {
				t.Fatal(err)
			}
			rules, ok := persisted["request_scoped_errors"].([]any)
			if !ok || len(rules) != 1 || rules[0].(map[string]any)["future_key"] != "preserved" || persisted["access_token"] != "synthetic-token" {
				t.Fatalf("persisted credential fields = %#v", persisted)
			}
			reloaded, err := synthesizer.SynthesizeAuthFile(&synthesizer.SynthesisContext{AuthDir: dir, Config: &config.Config{}, Now: time.Now()}, path, data)
			if err != nil || len(reloaded) != 1 || len(coreauth.OAuthModelAliasesFromAttributes(reloaded[0].Attributes)) != 1 {
				t.Fatalf("reload auths=%#v error=%v", reloaded, err)
			}
			for _, invalid := range []string{
				`"model_aliases":[{"name":"a","alias":"same"},{"name":"b","alias":"same"}]`,
				`"request_scoped_errors":[{"status":429,"match-regexr":["("],"action":"continue"}]`,
			} {
				patch(invalid, http.StatusBadRequest)
				after, err := os.ReadFile(path)
				if err != nil || !bytes.Equal(data, after) {
					t.Fatalf("invalid PATCH changed file: err=%v", err)
				}
			}
			patch(`"model_aliases":[],"request_scoped_errors":[]`, http.StatusOK)
			cleared, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			persisted = make(map[string]any)
			if err := json.Unmarshal(cleared, &persisted); err != nil {
				t.Fatal(err)
			}
			if _, ok := persisted["model_aliases"]; ok {
				t.Fatal("model aliases persisted after clearing")
			}
			if _, ok := persisted["request_scoped_errors"]; ok {
				t.Fatal("request rules persisted after clearing")
			}
			current, _ = manager.GetByID(auth.ID)
			if len(coreauth.OAuthModelAliasesFromAttributes(current.Attributes)) != 0 {
				t.Fatalf("runtime alias after clear = %#v", current.Attributes)
			}
			if _, ok := current.Metadata["request_scoped_errors"]; ok {
				t.Fatal("runtime request rules remained after clearing")
			}
		})
	}
}
