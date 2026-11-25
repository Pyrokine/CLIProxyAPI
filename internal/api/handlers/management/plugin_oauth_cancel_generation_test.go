package management

import (
	"context"
	"testing"
	"time"

	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/pluginapi"
)

// noinspection GoDirectComparisonOfErrors
func TestPluginOAuthCancelAttachmentRejectsStaleWorkerWithoutCancellingReplacement(t *testing.T) {
	sessions := newOAuthSessionStore(time.Minute)
	const state, provider = "reused-state", "custom-sso"
	var old oauthSession
	if err := sessions.registerPlugin(state, provider, "old-nonce", t.TempDir(), nil, [32]byte{}, &old); err != nil {
		t.Fatal(err)
	}
	oldCtx, oldCancel := context.WithCancel(context.Background())
	defer oldCancel()
	if !sessions.cancel(state) {
		t.Fatal("old session was not cancelled before attaching its worker")
	}
	if err := sessions.registerPlugin(state, provider, "new-nonce", t.TempDir(), nil, [32]byte{}); err != nil {
		t.Fatal(err)
	}
	current, _ := sessions.get(state)
	currentCtx, currentCancel := context.WithDeadline(context.Background(), current.ExpiresAt)
	defer currentCancel()
	if !sessions.attachCancel(state, provider, currentCancel, current.CreatedAt) {
		t.Fatal("replacement worker cancellation was not attached")
	}
	entered, release := make(chan struct{}), make(chan struct{})
	host := &managementPluginOAuthHost{
		provider: provider,
		poll: func(ctx context.Context, _, _ string, _ map[string]any) (pluginapi.AuthLoginPollResponse, error) {
			close(entered)
			select {
			case <-ctx.Done():
				return pluginapi.AuthLoginPollResponse{}, ctx.Err()
			case <-release:
				return pluginapi.AuthLoginPollResponse{
					Status: pluginapi.AuthLoginStatusPending, Message: "replacement worker remains active",
				}, nil
			}
		},
	}
	h := &Handler{pluginHost: host}
	done := make(chan bool, 1)
	go func() { done <- h.pollPluginOAuthSession(currentCtx, state, current, sessions) }()
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("replacement worker did not start")
	}
	if sessions.attachCancel(state, provider, oldCancel, old.CreatedAt) {
		t.Error("stale worker cancellation attached to the replacement session")
	}
	if oldCtx.Err() != context.Canceled {
		t.Error("rejected stale worker cancellation was not invoked")
	}
	if currentCtx.Err() != nil {
		t.Error("stale attachment cancelled the replacement worker")
	}
	close(release)
	select {
	case pending := <-done:
		if !pending {
			t.Error("replacement worker stopped after the stale attachment")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("replacement worker did not finish polling")
	}
	fresh, exists := sessions.get(state)
	if !exists || !fresh.CreatedAt.Equal(current.CreatedAt) || fresh.NonceHash != current.NonceHash ||
		fresh.Status != "" || fresh.Completed || fresh.Polling || fresh.Message != "replacement worker remains active" {
		t.Errorf("replacement session changed after stale attachment: %+v", fresh)
	}
	if !sessions.cancel(state) || currentCtx.Err() != context.Canceled {
		t.Error("replacement session did not retain its original cancellation handle")
	}
}

// noinspection GoDirectComparisonOfErrors
func TestPluginOAuthRegistrationReturnsOriginalGenerationSnapshot(t *testing.T) {
	sessions := newOAuthSessionStore(time.Minute)
	const state, provider = "registration-snapshot", "custom-sso"
	dir := t.TempDir()
	var registered oauthSession
	if err := sessions.registerPlugin(
		state, provider, "old-nonce", dir,
		map[string]any{"nested": map[string]any{"generation": "old"}}, [32]byte{}, &registered,
	); err != nil {
		t.Fatal(err)
	}
	registered.Metadata["nested"].(map[string]any)["generation"] = "changed"
	stored, _ := sessions.get(state)
	if registered.CreatedAt.IsZero() || !registered.CreatedAt.Equal(stored.CreatedAt) ||
		stored.Metadata["nested"].(map[string]any)["generation"] != "old" {
		t.Fatal("registration snapshot changed the original stored generation")
	}
	if !sessions.cancel(state) {
		t.Fatal("original generation was not cancelled")
	}
	if err := sessions.registerPlugin(state, provider, "new-nonce", t.TempDir(), nil, [32]byte{}); err != nil {
		t.Fatal(err)
	}
	replacement, _ := sessions.get(state)
	if registered.CreatedAt.Equal(replacement.CreatedAt) || registered.AuthDir != dir ||
		registered.NonceHash == replacement.NonceHash {
		t.Fatal("registration snapshot followed the replacement state")
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if !sessions.attachCancel(state, provider, cancel, replacement.CreatedAt) {
		t.Fatal("replacement cancel was not attached")
	}
	oldCtx, oldCancel := context.WithCancel(context.Background())
	defer oldCancel()
	if sessions.attachCancel(
		state, provider, oldCancel, registered.CreatedAt,
	) || oldCtx.Err() != context.Canceled || ctx.Err() != nil {
		t.Fatal("captured original registration interfered with the replacement")
	}
	if !sessions.cancel(state) || ctx.Err() != context.Canceled {
		t.Fatal("replacement lost its cancellation handle")
	}
}
