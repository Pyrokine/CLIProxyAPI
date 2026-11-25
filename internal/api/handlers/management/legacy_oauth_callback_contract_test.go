package management

import (
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

func TestPublishedOAuthCallbackProviderHelpers(t *testing.T) {
	for _, tc := range []struct {
		name, provider, want string
		normalize            func(string) (string, error)
	}{
		{"built-in alias", " OpenAI ", "codex", normalizeOAuthCallbackProvider},
		{"custom callback", " Gemini-CLI ", "gemini-cli", normalizeOAuthCallbackProvider},
		{"plugin alias", " OpenAI ", "openai", normalizePluginOAuthCallbackProvider},
		{"custom plugin", " Gemini-CLI ", "gemini-cli", normalizePluginOAuthCallbackProvider},
	} {
		t.Run(
			tc.name, func(t *testing.T) {
				if got, err := tc.normalize(tc.provider); err != nil || got != tc.want {
					t.Fatalf("provider = %q, error = %v, want %q", got, err, tc.want)
				}
			},
		)
	}
	for _, provider := range []string{"", "../provider", "custom_sso", "custom.sso"} {
		for _, normalize := range []func(string) (string, error){
			normalizeOAuthCallbackProvider, normalizePluginOAuthCallbackProvider,
		} {
			if _, err := normalize(provider); err == nil {
				t.Fatalf("invalid callback provider accepted: %q", provider)
			}
		}
	}
}

func TestLegacyOAuthCompletionKeepsPluginSourceAndCancelsBuiltinWork(t *testing.T) {
	const provider = "sdk-compat-provider"
	var cancelled atomic.Int32
	for _, state := range []string{"sdk-compat-batch-a", "sdk-compat-batch-b"} {
		registerOAuthSession(state, provider, "")
		if !setOAuthSessionCancel(state, provider, func() { cancelled.Add(1) }) {
			t.Fatal("session cancellation was not registered")
		}
		t.Cleanup(func() { completeOAuthSession(state) })
	}
	const pluginState = "sdk-compat-plugin-session"
	if err := registerPluginOAuthSession(
		pluginState, provider, "nonce", t.TempDir(), map[string]any{"fixture": true},
	); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { completeOAuthSession(pluginState) })
	if count := CompleteOAuthSessionsByProvider(provider); count != 2 || cancelled.Load() != 2 {
		t.Fatalf("completed=%d cancelled=%d", count, cancelled.Load())
	}
	if !isOAuthSessionPending(pluginState, provider) {
		t.Fatal("built-in completion changed a plugin session")
	}
	if count := CompleteOAuthSessionsByProvider(provider); count != 0 {
		t.Fatalf("repeated completion=%d", count)
	}
}

func TestLegacyOAuthCallbackKeepsCapturedDirectoryAndBindsLegacyDirectory(t *testing.T) {
	dir, otherDir := t.TempDir(), t.TempDir()
	state := "sdk-compat-bound-directory"
	registerOAuthSessionForAuthDir(state, "codex", "", dir)
	t.Cleanup(func() { completeOAuthSession(state) })
	if _, err := WriteOAuthCallbackFileForPendingSession("codex", state, "code", "", otherDir); err == nil {
		t.Fatal("callback redirected a session to another directory")
	}
	path, err := WriteOAuthCallbackFileForPendingSession("codex", state, "code", "")
	if err != nil || path != filepath.Join(dir, ".oauth-codex-"+state+".oauth") {
		t.Fatalf("captured directory callback=%q error=%v", path, err)
	}
	legacyState := "sdk-compat-legacy-directory"
	RegisterOAuthSession(legacyState, "codex")
	t.Cleanup(func() { completeOAuthSession(legacyState) })
	blockedDirectory := filepath.Join(t.TempDir(), "not-a-directory")
	if err := os.WriteFile(blockedDirectory, []byte("blocked"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := WriteOAuthCallbackFileForPendingSession(
		"codex", legacyState, "code", "", blockedDirectory,
	); err == nil {
		t.Fatal("callback file creation failure was hidden")
	}
	path, err = WriteOAuthCallbackFileForPendingSession("codex", legacyState, "code", "", otherDir)
	if err != nil || path != filepath.Join(otherDir, ".oauth-codex-"+legacyState+".oauth") {
		t.Fatalf("legacy directory callback=%q error=%v", path, err)
	}
	session, ok := oauthSessions.get(legacyState)
	if !ok || session.AuthDir != otherDir {
		t.Fatalf("legacy directory was not captured: %+v", session)
	}
}

func TestOAuthCallbackClaimRejectsTerminalSessions(t *testing.T) {
	store := newOAuthSessionStore(time.Minute)
	const state = "sdk-compat-terminal"
	store.register(state, "codex", "", t.TempDir(), [32]byte{})
	if _, _, claimed := store.claimCallback(state, "codex", nil); !claimed {
		t.Fatal("first callback was not claimed")
	}
	store.complete(state)
	if store.callbackClaimActive(state, "codex") {
		t.Fatal("completed session retained an active callback claim")
	}
	store.register(state, "codex", "", t.TempDir(), [32]byte{})
	if _, _, claimed := store.claimCallback(state, "codex", nil); !claimed {
		t.Fatal("second callback was not claimed")
	}
	store.setError(state, "synthetic failure")
	if store.callbackClaimActive(state, "codex") {
		t.Fatal("failed session retained an active callback claim")
	}
}
