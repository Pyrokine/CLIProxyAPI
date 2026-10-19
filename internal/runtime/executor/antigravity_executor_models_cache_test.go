package executor

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/registry"
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

func TestFetchAntigravityModelsUsesOneCatalogEndpoint(t *testing.T) {
	for _, test := range []struct {
		name       string
		attributes map[string]string
		wantHost   string
	}{
		{"default", nil, "daily-cloudcode-pa.googleapis.com"},
		{"configured-list", map[string]string{"base_url": "https://ignored.invalid", "base_urls": " https://first.invalid/ ,https://second.invalid"}, "first.invalid"},
		{"empty-list-elements", map[string]string{"base_urls": " , https://first.invalid///, https://second.invalid"}, "first.invalid"},
	} {
		t.Run(test.name, func(t *testing.T) {
			calls := 0
			transport := antigravityRoundTripFunc(func(req *http.Request) (*http.Response, error) {
				calls++
				if req.URL.Host != test.wantHost || req.URL.Path != antigravityModelsPath {
					t.Errorf("catalog endpoint = %s, want https://%s%s", req.URL, test.wantHost, antigravityModelsPath)
				}
				return &http.Response{StatusCode: http.StatusServiceUnavailable, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"error":"synthetic unavailable"}`)), Request: req}, nil
			})
			auth := antigravityModelDiscoveryAuth("https://unused.invalid")
			auth.Attributes = test.attributes
			result := FetchAntigravityModels(util.WithRoundTripper(context.Background(), transport), auth, &config.Config{})
			if calls != 1 || result.Outcome != ModelDiscoveryFailed || result.Err == nil {
				t.Fatalf("catalog retried another endpoint: calls=%d outcome=%s error=%v", calls, result.Outcome, result.Err)
			}
		})
	}
}

func TestFetchAntigravityModelsRejectsUntrustworthyCatalog(t *testing.T) {
	for _, test := range []struct {
		name string
		body string
	}{
		{"truncated-json", `{"models":{}`},
		{"trailing-json", `{"models":{}} {"models":{"other":{}}}`},
		{"oversized-catalog", `{"models":{}}` + strings.Repeat(" ", 8<<20)},
	} {
		t.Run(test.name, func(t *testing.T) {
			transport := antigravityRoundTripFunc(func(req *http.Request) (*http.Response, error) {
				return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(test.body)), Request: req}, nil
			})
			result := FetchAntigravityModels(util.WithRoundTripper(context.Background(), transport), antigravityModelDiscoveryAuth("https://synthetic.invalid"), &config.Config{})
			if result.Outcome == ModelDiscoveryComplete || result.Err == nil || len(result.Models) != 0 {
				t.Fatalf("untrustworthy response became an authoritative catalog: outcome=%s error=%v", result.Outcome, result.Err)
			}
		})
	}
}

func TestFetchAntigravityModelsEscapesProject(t *testing.T) {
	const project = "synthetic-project\"\\\nname"
	transport := antigravityRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		var payload map[string]string
		if err := json.NewDecoder(req.Body).Decode(&payload); err != nil {
			t.Errorf("invalid project request JSON: %v", err)
		}
		if payload["project"] != project || len(payload) != 1 {
			t.Errorf("request project was changed or introduced another field: %#v", payload)
		}
		return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"models":{}}`)), Request: req}, nil
	})
	auth := antigravityModelDiscoveryAuth("https://synthetic.invalid")
	auth.Metadata["project_id"] = project
	result := FetchAntigravityModels(util.WithRoundTripper(context.Background(), transport), auth, &config.Config{})
	if result.Outcome != ModelDiscoveryComplete || result.Err != nil {
		t.Fatalf("escaped project discovery failed: %s %v", result.Outcome, result.Err)
	}
}

func TestFetchAntigravityModelsPreservesKnownMetadata(t *testing.T) {
	var known *registry.ModelInfo
	for _, model := range registry.GetStaticModelDefinitionsByChannel("antigravity") {
		if model != nil && model.ContextLength > 1 {
			known = model
			break
		}
	}
	if known == nil {
		t.Fatal("no known Antigravity context length is available")
	}
	body, err := json.Marshal(map[string]any{"models": map[string]any{known.ID: map[string]any{"maxTokens": 1}}})
	if err != nil {
		t.Fatal(err)
	}
	transport := antigravityRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(string(body))), Request: req}, nil
	})
	result := FetchAntigravityModels(util.WithRoundTripper(context.Background(), transport), antigravityModelDiscoveryAuth("https://synthetic.invalid"), &config.Config{})
	if result.Outcome != ModelDiscoveryComplete || result.Err != nil || len(result.Models) != 1 {
		t.Fatalf("known model discovery = %s %v", result.Outcome, result.Err)
	}
	discovered := result.Models[0]
	if discovered.ContextLength != known.ContextLength || discovered.MaxCompletionTokens != known.MaxCompletionTokens {
		t.Fatalf("discovery discarded catalog metadata for %s: context=%d completion=%d", known.ID, discovered.ContextLength, discovered.MaxCompletionTokens)
	}
	if discovered.Created == 0 || known.Created != 0 && discovered.Created != known.Created || !reflect.DeepEqual(discovered.Thinking, known.Thinking) ||
		len(known.SupportedInputModalities) > 0 && !reflect.DeepEqual(discovered.SupportedInputModalities, known.SupportedInputModalities) ||
		len(known.SupportedOutputModalities) > 0 && !reflect.DeepEqual(discovered.SupportedOutputModalities, known.SupportedOutputModalities) ||
		len(known.SupportedGenerationMethods) > 0 && !reflect.DeepEqual(discovered.SupportedGenerationMethods, known.SupportedGenerationMethods) {
		t.Fatalf("discovery discarded known capabilities for %s: %+v", known.ID, discovered)
	}
	if current := registry.LookupStaticModelInfoByChannel(known.ID, "antigravity"); !reflect.DeepEqual(current, known) {
		t.Fatal("model discovery mutated shared static metadata")
	}
}

