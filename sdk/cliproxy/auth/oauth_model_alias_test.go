package auth

import (
	"testing"

	internalconfig "github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
)

func TestResolveOAuthUpstreamModel_SuffixPreservation(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		aliases map[string][]internalconfig.OAuthModelAlias
		channel string
		input   string
		want    string
	}{
		{
			name: "numeric suffix preserved",
			aliases: map[string][]internalconfig.OAuthModelAlias{
				"gemini-cli": {{Name: "gemini-2.5-pro-exp-03-25", Alias: "gemini-2.5-pro"}},
			},
			channel: "gemini-cli",
			input:   "gemini-2.5-pro(8192)",
			want:    "gemini-2.5-pro-exp-03-25(8192)",
		},
		{
			name: "level suffix preserved",
			aliases: map[string][]internalconfig.OAuthModelAlias{
				"claude": {{Name: "claude-sonnet-4-5-20250514", Alias: "claude-sonnet-4-5"}},
			},
			channel: "claude",
			input:   "claude-sonnet-4-5(high)",
			want:    "claude-sonnet-4-5-20250514(high)",
		},
		{
			name: "no suffix unchanged",
			aliases: map[string][]internalconfig.OAuthModelAlias{
				"gemini-cli": {{Name: "gemini-2.5-pro-exp-03-25", Alias: "gemini-2.5-pro"}},
			},
			channel: "gemini-cli",
			input:   "gemini-2.5-pro",
			want:    "gemini-2.5-pro-exp-03-25",
		},
		{
			name: "config suffix takes priority",
			aliases: map[string][]internalconfig.OAuthModelAlias{
				"claude": {{Name: "claude-sonnet-4-5-20250514(low)", Alias: "claude-sonnet-4-5"}},
			},
			channel: "claude",
			input:   "claude-sonnet-4-5(high)",
			want:    "claude-sonnet-4-5-20250514(low)",
		},
		{
			name: "auto suffix preserved",
			aliases: map[string][]internalconfig.OAuthModelAlias{
				"gemini-cli": {{Name: "gemini-2.5-pro-exp-03-25", Alias: "gemini-2.5-pro"}},
			},
			channel: "gemini-cli",
			input:   "gemini-2.5-pro(auto)",
			want:    "gemini-2.5-pro-exp-03-25(auto)",
		},
		{
			name: "none suffix preserved",
			aliases: map[string][]internalconfig.OAuthModelAlias{
				"gemini-cli": {{Name: "gemini-2.5-pro-exp-03-25", Alias: "gemini-2.5-pro"}},
			},
			channel: "gemini-cli",
			input:   "gemini-2.5-pro(none)",
			want:    "gemini-2.5-pro-exp-03-25(none)",
		},
		{
			name: "kimi suffix preserved",
			aliases: map[string][]internalconfig.OAuthModelAlias{
				"kimi": {{Name: "kimi-k2.5", Alias: "k2.5"}},
			},
			channel: "kimi",
			input:   "k2.5(high)",
			want:    "kimi-k2.5(high)",
		},
		{
			name: "meta oauth alias preserves suffix",
			aliases: map[string][]internalconfig.OAuthModelAlias{
				"meta": {{Name: "muse-spark-1.3", Alias: "muse-latest"}},
			},
			channel: "meta",
			input:   "muse-latest(high)",
			want:    "muse-spark-1.3(high)",
		},
		{
			name: "case insensitive alias lookup with suffix",
			aliases: map[string][]internalconfig.OAuthModelAlias{
				"gemini-cli": {{Name: "gemini-2.5-pro-exp-03-25", Alias: "Gemini-2.5-Pro"}},
			},
			channel: "gemini-cli",
			input:   "gemini-2.5-pro(high)",
			want:    "gemini-2.5-pro-exp-03-25(high)",
		},
		{
			name: "1m tag preserved",
			aliases: map[string][]internalconfig.OAuthModelAlias{
				"claude": {{Name: "claude-sonnet-4-5-20250514", Alias: "claude-sonnet-4-5"}},
			},
			channel: "claude",
			input:   "claude-sonnet-4-5[1m]",
			want:    "claude-sonnet-4-5-20250514[1m]",
		},
		{
			name: "1m tag with suffix preserved",
			aliases: map[string][]internalconfig.OAuthModelAlias{
				"claude": {{Name: "claude-sonnet-4-5-20250514", Alias: "claude-sonnet-4-5"}},
			},
			channel: "claude",
			input:   "claude-sonnet-4-5[1m](high)",
			want:    "claude-sonnet-4-5-20250514[1m](high)",
		},
		{
			name: "no alias returns empty",
			aliases: map[string][]internalconfig.OAuthModelAlias{
				"gemini-cli": {{Name: "gemini-2.5-pro-exp-03-25", Alias: "gemini-2.5-pro"}},
			},
			channel: "gemini-cli",
			input:   "unknown-model(high)",
			want:    "",
		},
		{
			name: "wrong channel returns empty",
			aliases: map[string][]internalconfig.OAuthModelAlias{
				"gemini-cli": {{Name: "gemini-2.5-pro-exp-03-25", Alias: "gemini-2.5-pro"}},
			},
			channel: "claude",
			input:   "gemini-2.5-pro(high)",
			want:    "",
		},
		{
			name: "empty suffix filtered out",
			aliases: map[string][]internalconfig.OAuthModelAlias{
				"gemini-cli": {{Name: "gemini-2.5-pro-exp-03-25", Alias: "gemini-2.5-pro"}},
			},
			channel: "gemini-cli",
			input:   "gemini-2.5-pro()",
			want:    "gemini-2.5-pro-exp-03-25",
		},
		{
			name: "incomplete suffix treated as no suffix",
			aliases: map[string][]internalconfig.OAuthModelAlias{
				"gemini-cli": {{Name: "gemini-2.5-pro-exp-03-25", Alias: "gemini-2.5-pro(high"}},
			},
			channel: "gemini-cli",
			input:   "gemini-2.5-pro(high",
			want:    "gemini-2.5-pro-exp-03-25",
		},
	}

	for _, tt := range tests {
		t.Run(
			tt.name, func(t *testing.T) {
				t.Parallel()

				mgr := NewManager(nil, nil, nil)
				mgr.SetConfig(&internalconfig.Config{})
				mgr.SetOAuthModelAlias(tt.aliases)

				auth := createAuthForChannel(tt.channel)
				got := mgr.resolveOAuthUpstreamModel(auth, tt.input)
				if got != tt.want {
					t.Errorf("resolveOAuthUpstreamModel(%q) = %q, want %q", tt.input, got, tt.want)
				}
			},
		)
	}
}

