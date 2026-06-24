package executor

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"testing"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	cliproxyauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
)

type testHomeRefreshClient struct {
	heartbeat bool
	payload   []byte
	err       error
	index     string
}

func (c *testHomeRefreshClient) HeartbeatOK() bool { return c.heartbeat }

func (c *testHomeRefreshClient) GetRefreshAuth(_ context.Context, authIndex string) ([]byte, error) {
	c.index = authIndex
	return c.payload, c.err
}

func TestRefreshAuthViaHomeAcceptsAuthEnvelope(t *testing.T) {
	updatedAuth := cliproxyauth.Auth{
		ID:       "auth-new",
		Provider: "codex",
		Metadata: map[string]any{"token": "new"},
	}
	payload, err := json.Marshal(homeRefreshAuthEnvelope{Auth: updatedAuth, AuthIndex: "returned-index"})
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}

	client := &testHomeRefreshClient{heartbeat: true, payload: payload}
	previous := currentHomeRefreshClient
	currentHomeRefreshClient = func() homeRefreshClient { return client }
	t.Cleanup(func() { currentHomeRefreshClient = previous })

	updated, attempted, err := refreshAuthViaHome(
		context.Background(),
		&config.Config{Home: config.HomeConfig{Enabled: true}},
		&cliproxyauth.Auth{ID: "auth-old", Provider: "codex", Index: "requested-index"},
	)
	if err != nil {
		t.Fatalf("refreshAuthViaHome() error = %v", err)
	}
	if !attempted {
		t.Fatal("expected Home refresh to be attempted")
	}
	if client.index != "requested-index" {
		t.Fatalf("GetRefreshAuth index = %q, want requested-index", client.index)
	}
	if updated == nil || updated.ID != "auth-old" || updated.Index != "returned-index" {
		t.Fatalf("updated auth = %+v, want original id auth-old and returned index", updated)
	}
}

func TestRefreshAuthViaHomeMapsHomeErrorEnvelope(t *testing.T) {
	payload := []byte(`{"error":{"type":"authentication_error","message":"refresh denied"}}`)
	client := &testHomeRefreshClient{heartbeat: true, payload: payload}
	previous := currentHomeRefreshClient
	currentHomeRefreshClient = func() homeRefreshClient { return client }
	t.Cleanup(func() { currentHomeRefreshClient = previous })

	_, attempted, err := refreshAuthViaHome(
		context.Background(),
		&config.Config{Home: config.HomeConfig{Enabled: true}},
		&cliproxyauth.Auth{ID: "auth-1", Provider: "claude", Index: "idx-1"},
	)
	if !attempted {
		t.Fatal("expected Home refresh to be attempted")
	}
	var statusErr statusErr
	if !errors.As(err, &statusErr) {
		t.Fatalf("expected statusErr, got %T", err)
	}
	if statusErr.StatusCode() != http.StatusUnauthorized {
		t.Fatalf("status code = %d, want %d", statusErr.StatusCode(), http.StatusUnauthorized)
	}
}
