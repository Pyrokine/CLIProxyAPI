package auth

import (
	"context"
	"encoding/json"
	"testing"

	internalconfig "github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/registry"
)

func TestLookupAPIKeyUpstreamModel(t *testing.T) {
	cfg := &internalconfig.Config{
		GeminiKey: []internalconfig.GeminiKey{
			{
				APIKey:  "k",
				BaseURL: "https://example.com",
				Models: []internalconfig.GeminiModel{
					{Name: "gemini-2.5-pro-exp-03-25", Alias: "g25p"},
					{Name: "gemini-2.5-flash(low)", Alias: "g25f"},
				},
			},
		},
	}

	mgr := NewManager(nil, nil, nil)
	mgr.SetConfig(cfg)

	ctx := context.Background()
	_, _ = mgr.Register(
		ctx, &Auth{
			ID: "a1", Provider: "gemini",
			Attributes: map[string]string{"api_key": "k", "base_url": "https://example.com"},
		},
	)

	tests := []struct {
		name   string
		authID string
		input  string
		want   string
	}{
		// Fast path + suffix preservation
		{"alias with suffix", "a1", "g25p(8192)", "gemini-2.5-pro-exp-03-25(8192)"},
		{"alias without suffix", "a1", "g25p", "gemini-2.5-pro-exp-03-25"},

		// Config suffix takes priority
		{"config suffix priority", "a1", "g25f(high)", "gemini-2.5-flash(low)"},
		{"config suffix no user suffix", "a1", "g25f", "gemini-2.5-flash(low)"},

		// Case-insensitive
		{"uppercase alias", "a1", "G25P", "gemini-2.5-pro-exp-03-25"},
		{"mixed case with suffix", "a1", "G25p(4096)", "gemini-2.5-pro-exp-03-25(4096)"},

		// Direct name lookup
		{"upstream name direct", "a1", "gemini-2.5-pro-exp-03-25", "gemini-2.5-pro-exp-03-25"},
		{"upstream name with suffix", "a1", "gemini-2.5-pro-exp-03-25(8192)", "gemini-2.5-pro-exp-03-25(8192)"},
		{"alias with 1m tag", "a1", "g25p[1m]", "gemini-2.5-pro-exp-03-25[1m]"},
		{"alias with 1m tag and thinking suffix", "a1", "g25p[1m](4096)", "gemini-2.5-pro-exp-03-25[1m](4096)"},

		// Cache miss scenarios
		{"non-existent auth", "non-existent", "g25p", ""},
		{"unknown alias", "a1", "unknown-alias", ""},
		{"empty auth ID", "", "g25p", ""},
		{"empty model", "a1", "", ""},
	}

	for _, tt := range tests {
		t.Run(
			tt.name, func(t *testing.T) {
				resolved := mgr.lookupAPIKeyUpstreamModel(tt.authID, tt.input)
				if resolved != tt.want {
					t.Errorf(
						"lookupAPIKeyUpstreamModel(%q, %q) = %q, want %q",
						tt.authID,
						tt.input,
						resolved,
						tt.want,
					)
				}
			},
		)
	}
}

func TestLookupAPIKeyUpstreamModelInteractions(t *testing.T) {
	cfg := &internalconfig.Config{
		InteractionsKey: []internalconfig.GeminiKey{
			{
				APIKey: "k",
				Models: []internalconfig.GeminiModel{
					{Name: "gemini-2.5-pro-exp-03-25", Alias: "g25p"},
				},
			},
		},
	}

	mgr := NewManager(nil, nil, nil)
	mgr.SetConfig(cfg)

	ctx := context.Background()
	_, _ = mgr.Register(
		ctx, &Auth{
			ID: "interactions-auth", Provider: "gemini-interactions",
			Attributes: map[string]string{"api_key": "k"},
		},
	)

	if resolved := mgr.lookupAPIKeyUpstreamModel(
		"interactions-auth", "g25p(high)",
	); resolved != "gemini-2.5-pro-exp-03-25(high)" {
		t.Fatalf("lookupAPIKeyUpstreamModel = %q, want gemini-2.5-pro-exp-03-25(high)", resolved)
	}

	resolvedModel := mgr.applyAPIKeyModelAlias(
		&Auth{Provider: "gemini-interactions", Attributes: map[string]string{"api_key": "k"}},
		"g25p[1m]",
	)
	if resolvedModel != "gemini-2.5-pro-exp-03-25[1m]" {
		t.Fatalf("applyAPIKeyModelAlias = %q, want gemini-2.5-pro-exp-03-25[1m]", resolvedModel)
	}
}

