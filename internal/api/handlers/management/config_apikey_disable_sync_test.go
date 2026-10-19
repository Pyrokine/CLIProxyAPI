package management

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/watcher/synthesizer"
	coreauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
)

func TestPatchConfigAPIKeyStatusPersistsAcrossSynthesis(t *testing.T) {
	const modelPattern = "test-model-*"
	for _, tc := range []struct {
		name       string
		cfg        *config.Config
		exclusions func(*config.Config) []string
	}{
		{
			name:       "gemini",
			cfg:        &config.Config{GeminiKey: []config.GeminiKey{{APIKey: "key-1", ExcludedModels: []string{modelPattern}}}},
			exclusions: func(cfg *config.Config) []string { return cfg.GeminiKey[0].ExcludedModels },
		},
		{
			name:       "gemini without api key",
			cfg:        &config.Config{GeminiKey: []config.GeminiKey{{BaseURL: "https://gemini.example.com", ExcludedModels: []string{modelPattern}}}},
			exclusions: func(cfg *config.Config) []string { return cfg.GeminiKey[0].ExcludedModels },
		},
		{
			name:       "interactions",
			cfg:        &config.Config{InteractionsKey: []config.GeminiKey{{APIKey: "key-1", ExcludedModels: []string{modelPattern}}}},
			exclusions: func(cfg *config.Config) []string { return cfg.InteractionsKey[0].ExcludedModels },
		},
		{
			name:       "claude",
			cfg:        &config.Config{ClaudeKey: []config.ClaudeKey{{APIKey: "key-1", ExcludedModels: []string{modelPattern}}}},
			exclusions: func(cfg *config.Config) []string { return cfg.ClaudeKey[0].ExcludedModels },
		},
		{
			name:       "claude without api key",
			cfg:        &config.Config{ClaudeKey: []config.ClaudeKey{{BaseURL: "https://claude.example.com", ExcludedModels: []string{modelPattern}}}},
			exclusions: func(cfg *config.Config) []string { return cfg.ClaudeKey[0].ExcludedModels },
		},
		{
			name:       "codex",
			cfg:        &config.Config{CodexKey: []config.CodexKey{{APIKey: "key-1", BaseURL: "https://codex.example.com", ExcludedModels: []string{modelPattern}}}},
			exclusions: func(cfg *config.Config) []string { return cfg.CodexKey[0].ExcludedModels },
		},
		{
			name:       "meta",
			cfg:        &config.Config{MetaKey: []config.MetaKey{{APIKey: "key-1", BaseURL: "https://api.meta.ai/v1", ExcludedModels: []string{modelPattern}}}},
			exclusions: func(cfg *config.Config) []string { return cfg.MetaKey[0].ExcludedModels },
		},
		{
			name:       "xai",
			cfg:        &config.Config{XAIKey: []config.XAIKey{{APIKey: "key-1", BaseURL: "https://api.x.ai/v1", ExcludedModels: []string{modelPattern}}}},
			exclusions: func(cfg *config.Config) []string { return cfg.XAIKey[0].ExcludedModels },
		},
		{
			name:       "vertex",
			cfg:        &config.Config{VertexCompatAPIKey: []config.VertexCompatKey{{APIKey: "key-1", ExcludedModels: []string{modelPattern}}}},
			exclusions: func(cfg *config.Config) []string { return cfg.VertexCompatAPIKey[0].ExcludedModels },
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synthesize := func(cfg *config.Config) []*coreauth.Auth {
				t.Helper()
				auths, err := synthesizer.NewConfigSynthesizer().Synthesize(&synthesizer.SynthesisContext{
					Config: cfg, Now: time.Now(), IDGenerator: synthesizer.NewStableIDGenerator(),
				})
				if err != nil {
					t.Fatal(err)
				}
				return auths
			}
			original := synthesize(tc.cfg)[0]
			manager := coreauth.NewManager(nil, nil, nil)
			if _, err := manager.Register(t.Context(), original); err != nil {
				t.Fatal(err)
			}
			handler := newConfigListTestHandler(t, tc.cfg)
			handler.authManager = manager
			handler.configCommitted = func(ctx context.Context, commit ConfigCommit) error {
				for _, auth := range synthesize(commit.Config) {
					if _, err := manager.Update(coreauth.WithSkipPersist(ctx), auth); err != nil {
						return err
					}
				}
				return nil
			}
			for _, disabled := range []bool{true, false} {
				body := `{"name":"` + original.ID + `","auth_index":"` + original.EnsureIndex() + `","disabled":`
				if disabled {
					body += "true}"
				} else {
					body += "false}"
				}
				recorder := runConfigListRequest(handler.PatchAuthFileStatus, http.MethodPatch, "/auth-files/status", body)
				if recorder.Code != http.StatusOK {
					t.Fatalf("disabled=%v: status=%d body=%s", disabled, recorder.Code, recorder.Body.String())
				}
				loaded, err := config.LoadConfig(handler.configFilePath)
				if err != nil {
					t.Fatalf("reload config: %v", err)
				}
				generated := synthesize(loaded)
				if len(generated) != 1 {
					t.Fatalf("disabled=%v: saved config produced %d auths, want 1", disabled, len(generated))
				}
				want := []string{modelPattern}
				if disabled {
					want = append(want, "*")
				}
				if got := tc.exclusions(loaded); !reflect.DeepEqual(got, want) {
					t.Fatalf("disabled=%v: saved exclusions=%v, want %v", disabled, got, want)
				}
				regenerated := generated[0]
				if regenerated.ID != original.ID || regenerated.Disabled != disabled ||
					(regenerated.Status == coreauth.StatusDisabled) != disabled {
					t.Fatalf("disabled=%v: regenerated auth ID=%q Disabled=%v Status=%q", disabled, regenerated.ID, regenerated.Disabled, regenerated.Status)
				}
				if disabled && !slices.Contains(strings.Split(regenerated.Attributes["excluded_models"], ","), "*") {
					t.Fatal("disabled auth still advertises models")
				}
				current, ok := manager.GetByID(original.ID)
				if !ok || current.Disabled != disabled {
					t.Fatalf("disabled=%v: runtime auth=%v", disabled, current)
				}
			}
		})
	}
}