func createAuthForChannel(channel string) *Auth {
	switch channel {
	case "gemini-cli":
		return &Auth{Provider: "gemini-cli"}
	case "claude":
		return &Auth{Provider: "claude", Attributes: map[string]string{"auth_kind": "oauth"}}
	case "vertex":
		return &Auth{Provider: "vertex", Attributes: map[string]string{"auth_kind": "oauth"}}
	case "codex":
		return &Auth{Provider: "codex", Attributes: map[string]string{"auth_kind": "oauth"}}
	case "aistudio":
		return &Auth{Provider: "aistudio"}
	case "antigravity":
		return &Auth{Provider: "antigravity"}
	case "qwen":
		return &Auth{Provider: "qwen"}
	case "iflow":
		return &Auth{Provider: "iflow"}
	case "kimi":
		return &Auth{Provider: "kimi"}
	default:
		return &Auth{Provider: channel}
	}
}

func TestOAuthModelAliasChannel_Kimi(t *testing.T) {
	t.Parallel()

	if got := OAuthModelAliasChannel("kimi", "oauth"); got != "kimi" {
		t.Fatalf("OAuthModelAliasChannel() = %q, want %q", got, "kimi")
	}
}

func TestOAuthModelAliasChannel_Meta(t *testing.T) {
	t.Parallel()

	if got := OAuthModelAliasChannel("meta", "oauth"); got != "meta" {
		t.Fatalf("Meta OAuth channel = %q, want meta", got)
	}
	if got := OAuthModelAliasChannel("meta", "apikey"); got != "" {
		t.Fatalf("Meta API key channel = %q, want empty", got)
	}
}

