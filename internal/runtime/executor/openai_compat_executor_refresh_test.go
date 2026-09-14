package executor

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	cliproxyauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
)

func TestOpenAICompatExecutorRefreshNoProviderBehavior(t *testing.T) {
	executor := NewOpenAICompatExecutor("openai-compatible-custom", &config.Config{})

	t.Run(
		"api key remains no-op", func(t *testing.T) {
			auth := &cliproxyauth.Auth{
				ID:       "auth-1",
				Provider: "custom-provider",
				Attributes: map[string]string{
					"api_key": "sk-test",
				},
			}
			refreshed, errRefresh := executor.Refresh(context.Background(), auth)
			if errRefresh != nil {
				t.Fatalf("Refresh() error = %v", errRefresh)
			}
			if refreshed != auth {
				t.Fatalf("Refresh() auth = %p, want original %p", refreshed, auth)
			}
		},
	)

	t.Run(
		"oauth reports missing owner", func(t *testing.T) {
			auth := &cliproxyauth.Auth{
				ID:       "auth-1",
				Provider: "custom-provider",
				Metadata: map[string]any{
					"access_token":  "old-token",
					"refresh_token": "refresh-1",
				},
			}
			refreshed, errRefresh := executor.Refresh(context.Background(), auth)
			if errRefresh == nil || refreshed != nil {
				t.Fatalf("Refresh() = (%#v, %v), want missing owner error", refreshed, errRefresh)
			}
			if !strings.Contains(errRefresh.Error(), "cannot refresh oauth credentials") {
				t.Fatalf("Refresh() error = %v", errRefresh)
			}
		},
	)
}

func TestOpenAICompatExecutorRefreshPrefersHome(t *testing.T) {
	updatedAuth := cliproxyauth.Auth{
		Provider: "custom-provider",
		Metadata: map[string]any{
			"access_token":  "home-token",
			"refresh_token": "refresh-2",
		},
	}
	payload, errMarshal := json.Marshal(
		homeRefreshAuthEnvelope{
			Auth:      updatedAuth,
			AuthIndex: "returned-index",
		},
	)
	if errMarshal != nil {
		t.Fatalf("Marshal() error = %v", errMarshal)
	}
	client := &testHomeRefreshClient{heartbeat: true, payload: payload}
	ctx := cliproxyauth.WithHomeRefreshClient(context.Background(), client)
	executor := NewOpenAICompatExecutor(
		"openai-compatible-custom",
		&config.Config{Home: config.HomeConfig{Enabled: true}},
	)
	auth := &cliproxyauth.Auth{
		ID:       "auth-1",
		Index:    "requested-index",
		Provider: "custom-provider",
		Metadata: map[string]any{
			"access_token":  "old-token",
			"refresh_token": "refresh-1",
		},
	}

	refreshed, errRefresh := executor.Refresh(ctx, auth)
	if errRefresh != nil {
		t.Fatalf("Refresh() error = %v", errRefresh)
	}
	if client.index != "requested-index" {
		t.Fatalf("Home auth index = %q", client.index)
	}
	if refreshed == nil || refreshed.ID != "auth-1" || refreshed.Index != "returned-index" ||
		refreshed.Metadata["access_token"] != "home-token" {
		t.Fatalf("Refresh() auth = %#v", refreshed)
	}
}