func TestFetchAntigravityModelsDoesNotAcquireCredentialsAfterCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	calls := 0
	transport := antigravityRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		calls++
		return &http.Response{StatusCode: http.StatusServiceUnavailable, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"error":"synthetic unavailable"}`)), Request: req}, nil
	})
	auth := &cliproxyauth.Auth{ID: "synthetic-already-canceled", Provider: "antigravity", Metadata: map[string]any{"refresh_token": "synthetic-refresh-token"}}
	result := FetchAntigravityModels(util.WithRoundTripper(ctx, transport), auth, &config.Config{})
	if calls != 0 || result.Outcome != ModelDiscoveryInconclusive || result.Err != context.Canceled {
		t.Fatalf("canceled discovery acquired credentials: calls=%d outcome=%s error=%v", calls, result.Outcome, result.Err)
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
			name:        "null models field",
			status:      http.StatusOK,
			body:        `{"models":null}`,
			wantOutcome: ModelDiscoveryInconclusive,
			wantErr:     true,
		},
		{
			name:        "maximum catalog size",
			status:      http.StatusOK,
			body:        `{"models":{}}` + strings.Repeat(" ", (8<<20)-len(`{"models":{}}`)),
			wantOutcome: ModelDiscoveryComplete,
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

func TestFetchAntigravityModelsPublishesDiscoveredWebSearchCapability(t *testing.T) {
	trueValue, falseValue := true, false
	for _, test := range []struct {
		name string
		body string
		want map[string]*bool
	}{
		{
			name: "known account capabilities",
			body: `{"models":{"gemini-search":{"displayName":"Search"},"gemini-plain":{"displayName":"Plain"}},"webSearchModelIds":[" GEMINI-SEARCH "]}`,
			want: map[string]*bool{"gemini-search": &trueValue, "gemini-plain": &falseValue},
		},
		{
			name: "empty account capability list",
			body: `{"models":{"gemini-search":{"displayName":"Search"}},"webSearchModelIds":[]}`,
			want: map[string]*bool{"gemini-search": &falseValue},
		},
		{
			name: "no capability evidence",
			body: `{"models":{"gemini-search":{"displayName":"Search"}}}`,
			want: map[string]*bool{"gemini-search": nil},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				_, _ = io.WriteString(w, test.body)
			}))
			defer server.Close()

			result := FetchAntigravityModels(context.Background(), antigravityModelDiscoveryAuth(server.URL), &config.Config{})
			if result.Outcome != ModelDiscoveryComplete || len(result.Models) != len(test.want) {
				t.Fatalf("discovery = %q, %v, models=%+v", result.Outcome, result.Err, result.Models)
			}
			for _, model := range result.Models {
				want, ok := test.want[model.ID]
				if !ok {
					t.Fatalf("unexpected model %q", model.ID)
				}
				var got *bool
				if model.NativeCapabilities != nil {
					got = model.NativeCapabilities.WebSearch
				}
				if (got == nil) != (want == nil) || got != nil && *got != *want {
					t.Fatalf("%s web search = %v, want %v", model.ID, got, want)
				}
			}
		})
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