func TestApplyOAuthModelAliasMetaOAuthWithMintedAPIKey(t *testing.T) {
	manager := NewManager(nil, nil, nil)
	manager.SetConfig(&internalconfig.Config{})
	manager.SetOAuthModelAlias(map[string][]internalconfig.OAuthModelAlias{
		"meta": {{Name: "muse-spark-1.3", Alias: "muse-latest", ForceMapping: true}},
	})
	auth := &Auth{Provider: "meta", Attributes: map[string]string{
		"auth_kind": "oauth", "api_key": "LLM|fixture", "dca_token": "dca:fixture",
	}}
	result := manager.applyOAuthModelAliasWithResult(auth, "muse-latest(max)")
	if result.UpstreamModel != "muse-spark-1.3(max)" || !result.ForceMapping || result.OriginalAlias != "muse-latest" {
		t.Fatalf("Meta OAuth alias = %#v", result)
	}
}

func TestApplyOAuthModelAlias_SuffixPreservation(t *testing.T) {
	t.Parallel()

	aliases := map[string][]internalconfig.OAuthModelAlias{
		"gemini-cli": {{Name: "gemini-2.5-pro-exp-03-25", Alias: "gemini-2.5-pro"}},
	}

	mgr := NewManager(nil, nil, nil)
	mgr.SetConfig(&internalconfig.Config{})
	mgr.SetOAuthModelAlias(aliases)

	auth := &Auth{ID: "test-auth-id", Provider: "gemini-cli"}

	resolvedModel := mgr.applyOAuthModelAlias(auth, "gemini-2.5-pro(8192)")
	if resolvedModel != "gemini-2.5-pro-exp-03-25(8192)" {
		t.Errorf("applyOAuthModelAlias() model = %q, want %q", resolvedModel, "gemini-2.5-pro-exp-03-25(8192)")
	}
}

func TestApplyOAuthModelAlias_PluginProvider(t *testing.T) {
	t.Parallel()

	aliases := map[string][]internalconfig.OAuthModelAlias{
		"sample-provider": {{Name: "sample-model-latest", Alias: "sample-latest"}},
	}

	mgr := NewManager(nil, nil, nil)
	mgr.SetConfig(&internalconfig.Config{})
	mgr.SetOAuthModelAlias(aliases)

	auth := &Auth{
		ID: "sample-provider-auth", Provider: "sample-provider", Attributes: map[string]string{"auth_kind": "oauth"},
	}

	resolvedModel := mgr.applyOAuthModelAlias(auth, "sample-latest")
	if resolvedModel != "sample-model-latest" {
		t.Errorf("applyOAuthModelAlias() model = %q, want %q", resolvedModel, "sample-model-latest")
	}
}

func TestApplyOAuthModelAlias_PluginProviderSkipsAPIKey(t *testing.T) {
	t.Parallel()

	aliases := map[string][]internalconfig.OAuthModelAlias{
		"sample-provider": {{Name: "sample-model-latest", Alias: "sample-latest"}},
	}

	mgr := NewManager(nil, nil, nil)
	mgr.SetConfig(&internalconfig.Config{})
	mgr.SetOAuthModelAlias(aliases)

	auth := &Auth{
		ID: "sample-provider-auth", Provider: "sample-provider", Attributes: map[string]string{"auth_kind": "api_key"},
	}

	resolvedModel := mgr.applyOAuthModelAlias(auth, "sample-latest")
	if resolvedModel != "sample-latest" {
		t.Errorf("applyOAuthModelAlias() model = %q, want %q", resolvedModel, "sample-latest")
	}
}

func TestApplyOAuthModelAliasWithResult_ForceMappingUsesConfigAliasNotRequestSuffix(t *testing.T) {
	t.Parallel()

	mgr := NewManager(nil, nil, nil)
	mgr.SetOAuthModelAlias(
		map[string][]internalconfig.OAuthModelAlias{
			"codex": {
				{
					Name: "gpt-5.4", Alias: "gpt-5.4-fast", Fork: true, ForceMapping: true,
				},
			},
		},
	)
	auth := &Auth{ID: "test-auth-id", Provider: "codex"}

	res := mgr.applyOAuthModelAliasWithResult(auth, "gpt-5.4-fast(high)")
	if res.UpstreamModel != "gpt-5.4(high)" {
		t.Fatalf("upstream = %q want gpt-5.4(high)", res.UpstreamModel)
	}
	if res.OriginalAlias != "gpt-5.4-fast" {
		t.Fatalf("OriginalAlias = %q want gpt-5.4-fast", res.OriginalAlias)
	}
}

