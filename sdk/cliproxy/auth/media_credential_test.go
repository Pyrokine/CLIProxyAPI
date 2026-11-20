package auth

import (
	"context"
	"testing"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executionregistry"
	cliproxyexecutor "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
)

func TestMediaAPIKeyEligibilityIntersectsExistingConstraints(t *testing.T) {
	apiKey := &Auth{
		ID: "media-key", Provider: "codex",
		Attributes: map[string]string{AttributeAuthKind: AuthKindAPIKey, AttributeAPIKey: "synthetic-key"},
	}
	oauth := &Auth{
		ID: "media-oauth", Provider: "codex", Attributes: map[string]string{AttributeAuthKind: AuthKindOAuth},
		Metadata: map[string]any{"access_token": "synthetic-oauth"},
	}
	opts := cliproxyexecutor.Options{Metadata: map[string]any{cliproxyexecutor.RequireAPIKeyAuthMetadataKey: true}}
	for _, test := range []struct {
		name       string
		ctx        context.Context
		key, oauth bool
	}{
		{"media only", t.Context(), true, false},
		{"explicit api key", withRequiredAuthKind(t.Context(), AuthKindAPIKey), true, false},
		{"explicit oauth", withRequiredAuthKind(t.Context(), AuthKindOAuth), false, false},
		{"credential policy", withCredentialPolicy(t.Context(), CredentialPolicyCodexAlphaSearchV1), false, false},
	} {
		t.Run(
			test.name, func(t *testing.T) {
				eligibility := authSelectionEligibilityForRequest(test.ctx, opts)
				if eligibility.unrestricted() || eligibility.allows(apiKey) != test.key || eligibility.allows(oauth) != test.oauth {
					t.Fatalf("media eligibility key=%t oauth=%t", eligibility.allows(apiKey), eligibility.allows(oauth))
				}
			},
		)
	}
	ordinary := authSelectionEligibilityForRequest(t.Context(), cliproxyexecutor.Options{})
	if !ordinary.unrestricted() || !ordinary.allows(oauth) {
		t.Fatal("ordinary OAuth eligibility changed")
	}
}

func TestMediaAPIKeySelectionPreservesPinnedCredentials(t *testing.T) {
	manager := NewManager(nil, &RoundRobinSelector{}, nil)
	manager.RegisterExecutor(&replaceAwareExecutor{id: "codex"})
	for _, auth := range []*Auth{
		{
			ID: "media-oauth", Provider: "codex",
			Attributes: map[string]string{AttributeAuthKind: AuthKindOAuth, "priority": "10"},
			Metadata:   map[string]any{"access_token": "synthetic-oauth"},
		},
		{ID: "media-key", Provider: "codex", Attributes: map[string]string{AttributeAPIKey: "synthetic-key"}},
	} {
		if _, err := manager.Register(t.Context(), auth); err != nil {
			t.Fatal(err)
		}
	}
	for _, pinned := range []string{"", "media-key", "media-oauth"} {
		opts := cliproxyexecutor.Options{
			Metadata: map[string]any{
				cliproxyexecutor.RequireAPIKeyAuthMetadataKey: true, cliproxyexecutor.PinnedAuthMetadataKey: pinned,
			},
		}
		selected, err := manager.SelectAuth(t.Context(), "codex", "", opts)
		if pinned == "media-oauth" {
			if err == nil || selected != nil {
				t.Fatalf("pinned OAuth escaped media eligibility: auth=%v error=%v", selected, err)
			}
			continue
		}
		if err != nil || selected == nil || selected.ID != "media-key" {
			t.Fatalf("pinned=%q auth=%v error=%v", pinned, selected, err)
		}
	}
}

func TestMediaAPIKeyHomeSelectionSkipsOAuthWithoutSending(t *testing.T) {
	for _, pinned := range []string{"", "media-oauth"} {
		dispatcher := &authKindHomeDispatcher{
			auths: []Auth{
				{ID: "media-oauth", Provider: "test", Metadata: map[string]any{"access_token": "synthetic-oauth"}},
				{ID: "media-key", Provider: "test", Attributes: map[string]string{AttributeAPIKey: "synthetic-key"}},
			},
		}
		manager := NewManager(nil, nil, nil)
		manager.SetConfig(&config.Config{Home: config.HomeConfig{Enabled: true}})
		manager.PublishHomeDispatch(dispatcher, executionregistry.New(), 1)
		manager.RegisterExecutor(schedulerTestExecutor{})
		opts := cliproxyexecutor.Options{
			Metadata: map[string]any{
				cliproxyexecutor.RequireAPIKeyAuthMetadataKey: true, cliproxyexecutor.PinnedAuthMetadataKey: pinned,
			},
		}
		selection, err := manager.selectHomeAuthForExecution(t.Context(), "synthetic-model", opts)
		if pinned != "" {
			if err == nil || selection != nil {
				t.Fatalf("pinned Home OAuth escaped media eligibility: selection=%v error=%v", selection, err)
			}
			continue
		}
		if err != nil {
			t.Fatal(err)
		}
		defer selection.End("test_complete")
		if selection.CloneAuth().ID != "media-key" || len(dispatcher.counts) != 2 {
			t.Fatalf("Home media selection=%v counts=%v", selection.CloneAuth(), dispatcher.counts)
		}
	}
}
