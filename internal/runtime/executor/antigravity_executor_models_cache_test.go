package executor

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/util"
	cliproxyauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
)

type antigravityCancelReadBody struct {
	ctx     context.Context
	started chan struct{}
	once    sync.Once
}

func (b *antigravityCancelReadBody) Read([]byte) (int, error) {
	b.once.Do(func() { close(b.started) })
	<-b.ctx.Done()
	return 0, b.ctx.Err()
}

func (b *antigravityCancelReadBody) Close() error { return nil }

func antigravityModelDiscoveryAuth(baseURL string) *cliproxyauth.Auth {
	return &cliproxyauth.Auth{
		ID:       "antigravity-discovery-test",
		Provider: "antigravity",
		Attributes: map[string]string{
			"base_url": baseURL,
		},
		Metadata: map[string]any{
			"access_token": "test-token",
			"expired":      time.Now().Add(2 * time.Hour).Format(time.RFC3339),
		},
	}
}

func TestFetchAntigravityModelsClassifiesResponses(t *testing.T) {
	tests := []struct {
		name        string
		status      int
		body        string
		wantOutcome modelDiscoveryOutcome
		wantModels  int
		wantErr     bool
	}{
		{
			name:        "authoritative positive",
			status:      http.StatusOK,
			body:        `{"models":{"claude-sonnet-4-5":{"displayName":"Gemini 2.5 Pro"}}}`,
			wantOutcome: ModelDiscoveryComplete,
			wantModels:  1,
		},
		{
			name:        "authoritative empty",
			status:      http.StatusOK,
			body:        `{"models":{}}`,
			wantOutcome: ModelDiscoveryComplete,
		},
		{
			name:        "missing models field",
			status:      http.StatusOK,
			body:        `{}`,
			wantOutcome: ModelDiscoveryInconclusive,
			wantErr:     true,
		},
		{
			name:        "invalid models field",
			status:      http.StatusOK,
			body:        `{"models":[]}`,
			wantOutcome: ModelDiscoveryInconclusive,
			wantErr:     true,
		},
		{
			name:        "provider failure",
			status:      http.StatusServiceUnavailable,
			body:        `{"error":"unavailable"}`,
			wantOutcome: ModelDiscoveryFailed,
			wantErr:     true,
		},
	}

	for _, test := range tests {
		t.Run(
			test.name, func(t *testing.T) {
				server := httptest.NewServer(
					http.HandlerFunc(
						func(w http.ResponseWriter, r *http.Request) {
							if r.URL.Path != antigravityModelsPath {
								t.Errorf("request path = %q, want %q", r.URL.Path, antigravityModelsPath)
							}
							if got := r.Header.Get("Authorization"); got != "Bearer test-token" {
								t.Errorf("Authorization = %q, want bearer token", got)
							}
							w.WriteHeader(test.status)
							_, _ = w.Write([]byte(test.body))
						},
					),
				)
				defer server.Close()

				result := FetchAntigravityModels(
					context.Background(),
					antigravityModelDiscoveryAuth(server.URL),
					&config.Config{},
				)
				if result.Outcome != test.wantOutcome {
					t.Fatalf("Outcome = %q, want %q; error = %v", result.Outcome, test.wantOutcome, result.Err)
				}
				if len(result.Models) != test.wantModels {
					t.Fatalf("model count = %d, want %d", len(result.Models), test.wantModels)
				}
				if test.wantErr != (result.Err != nil) {
					t.Fatalf("error = %v, wantErr = %t", result.Err, test.wantErr)
				}
			},
		)
	}
}