func TestApplyOAuthModelAliasWithResult_NoForceMappingPreservesRequestedModelInOriginalAlias(t *testing.T) {
	t.Parallel()

	mgr := NewManager(nil, nil, nil)
	mgr.SetOAuthModelAlias(
		map[string][]internalconfig.OAuthModelAlias{
			"codex": {
				{
					Name: "gpt-5.4", Alias: "gpt-5.4-fast", Fork: true,
				},
			},
		},
	)
	auth := &Auth{ID: "test-auth-id", Provider: "codex"}

	res := mgr.applyOAuthModelAliasWithResult(auth, "gpt-5.4-fast(high)")
	if res.ForceMapping {
		t.Fatal("expected ForceMapping false")
	}
	if res.OriginalAlias != "gpt-5.4-fast(high)" {
		t.Fatalf("OriginalAlias = %q want requested model when force-mapping off", res.OriginalAlias)
	}
}

func TestCredentialOAuthAliasesOverrideGlobalAndClear(t *testing.T) {
	mgr := NewManager(nil, nil, nil)
	mgr.SetOAuthModelAlias(map[string][]internalconfig.OAuthModelAlias{
		"codex": {{Name: "global-model", Alias: "shared"}},
	})
	auth := &Auth{Provider: "codex", Attributes: map[string]string{"auth_kind": "oauth"}}
	if err := SetOAuthModelAliasesAttribute(auth, []internalconfig.OAuthModelAlias{
		{Name: "credential-model", Alias: "shared", Fork: true, ForceMapping: true},
	}); err != nil {
		t.Fatal(err)
	}
	result := mgr.applyOAuthModelAliasWithResult(auth, "shared(high)")
	if result.UpstreamModel != "credential-model(high)" || !result.ForceMapping || result.OriginalAlias != "shared" {
		t.Fatalf("credential alias result = %#v", result)
	}
	if err := SetOAuthModelAliasesAttribute(auth, nil); err != nil {
		t.Fatal(err)
	}
	if _, exists := auth.Attributes[oauthModelAliasesAttributeKey]; exists {
		t.Fatal("empty aliases did not clear the credential attribute")
	}
	if resolved := mgr.applyOAuthModelAlias(auth, "shared(high)"); resolved != "global-model(high)" {
		t.Fatalf("global alias after clear = %q", resolved)
	}
	auth.Attributes["auth_kind"] = "api_key"
	if resolved := mgr.applyOAuthModelAlias(auth, "shared(high)"); resolved != "shared(high)" {
		t.Fatalf("API key used OAuth alias: %q", resolved)
	}

	fileAuth := &Auth{Provider: "codex", Metadata: map[string]any{
		"api_key": "synthetic-key", "access_token": "synthetic-token",
	}}
	if fileAuth.AuthKind() != AuthKindAPIKey {
		t.Fatalf("file API key classified as %q", fileAuth.AuthKind())
	}
	if err := SetOAuthModelAliasesAttribute(fileAuth, []internalconfig.OAuthModelAlias{
		{Name: "credential-model", Alias: "shared"},
	}); err != nil {
		t.Fatal(err)
	}
	if resolved := mgr.applyOAuthModelAlias(fileAuth, "shared(high)"); resolved != "shared(high)" {
		t.Fatalf("file API key used OAuth alias: %q", resolved)
	}
	fileAuth.Metadata["auth_kind"] = "oauth"
	if fileAuth.AuthKind() != AuthKindOAuth {
		t.Fatalf("explicit OAuth file classified as %q", fileAuth.AuthKind())
	}
	if resolved := mgr.applyOAuthModelAlias(fileAuth, "shared(high)"); resolved != "credential-model(high)" {
		t.Fatalf("explicit OAuth file missed credential alias: %q", resolved)
	}
}