func TestAPIKeyModelAlias_ConfigHotReload(t *testing.T) {
	cfg := &internalconfig.Config{
		GeminiKey: []internalconfig.GeminiKey{
			{
				APIKey: "k",
				Models: []internalconfig.GeminiModel{{Name: "gemini-2.5-pro-exp-03-25", Alias: "g25p"}},
			},
		},
	}

	mgr := NewManager(nil, nil, nil)
	mgr.SetConfig(cfg)

	ctx := context.Background()
	_, _ = mgr.Register(ctx, &Auth{ID: "a1", Provider: "gemini", Attributes: map[string]string{"api_key": "k"}})

	// Initial alias
	if resolved := mgr.lookupAPIKeyUpstreamModel("a1", "g25p"); resolved != "gemini-2.5-pro-exp-03-25" {
		t.Fatalf("before reload: got %q, want %q", resolved, "gemini-2.5-pro-exp-03-25")
	}

	// Hot reload with new alias
	mgr.SetConfig(
		&internalconfig.Config{
			GeminiKey: []internalconfig.GeminiKey{
				{
					APIKey: "k",
					Models: []internalconfig.GeminiModel{{Name: "gemini-2.5-flash", Alias: "g25p"}},
				},
			},
		},
	)

	// New alias should take effect
	if resolved := mgr.lookupAPIKeyUpstreamModel("a1", "g25p"); resolved != "gemini-2.5-flash" {
		t.Fatalf("after reload: got %q, want %q", resolved, "gemini-2.5-flash")
	}
}

func TestAPIKeyModelAlias_MultipleProviders(t *testing.T) {
	cfg := &internalconfig.Config{
		GeminiKey: []internalconfig.GeminiKey{
			{
				APIKey: "gemini-key", Models: []internalconfig.GeminiModel{
					{
						Name: "gemini-2.5-pro", Alias: "gp",
					},
				},
			},
		},
		ClaudeKey: []internalconfig.ClaudeKey{
			{
				APIKey: "claude-key", Models: []internalconfig.ClaudeModel{
					{
						Name: "claude-sonnet-4", Alias: "cs4",
					},
				},
			},
		},
		CodexKey: []internalconfig.CodexKey{
			{
				APIKey: "codex-key", Models: []internalconfig.CodexModel{{Name: "o3", Alias: "o"}},
			},
		},
		MetaKey: []internalconfig.MetaKey{
			{
				APIKey: "meta-key", Models: []internalconfig.CodexModel{{Name: "muse-spark-1.3", Alias: "muse-latest"}},
			},
		},
	}

	mgr := NewManager(nil, nil, nil)
	mgr.SetConfig(cfg)

	ctx := context.Background()
	_, _ = mgr.Register(
		ctx,
		&Auth{ID: "gemini-auth", Provider: "gemini", Attributes: map[string]string{"api_key": "gemini-key"}},
	)
	_, _ = mgr.Register(
		ctx,
		&Auth{ID: "claude-auth", Provider: "claude", Attributes: map[string]string{"api_key": "claude-key"}},
	)
	_, _ = mgr.Register(
		ctx, &Auth{ID: "codex-auth", Provider: "codex", Attributes: map[string]string{"api_key": "codex-key"}},
	)
	_, _ = mgr.Register(
		ctx, &Auth{ID: "meta-auth", Provider: "meta", Attributes: map[string]string{"api_key": "meta-key"}},
	)

	tests := []struct {
		authID, input, want string
	}{
		{"gemini-auth", "gp", "gemini-2.5-pro"},
		{"claude-auth", "cs4", "claude-sonnet-4"},
		{"codex-auth", "o", "o3"},
		{"meta-auth", "muse-latest", "muse-spark-1.3"},
	}

	for _, tt := range tests {
		if resolved := mgr.lookupAPIKeyUpstreamModel(tt.authID, tt.input); resolved != tt.want {
			t.Errorf("lookupAPIKeyUpstreamModel(%q, %q) = %q, want %q", tt.authID, tt.input, resolved, tt.want)
		}
	}
}