func TestPatchConfigAPIKeyStatusDoesNotGuessWhenConfigHasChanged(t *testing.T) {
	cfg := &config.Config{ClaudeKey: []config.ClaudeKey{{APIKey: "old-secret"}}}
	original, err := synthesizer.NewConfigSynthesizer().Synthesize(&synthesizer.SynthesisContext{
		Config: cfg, Now: time.Now(), IDGenerator: synthesizer.NewStableIDGenerator(),
	})
	if err != nil {
		t.Fatal(err)
	}
	manager := coreauth.NewManager(nil, nil, nil)
	if _, err = manager.Register(t.Context(), original[0]); err != nil {
		t.Fatal(err)
	}
	handler := newConfigListTestHandler(t, &config.Config{ClaudeKey: []config.ClaudeKey{{APIKey: "new-secret"}}})
	handler.authManager = manager
	body := `{"name":"` + original[0].ID + `","disabled":true}`
	recorder := runConfigListRequest(handler.PatchAuthFileStatus, http.MethodPatch, "/auth-files/status", body)
	if recorder.Code != http.StatusConflict {
		t.Fatalf("status=%d body=%s, want conflict", recorder.Code, recorder.Body.String())
	}
	current, _ := manager.GetByID(original[0].ID)
	if handler.configSnapshot().ClaudeKey[0].APIKey != "new-secret" || current == nil || current.Disabled {
		t.Fatal("stale auth ID changed the configuration or runtime auth")
	}
}