func TestOAuthModelAliasesFromMetadataRejectsMalformedRows(t *testing.T) {
	for _, tc := range []struct {
		name  string
		value any
	}{
		{name: "not an array", value: map[string]any{"name": "model"}},
		{name: "missing alias", value: []any{map[string]any{"name": "model"}}},
		{name: "duplicate alias", value: []any{
			map[string]any{"name": "a", "alias": "shared"},
			map[string]any{"name": "b", "alias": "SHARED"},
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := OAuthModelAliasesFromMetadata(map[string]any{"model_aliases": tc.value}); err == nil {
				t.Fatal("invalid aliases accepted")
			}
		})
	}
	if aliases, err := OAuthModelAliasesFromMetadata(map[string]any{"model_aliases": []any{}}); err != nil || len(aliases) != 0 {
		t.Fatalf("empty aliases = %#v error = %v", aliases, err)
	}
}

func TestModelAliasExactSuffixPrecedence(t *testing.T) {
	aliases := []internalconfig.OAuthModelAlias{
		{Name: "base-upstream", Alias: "public"},
		{Name: "low-upstream", Alias: "public(low)", ForceMapping: true},
	}
	for _, tc := range []struct {
		name, requested, upstream, responseAlias string
		forceMapping                             bool
	}{
		{name: "exact", requested: "public(low)", upstream: "low-upstream(low)", responseAlias: "public(low)", forceMapping: true},
		{name: "case insensitive", requested: "PUBLIC(low)", upstream: "low-upstream(low)", responseAlias: "public(low)", forceMapping: true},
		{name: "base fallback", requested: "public(high)", upstream: "base-upstream(high)", responseAlias: "public(high)"},
		{name: "base", requested: "public", upstream: "base-upstream", responseAlias: "public"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			manager := NewManager(nil, nil, nil)
			manager.SetOAuthModelAlias(map[string][]internalconfig.OAuthModelAlias{"codex": aliases})
			oauth := &Auth{Provider: "codex", Attributes: map[string]string{"auth_kind": "oauth"}}
			check := func(t *testing.T, result OAuthModelAliasResult) {
				t.Helper()
				if result.UpstreamModel != tc.upstream || result.ForceMapping != tc.forceMapping || result.OriginalAlias != tc.responseAlias {
					t.Fatalf("alias result = %+v, want upstream=%q force=%t response=%q", result, tc.upstream, tc.forceMapping, tc.responseAlias)
				}
			}
			t.Run("global OAuth", func(t *testing.T) {
				check(t, manager.applyOAuthModelAliasWithResult(oauth, tc.requested))
			})
			if err := SetOAuthModelAliasesAttribute(oauth, aliases); err != nil {
				t.Fatal(err)
			}
			t.Run("credential OAuth", func(t *testing.T) {
				check(t, manager.applyOAuthModelAliasWithResult(oauth, tc.requested))
			})
			manager.SetConfig(&internalconfig.Config{CodexKey: []internalconfig.CodexKey{{
				APIKey: "fixture-exact-alias",
				Models: []internalconfig.CodexModel{
					{Name: "base-upstream", Alias: "public"},
					{Name: "low-upstream", Alias: "public(low)", ForceMapping: true},
				},
			}}})
			keyAuth := &Auth{ID: "exact-alias-key", Provider: "codex", Attributes: map[string]string{
				"auth_kind": "apikey", "api_key": "fixture-exact-alias",
			}}
			if _, err := manager.Register(t.Context(), keyAuth); err != nil {
				t.Fatal(err)
			}
			t.Run("API key lookup", func(t *testing.T) {
				if resolved := manager.lookupAPIKeyUpstreamModel(keyAuth.ID, tc.requested); resolved != tc.upstream {
					t.Fatalf("lookup model = %q, want %q", resolved, tc.upstream)
				}
			})
			t.Run("API key result", func(t *testing.T) {
				check(t, manager.resolveAPIKeyModelAliasWithResult(keyAuth, tc.requested))
			})
		})
	}
}