func TestResolveOpenAICompatConfig_SkipsDisabled(t *testing.T) {
	cfg := &internalconfig.Config{
		OpenAICompatibility: []internalconfig.OpenAICompatibility{
			{
				Name: "pool", Disabled: true,
				Models: []internalconfig.OpenAICompatibilityModel{{Name: "disabled-model", Alias: "alias"}},
			},
			{
				Name:   "pool",
				Models: []internalconfig.OpenAICompatibilityModel{{Name: "active-model", Alias: "alias"}},
			},
		},
	}

	entry := resolveOpenAICompatConfig(cfg, "pool", "pool", "pool")
	if entry == nil {
		t.Fatal("expected active entry, got nil")
	}
	if entry.Disabled {
		t.Fatal("expected disabled entry to be skipped")
	}
	if len(entry.Models) != 1 || entry.Models[0].Name != "active-model" {
		t.Fatalf("unexpected entry selected: %+v", entry.Models)
	}
}

func TestOriginalModelConfigSelectionKeepsPrefixAndProxy(t *testing.T) {
	for _, provider := range []struct{ name, configKey string }{
		{"claude", "claude-api-key"}, {"codex", "codex-api-key"}, {"xai", "xai-api-key"},
		{"meta", "meta-api-key"}, {"gemini", "gemini-api-key"},
		{"gemini-interactions", "interactions-api-key"}, {"vertex", "vertex-api-key"},
	} {
		for _, setting := range []string{"prefix", "proxy", "both"} {
			for _, mode := range []string{"execute", "stream"} {
				t.Run(
					provider.name+"/"+setting+"/"+mode, func(t *testing.T) {
						firstPrefix, firstProxy := "other", "direct"
						if setting == "prefix" {
							firstProxy = "none"
						}
						if setting == "proxy" {
							firstPrefix = "team"
						}
						entries := []map[string]any{
							{
								"api-key": "synthetic-shared-key", "base-url": "https://models.synthetic.invalid",
								"prefix": firstPrefix, "proxy-url": firstProxy,
								"models": []map[string]any{{"name": "first-upstream", "alias": "public-model"}},
							},
							{
								"api-key": "synthetic-shared-key", "base-url": "https://models.synthetic.invalid",
								"prefix": "team", "proxy-url": "none",
								"models": []map[string]any{
									{"name": "second-upstream", "alias": "public-model"},
								},
							},
						}
						raw, err := json.Marshal(map[string]any{provider.configKey: entries})
						if err != nil {
							t.Fatal(err)
						}
						cfg := &internalconfig.Config{}
						if err := json.Unmarshal(raw, cfg); err != nil {
							t.Fatal(err)
						}
						manager := NewManager(nil, nil, nil)
						manager.SetHomeClientProvider(func() HomeClient { return nil })
						manager.SetConfig(cfg)
						capture := &forceMappingExecutor{id: provider.name}
						manager.RegisterExecutor(capture)
						credential, err := manager.Register(
							t.Context(), &Auth{
								ID: t.Name(), Provider: provider.name, Prefix: "team", ProxyURL: "none",
								Attributes: map[string]string{
									AttributeAPIKey: "synthetic-shared-key",
									"base_url":      "https://models.synthetic.invalid",
									AttributeSource: AuthSourceFile,
								},
							},
						)
						if err != nil {
							t.Fatal(err)
						}
						registerProjectionTestClient(
							t, credential.ID, provider.name,
							&registry.ModelInfo{ID: "team/public-model"},
						)
						manager.RefreshSchedulerEntry(credential.ID)
						if cached := manager.lookupAPIKeyUpstreamModel(
							credential.ID, "public-model(high)",
						); cached != "second-upstream(high)" {
							t.Errorf("compiled alias=%s, want second-upstream(high)", cached)
						}
						if resolved := manager.ResolveExecutionModel(
							credential, "team/public-model(high)",
						); resolved != "second-upstream(high)" {
							t.Errorf("resolved target=%s, want second-upstream(high)", resolved)
						}
						originalForceMappingPayload(
							t, manager, provider.name, "team/public-model(high)", mode == "stream",
						)
						models := capture.ExecuteModels()
						if mode == "stream" {
							models = capture.StreamModels()
						}
						if len(models) != 1 || models[0] != "second-upstream(high)" {
							t.Fatalf("executed targets=%v, want one second-upstream(high)", models)
						}
					},
				)
			}
		}
	}
}

