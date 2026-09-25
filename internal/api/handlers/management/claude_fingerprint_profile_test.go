package management

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"reflect"
	"testing"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/watcher/synthesizer"
	coreauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/pluginapi"
	"github.com/gin-gonic/gin"
)

func TestClaudeFingerprintConfigWrites(t *testing.T) {
	for _, method := range []string{http.MethodPost, http.MethodPut, http.MethodPatch} {
		for _, raw := range []string{`"oauth-cli"`, `""`, `"unknown"`, `null`, `42`, `true`, `[]`, `{}`} {
			t.Run(method+raw, func(t *testing.T) {
				initial := config.ClaudeKey{APIKey: "synthetic", Prefix: "keep", FingerprintProfile: "claude-code-cli"}
				h := newConfigListTestHandler(t, &config.Config{ClaudeKey: []config.ClaudeKey{initial}})
				beforeFile, err := os.ReadFile(h.configFilePath)
				if err != nil {
					t.Fatal(err)
				}
				value := `{"api-key":"synthetic-new","fingerprint-profile":` + raw + `}`
				body := value
				var handler gin.HandlerFunc = h.PostClaudeKey
				if method == http.MethodPut {
					handler, body = h.PutClaudeKeys, "["+value+"]"
				} else if method == http.MethodPatch {
					handler, body = h.PatchClaudeKey, `{"index":0,"value":{"fingerprint-profile":`+raw+`}}`
				}
				result := runConfigListRequest(handler, method, "/claude-api-key", body)
				valid := raw == `"oauth-cli"` || raw == `""`
				if !valid {
					if result.Code != http.StatusBadRequest {
						t.Fatalf("invalid profile status %d: %s", result.Code, result.Body.String())
					}
					afterFile, _ := os.ReadFile(h.configFilePath)
					if string(beforeFile) != string(afterFile) || !reflect.DeepEqual(h.configSnapshot().ClaudeKey, []config.ClaudeKey{initial}) {
						t.Fatal("failed validation changed live or persisted config")
					}
					return
				}
				if result.Code != http.StatusOK && result.Code != http.StatusCreated {
					t.Fatalf("write failed %d: %s", result.Code, result.Body.String())
				}
				entries := h.configSnapshot().ClaudeKey
				entry := entries[len(entries)-1]
				want := ""
				if raw == `"oauth-cli"` {
					want = "claude-code-cli"
				}
				if entry.FingerprintProfile != want {
					t.Fatalf("profile %q, want %q", entry.FingerprintProfile, want)
				}
				if method == http.MethodPatch && (entry.APIKey != initial.APIKey || entry.Prefix != initial.Prefix) {
					t.Fatal("profile patch changed unrelated fields")
				}
			})
		}
	}
}

func TestClaudeFingerprintPostSeparatesRoutesAndPatchOmission(t *testing.T) {
	h := newConfigListTestHandler(t, &config.Config{ClaudeKey: []config.ClaudeKey{{APIKey: "synthetic"}}})
	body := `{"api-key":"synthetic","fingerprint-profile":"oauth-cli"}`
	if result := runConfigListRequest(h.PostClaudeKey, http.MethodPost, "/claude-api-key", body); result.Code != http.StatusCreated {
		t.Fatalf("distinct profile route rejected: %s", result.Body.String())
	}
	if result := runConfigListRequest(h.PatchClaudeKey, http.MethodPatch, "/claude-api-key", `{"index":1,"value":{"prefix":"changed"}}`); result.Code != http.StatusOK {
		t.Fatalf("patch failed: %s", result.Body.String())
	}
	if h.configSnapshot().ClaudeKey[1].FingerprintProfile != "claude-code-cli" {
		t.Fatal("omitted profile was cleared")
	}
}

