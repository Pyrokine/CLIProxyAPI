package executor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"testing"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/clienterror"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	cliproxyauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
)

type testHomeRefreshClient struct {
	heartbeat       bool
	payload         []byte
	err             error
	index           string
	accessTokenHash string
}

func (c *testHomeRefreshClient) HeartbeatOK() bool { return c.heartbeat }

func (c *testHomeRefreshClient) GetRefreshAuth(
	_ context.Context,
	authIndex,
	accessTokenHash string,
) ([]byte, error) {
	c.index = authIndex
	c.accessTokenHash = accessTokenHash
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

	requestAuth := &cliproxyauth.Auth{
		ID:       "auth-old",
		Provider: "codex",
		Index:    "requested-index",
		Metadata: map[string]any{"access_token": "observed-access-token"},
	}
	updated, attempted, err := refreshAuthViaHome(
		context.Background(),
		&config.Config{Home: config.HomeConfig{Enabled: true}},
		requestAuth,
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
	if wantHash := cliproxyauth.AccessTokenSHA256(requestAuth); client.accessTokenHash != wantHash {
		t.Fatalf("GetRefreshAuth access token hash = %q, want %q", client.accessTokenHash, wantHash)
	}
	if updated == nil || updated.ID != "auth-old" || updated.Index != "returned-index" {
		t.Fatalf("updated auth = %+v, want original id auth-old and returned index", updated)
	}
}

func TestRefreshAuthViaHomeUsesContextClient(t *testing.T) {
	payload := []byte(`{"auth":{"provider":"codex","metadata":{"token":"local"}},"auth_index":"local-index"}`)
	localClient := &testHomeRefreshClient{heartbeat: true, payload: payload}
	globalClient := &testHomeRefreshClient{heartbeat: true, payload: []byte(`{"auth_index":"global-index"}`)}
	previous := currentHomeRefreshClient
	currentHomeRefreshClient = func() homeRefreshClient { return globalClient }
	t.Cleanup(func() { currentHomeRefreshClient = previous })

	ctx := cliproxyauth.WithHomeRefreshClient(context.Background(), localClient)
	updated, attempted, errRefresh := refreshAuthViaHome(
		ctx,
		&config.Config{Home: config.HomeConfig{Enabled: true}},
		&cliproxyauth.Auth{ID: "auth-old", Provider: "codex", Index: "requested-index"},
	)
	if errRefresh != nil || !attempted {
		t.Fatalf("refreshAuthViaHome() = (%#v, %t, %v)", updated, attempted, errRefresh)
	}
	if localClient.index != "requested-index" || globalClient.index != "" {
		t.Fatalf("refresh clients indexes = local %q, global %q", localClient.index, globalClient.index)
	}
	if updated == nil || updated.Index != "local-index" {
		t.Fatalf("updated auth = %#v", updated)
	}

	nilCtx := cliproxyauth.WithHomeRefreshClient(context.Background(), nil)
	_, attempted, errRefresh = refreshAuthViaHome(
		nilCtx,
		&config.Config{Home: config.HomeConfig{Enabled: true}},
		&cliproxyauth.Auth{ID: "auth-old", Provider: "codex", Index: "requested-index"},
	)
	if !attempted || errRefresh == nil {
		t.Fatalf("explicit nil Home client = attempted %t, error %v", attempted, errRefresh)
	}
	if globalClient.index != "" {
		t.Fatalf("explicit nil Home client used compatibility client: %q", globalClient.index)
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

func TestRefreshAuthViaHomePreservesMarkedUpstreamResponse(t *testing.T) {
	for _, test := range []struct {
		name, body string
		status     int
	}{
		{"json", "  {\"error\":\"synthetic denied\"}\n", http.StatusUnauthorized},
		{"text", " synthetic rate limited\n", http.StatusTooManyRequests},
		{"empty", "", http.StatusUnauthorized},
		{"binary", "\xff\x00synthetic", http.StatusBadGateway},
	} {
		t.Run(test.name, func(t *testing.T) {
			payload, err := json.Marshal(map[string]any{
				"error": map[string]any{"type": "authentication_error", "message": "private control diagnostic", "upstream": map[string]any{"status": test.status, "body": []byte(test.body)}},
				"auth":  map[string]any{"metadata": map[string]any{"access_token": "synthetic-private-token"}},
			})
			if err != nil {
				t.Fatal(err)
			}
			client := &testHomeRefreshClient{heartbeat: true, payload: payload}
			ctx := cliproxyauth.WithHomeRefreshClient(context.Background(), client)
			updated, attempted, err := refreshAuthViaHome(ctx, &config.Config{Home: config.HomeConfig{Enabled: true}}, &cliproxyauth.Auth{ID: "synthetic", Index: "synthetic-index"})
			if !attempted || updated != nil || err == nil {
				t.Fatalf("updated=%v attempted=%t err=%v", updated, attempted, err)
			}
			for _, cause := range []error{err, fmt.Errorf("wrapper: %w", err), markResponseStarted(err)} {
				body, direct := clienterror.ResponseBodyFromError(cause)
				if !direct || string(body) != test.body || clienterror.HTTPStatusFromError(cause) != test.status {
					t.Fatalf("status=%d body=%q direct=%t", clienterror.HTTPStatusFromError(cause), body, direct)
				}
				failure := failFromErrors(nil, cause)
				if failure.StatusCode != test.status || failure.Body != test.body {
					t.Fatalf("usage failure=%#v", failure)
				}
			}
			var upstream statusErr
			if !errors.As(err, &upstream) || upstream.IsRequestScoped() || upstream.ExecutionDisposition().DispatchPhase != cliproxyexecutor.DispatchPhaseResponseStarted {
				t.Fatalf("upstream disposition=%#v", upstream)
			}
			var terminated *cliproxyexecutor.RequestTerminatedError
			if errors.As(err, &terminated) {
				t.Fatal("upstream response became plugin termination")
			}
		})
	}
}

func TestRefreshAuthViaHomeRejectsMalformedUpstreamEnvelope(t *testing.T) {
	for _, payload := range []string{
		`{"error":{"upstream":{"status":0,"body":"cHJpdmF0ZQ=="}}}`,
		`{"error":{"upstream":{"status":-1,"body":"cHJpdmF0ZQ=="}}}`,
		`{"error":{"upstream":{"status":200,"body":"cHJpdmF0ZQ=="}}}`,
		`{"error":{"upstream":{"status":399,"body":"cHJpdmF0ZQ=="}}}`,
		`{"error":{"upstream":{"status":600,"body":"cHJpdmF0ZQ=="}}}`,
		`{"error":{"upstream":{"status":401,"body":"not-base64"}},"auth":{"metadata":{"access_token":"synthetic-private"}}}`,
		`{"error":{"upstream":{"status":401,"body":{"access_token":"synthetic-private"}}}}`,
	} {
		client := &testHomeRefreshClient{heartbeat: true, payload: []byte(payload)}
		updated, attempted, err := refreshAuthViaHome(cliproxyauth.WithHomeRefreshClient(context.Background(), client), &config.Config{Home: config.HomeConfig{Enabled: true}}, &cliproxyauth.Auth{ID: "synthetic", Index: "synthetic-index"})
		if !attempted || updated != nil || clienterror.HTTPStatusFromError(err) != http.StatusBadGateway {
			t.Fatalf("updated=%v attempted=%t err=%v", updated, attempted, err)
		}
		if body, direct := clienterror.ResponseBodyFromError(err); direct || len(body) != 0 {
			t.Fatalf("invalid envelope exposed body=%q", body)
		}
	}
}

func TestRefreshAuthViaHomeKeepsControlErrorsPrivate(t *testing.T) {
	client := &testHomeRefreshClient{heartbeat: true, payload: []byte(`{"error":{"type":"authentication_error","message":"synthetic-private-token"},"auth":{"metadata":{"access_token":"synthetic-private-token"}}}`)}
	updated, attempted, err := refreshAuthViaHome(cliproxyauth.WithHomeRefreshClient(context.Background(), client), &config.Config{Home: config.HomeConfig{Enabled: true}}, &cliproxyauth.Auth{ID: "synthetic", Index: "synthetic-index"})
	if !attempted || updated != nil || err == nil || err.Error() != "credential unauthorized" {
		t.Fatalf("updated=%v attempted=%t err=%v", updated, attempted, err)
	}
	if _, direct := clienterror.ResponseBodyFromError(err); direct {
		t.Fatal("unmarked control error became direct response")
	}
	failure := failFromErrors(err)
	if failure.StatusCode != http.StatusUnauthorized || failure.Body != "credential unauthorized" {
		t.Fatalf("failure=%#v", failure)
	}
}

func TestC08RefreshAuthViaHomeRejectsDisabledCredential(t *testing.T) {
	payload := []byte(`{"auth":{"provider":"codex","disabled":true,"metadata":{"access_token":"new"}},"auth_index":"idx-1"}`)
	client := &testHomeRefreshClient{heartbeat: true, payload: payload}
	previous := currentHomeRefreshClient
	currentHomeRefreshClient = func() homeRefreshClient { return client }
	t.Cleanup(func() { currentHomeRefreshClient = previous })

	updated, attempted, errRefresh := refreshAuthViaHome(
		context.Background(),
		&config.Config{Home: config.HomeConfig{Enabled: true}},
		&cliproxyauth.Auth{ID: "auth-1", Provider: "codex", Index: "idx-1"},
	)
	if !attempted || updated != nil {
		t.Fatalf("refresh result = (%#v, %t), want nil, true", updated, attempted)
	}
	var refreshStatus statusErr
	if !errors.As(errRefresh, &refreshStatus) {
		t.Fatalf("refresh error = %T %v, want statusErr", errRefresh, errRefresh)
	}
	if refreshStatus.StatusCode() != http.StatusUnauthorized {
		t.Fatalf("refresh status = %d, want 401", refreshStatus.StatusCode())
	}
}