func TestPatchConfigAPIKeyStatusRejectsUnsupportedConfigAuth(t *testing.T) {
	cfg := &config.Config{OpenAICompatibility: []config.OpenAICompatibility{{
		Name: "custom", BaseURL: "https://example.com", APIKeyEntries: []config.OpenAICompatibilityAPIKey{{APIKey: "key-1"}},
	}}}
	auths, err := synthesizer.NewConfigSynthesizer().Synthesize(&synthesizer.SynthesisContext{
		Config: cfg, Now: time.Now(), IDGenerator: synthesizer.NewStableIDGenerator(),
	})
	if err != nil {
		t.Fatal(err)
	}
	manager := coreauth.NewManager(nil, nil, nil)
	if _, err = manager.Register(t.Context(), auths[0]); err != nil {
		t.Fatal(err)
	}
	handler := newConfigListTestHandler(t, cfg)
	handler.authManager = manager
	recorder := runConfigListRequest(handler.PatchAuthFileStatus, http.MethodPatch, "/auth-files/status", `{"name":"`+auths[0].ID+`","disabled":true}`)
	if recorder.Code != http.StatusConflict {
		t.Fatalf("status=%d body=%s, want conflict", recorder.Code, recorder.Body.String())
	}
	if cfg.OpenAICompatibility[0].Disabled || auths[0].Disabled {
		t.Fatal("unsupported config auth changed")
	}
}

func TestPatchConfigAPIKeyStatusTargetsReorderedClaudeKey(t *testing.T) {
	original := &config.Config{ClaudeKey: []config.ClaudeKey{
		{APIKey: "same-key", Prefix: "first", ExcludedModels: []string{"keep-first"}},
		{APIKey: "same-key", Prefix: "second", ExcludedModels: []string{"keep-second"}},
	}}
	auths, err := synthesizer.NewConfigSynthesizer().Synthesize(&synthesizer.SynthesisContext{
		Config: original, Now: time.Now(), IDGenerator: synthesizer.NewStableIDGenerator(),
	})
	if err != nil || len(auths) != 2 {
		t.Fatalf("synthesize auths: %v, count=%d", err, len(auths))
	}
	manager := coreauth.NewManager(nil, nil, nil)
	for _, auth := range auths {
		if _, err = manager.Register(t.Context(), auth); err != nil {
			t.Fatal(err)
		}
	}
	handler := newConfigListTestHandler(t, &config.Config{ClaudeKey: []config.ClaudeKey{
		original.ClaudeKey[1], original.ClaudeKey[0],
	}})
	handler.authManager = manager
	body := `{"name":"` + auths[1].ID + `","auth_index":"` + auths[1].EnsureIndex() + `","disabled":true}`
	recorder := runConfigListRequest(handler.PatchAuthFileStatus, http.MethodPatch, "/auth-files/status", body)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	got := handler.configSnapshot().ClaudeKey
	if !reflect.DeepEqual(got[0].ExcludedModels, []string{"keep-second", "*"}) ||
		!reflect.DeepEqual(got[1].ExcludedModels, []string{"keep-first"}) {
		t.Fatalf("wrong Claude key changed after reorder: %#v", got)
	}
}

func TestPatchConfigAPIKeyStatusKeepsRuntimeOnSaveFailure(t *testing.T) {
	cfg := &config.Config{GeminiKey: []config.GeminiKey{{APIKey: "key-1"}}}
	auths, err := synthesizer.NewConfigSynthesizer().Synthesize(&synthesizer.SynthesisContext{
		Config: cfg, Now: time.Now(), IDGenerator: synthesizer.NewStableIDGenerator(),
	})
	if err != nil {
		t.Fatal(err)
	}
	manager := coreauth.NewManager(nil, nil, nil)
	if _, err = manager.Register(t.Context(), auths[0]); err != nil {
		t.Fatal(err)
	}
	handler := newConfigListTestHandler(t, cfg)
	handler.authManager = manager
	blocked := filepath.Join(t.TempDir(), "not-a-directory")
	if err = os.WriteFile(blocked, []byte("blocked"), 0o600); err != nil {
		t.Fatal(err)
	}
	handler.configFilePath = filepath.Join(blocked, "config.yaml")
	recorder := runConfigListRequest(handler.PatchAuthFileStatus, http.MethodPatch, "/auth-files/status", `{"name":"`+auths[0].ID+`","disabled":true}`)
	if recorder.Code != http.StatusInternalServerError {
		t.Fatalf("save failure status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	current, _ := manager.GetByID(auths[0].ID)
	if current == nil || current.Disabled || len(handler.configSnapshot().GeminiKey[0].ExcludedModels) != 0 {
		t.Fatal("failed config save changed credential status")
	}
}