func TestFetchAntigravityModelsReturnsRefreshedAuth(t *testing.T) {
	roundTripper := antigravityRoundTripFunc(
		func(req *http.Request) (*http.Response, error) {
			var body string
			switch {
			case req.URL.Host == "oauth2.googleapis.com" && req.URL.Path == "/token":
				body = `{"access_token":"new-access","refresh_token":"new-refresh","expires_in":3600,"token_type":"Bearer"}`
			case req.URL.Host == "models.example" && req.URL.Path == antigravityModelsPath:
				if got := req.Header.Get("Authorization"); got != "Bearer new-access" {
					t.Errorf("Authorization = %q, want refreshed access token", got)
				}
				body = `{"models":{"claude-sonnet-4-5":{"displayName":"Claude Sonnet 4.5"}}}`
			default:
				t.Fatalf("unexpected request URL %s", req.URL)
			}
			return &http.Response{
				StatusCode: http.StatusOK,
				Body:       io.NopCloser(strings.NewReader(body)),
				Header:     make(http.Header),
				Request:    req,
			}, nil
		},
	)
	ctx := util.WithRoundTripper(context.Background(), roundTripper)
	auth := &cliproxyauth.Auth{
		ID:       "antigravity-refresh-models",
		Provider: "antigravity",
		Attributes: map[string]string{
			"base_url": "https://models.example",
		},
		Metadata: map[string]any{
			"refresh_token": "model-discovery-refresh-token",
			"project_id":    "project-id",
		},
	}

	result := FetchAntigravityModels(ctx, auth, &config.Config{})
	if result.Outcome != ModelDiscoveryComplete || result.Err != nil || len(result.Models) != 1 {
		t.Fatalf("model discovery result = %#v", result)
	}
	if result.Auth == nil || metaStringValue(result.Auth.Metadata, "access_token") != "new-access" ||
		metaStringValue(result.Auth.Metadata, "refresh_token") != "new-refresh" {
		t.Fatalf("refreshed auth = %#v", result.Auth)
	}
	if metaStringValue(auth.Metadata, "access_token") != "" ||
		metaStringValue(auth.Metadata, "refresh_token") != "model-discovery-refresh-token" {
		t.Fatalf("source auth was mutated: %#v", auth.Metadata)
	}
}

func TestFetchAntigravityModelsCancellationDuringBodyReadIsExact(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	started := make(chan struct{})
	roundTripper := antigravityRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     make(http.Header),
			Body: &antigravityCancelReadBody{
				ctx:     req.Context(),
				started: started,
			},
			Request: req,
		}, nil
	})
	resultCh := make(chan ModelDiscoveryResult, 1)
	go func() {
		resultCh <- FetchAntigravityModels(
			util.WithRoundTripper(ctx, roundTripper),
			antigravityModelDiscoveryAuth("https://models.example"),
			&config.Config{},
		)
	}()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("model discovery response body was not read")
	}
	cancel()

	select {
	case result := <-resultCh:
		if result.Outcome != ModelDiscoveryInconclusive || result.Err != context.Canceled {
			t.Fatalf("model discovery result = %#v, want exact context.Canceled", result)
		}
	case <-time.After(time.Second):
		t.Fatal("canceled model discovery body read did not return")
	}
}

func TestFetchAntigravityModelsCancellationIsInconclusive(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	server := httptest.NewServer(
		http.HandlerFunc(
			func(http.ResponseWriter, *http.Request) {
				close(started)
				<-release
			},
		),
	)
	defer server.Close()
	defer close(release)

	ctx, cancel := context.WithCancel(context.Background())
	resultCh := make(chan ModelDiscoveryResult, 1)
	go func() {
		resultCh <- FetchAntigravityModels(
			ctx,
			antigravityModelDiscoveryAuth(server.URL),
			&config.Config{},
		)
	}()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("model discovery request did not start")
	}
	cancel()

	select {
	case result := <-resultCh:
		if result.Outcome != ModelDiscoveryInconclusive {
			t.Fatalf("Outcome = %q, want %q", result.Outcome, ModelDiscoveryInconclusive)
		}
		if result.Err != context.Canceled {
			t.Fatalf("error = %#v, want context.Canceled", result.Err)
		}
	case <-time.After(time.Second):
		t.Fatal("canceled model discovery did not return")
	}
}