func TestOriginalModelConfigSelectionKeepsIndexAndCredentialFallback(t *testing.T) {
	entries := []internalconfig.ClaudeKey{
		{
			APIKey:   "synthetic-key",
			BaseURL:  "https://models.synthetic.invalid",
			Prefix:   "first",
			ProxyURL: "direct",
		},
		{
			APIKey:   "synthetic-key",
			BaseURL:  "https://models.synthetic.invalid",
			Prefix:   "team",
			ProxyURL: "none",
		},
	}
	for _, test := range []struct {
		name, index, key, base, prefix string
		want                           int
	}{
		{"matching settings", "", "synthetic-key", "https://models.synthetic.invalid", "team", 1},
		{"first index wins", "0", "synthetic-key", "https://models.synthetic.invalid", "team", 0},
		{"second index", "1", "synthetic-key", "https://models.synthetic.invalid", "team", 1},
		{"invalid index", "invalid", "synthetic-key", "https://models.synthetic.invalid", "team", -1},
		{"out of range index", "2", "synthetic-key", "https://models.synthetic.invalid", "team", -1},
		{"indexed credential changed", "1", "different-key", "https://models.synthetic.invalid", "team", -1},
		{"credential fallback", "", "synthetic-key", "https://models.synthetic.invalid", "unmatched", 0},
		{"base only", "", "", "https://models.synthetic.invalid", "team", 1},
		{"key only fallback", "", "synthetic-key", "https://unknown.synthetic.invalid", "team", 0},
		{"missing identity", "", "", "", "team", -1},
	} {
		t.Run(
			test.name, func(t *testing.T) {
				auth := &Auth{
					Prefix: test.prefix, ProxyURL: "none", Attributes: map[string]string{
						AttributeAPIKey: test.key, "base_url": test.base,
					},
				}
				if test.index != "" {
					auth.Attributes[AttributeConfigIndex] = test.index
				}
				got := resolveAPIKeyConfig(entries, auth)
				if test.want < 0 {
					if got != nil {
						t.Fatalf("unexpected configuration: %+v", got)
					}
				} else if got != &entries[test.want] {
					t.Fatalf("configuration=%+v, want entry %d", got, test.want)
				}
			},
		)
	}
}

func TestApplyAPIKeyModelAlias(t *testing.T) {
	cfg := &internalconfig.Config{
		GeminiKey: []internalconfig.GeminiKey{
			{
				APIKey: "k",
				Models: []internalconfig.GeminiModel{{Name: "gemini-2.5-pro-exp-03-25", Alias: "g25p"}},
			},
		},
	}

	mgr := NewManager(nil, nil, nil)
	mgr.SetConfig(cfg)

	ctx := context.Background()
	apiKeyAuth := &Auth{ID: "a1", Provider: "gemini", Attributes: map[string]string{"api_key": "k"}}
	oauthAuth := &Auth{
		ID:         "oauth-auth",
		Provider:   "gemini",
		Attributes: map[string]string{"auth_kind": "oauth"},
	}
	_, _ = mgr.Register(ctx, apiKeyAuth)

	tests := []struct {
		name       string
		auth       *Auth
		inputModel string
		wantModel  string
	}{
		{
			name:       "api_key auth with alias",
			auth:       apiKeyAuth,
			inputModel: "g25p(8192)",
			wantModel:  "gemini-2.5-pro-exp-03-25(8192)",
		},
		{
			name:       "oauth auth passthrough",
			auth:       oauthAuth,
			inputModel: "some-model",
			wantModel:  "some-model",
		},
	}

	for _, tt := range tests {
		t.Run(
			tt.name, func(t *testing.T) {
				resolvedModel := mgr.applyAPIKeyModelAlias(tt.auth, tt.inputModel)

				if resolvedModel != tt.wantModel {
					t.Errorf("model = %q, want %q", resolvedModel, tt.wantModel)
				}
			},
		)
	}
}