func TestClaudeFingerprintUploadedAndPluginFiles(t *testing.T) {
	for _, plugin := range []bool{false, true} {
		for _, raw := range []string{`""`, `"oauth-cli"`, `null`, `false`, `"typo"`} {
			called := 0
			var parser uploadedAuthParserFunc
			if plugin {
				parser = func(context.Context, pluginapi.AuthParseRequest) (*coreauth.Auth, bool, error) {
					called++
					return &coreauth.Auth{ID: "synthetic", Provider: "claude", Attributes: map[string]string{"fingerprint_profile": "claude-code-cli"}}, true, nil
				}
			}
			data := []byte(`{"type":"claude","fingerprint_profile":` + raw + `}`)
			valid := raw == `""` || raw == `"oauth-cli"`
			for _, source := range []string{"upload", "watcher"} {
				called = 0
				var auth *coreauth.Auth
				var err error
				if source == "upload" {
					if plugin {
						auth, _, err = authFromUploadedFile(context.Background(), parser, t.TempDir(), "synthetic.json", data)
					} else {
						auth, _, err = authFromFile(t.TempDir(), "synthetic.json", data)
					}
				} else {
					ctx := &synthesizer.SynthesisContext{AuthDir: t.TempDir(), Config: &config.Config{}}
					if plugin {
						ctx.PluginAuthParser = parser
					}
					var auths []*coreauth.Auth
					auths, err = synthesizer.SynthesizeAuthFile(ctx, "synthetic.json", data)
					if len(auths) > 0 {
						auth = auths[0]
					}
				}
				if !valid {
					if err == nil || auth != nil || called != 0 {
						t.Fatalf("%s plugin=%t accepted invalid %s, err=%v calls=%d", source, plugin, raw, err, called)
					}
					continue
				}
				if err != nil || auth == nil {
					t.Fatalf("%s failed: %v", source, err)
				}
				want := ""
				if raw == `"oauth-cli"` {
					want = "claude-code-cli"
				}
				if profile, present, err := coreauth.ClaudeFingerprintProfile(auth); err != nil || !present || profile != want || auth.Metadata["fingerprint_profile"] != want {
					t.Fatalf("%s profile not projected: %q, %t, %v", source, profile, present, err)
				}
			}
		}
	}
}

func TestClaudeFingerprintAuthFields(t *testing.T) {
	for _, tc := range []struct {
		fields string
		status int
		want   string
	}{
		{`"fingerprint_profile":""`, 200, ""},
		{`"fingerprint-profile":" OAUTH-CLI "`, 200, "claude-code-cli"},
		{`"fingerprint_profile":"","fingerprint-profile":"oauth-cli"`, 200, ""},
		{`"note":"changed"`, 200, "claude-code-cli"},
		{`"fingerprint_profile":null`, 400, "claude-code-cli"},
		{`"fingerprint_profile":false`, 400, "claude-code-cli"},
		{`"fingerprint_profile":"typo"`, 400, "claude-code-cli"},
		{`"auth-index":"unknown","fingerprint_profile":""`, 409, "claude-code-cli"},
	} {
		t.Run(tc.fields, func(t *testing.T) {
			store := &memoryAuthStore{}
			manager := coreauth.NewManager(store, nil, nil)
			auth := &coreauth.Auth{ID: "synthetic", FileName: "synthetic.json", Provider: "claude", Status: coreauth.StatusActive,
				Attributes: map[string]string{"fingerprint_profile": "claude-code-cli"},
				Metadata:   map[string]any{"fingerprint_profile": "claude-code-cli", "fingerprint-profile": "oauth-cli", "note": "keep"}}
			if _, err := manager.Register(context.Background(), auth); err != nil {
				t.Fatal(err)
			}
			h := &Handler{authManager: manager}
			result := runConfigListRequest(h.PatchAuthFileFields, http.MethodPatch, "/auth-files/fields", `{"name":"synthetic",`+tc.fields+`}`)
			if result.Code != tc.status {
				t.Fatalf("status %d, want %d: %s", result.Code, tc.status, result.Body.String())
			}
			updated, _ := manager.GetByID(auth.ID)
			if updated.Attributes["fingerprint_profile"] != tc.want || updated.Metadata["fingerprint_profile"] != tc.want {
				t.Fatal("profile was not consistently updated")
			}
			if tc.status != 200 && updated.Metadata["note"] != "keep" {
				t.Fatal("invalid update mutated unrelated fields")
			}
			store.mu.Lock()
			persisted := store.items[auth.ID].Clone()
			store.mu.Unlock()
			data, _ := json.Marshal(persisted.Metadata)
			reloaded, _, err := authFromFile(t.TempDir(), "synthetic.json", data)
			if err != nil || reloaded.Attributes["fingerprint_profile"] != tc.want {
				t.Fatalf("reload restored stale profile: %v", err)
			}
		})
	}
}
