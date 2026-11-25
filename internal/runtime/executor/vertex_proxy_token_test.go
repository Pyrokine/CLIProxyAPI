package executor

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/util"
	cliproxyauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
)

const testVertexTokenURL = "https://oauth2.googleapis.com/token"

func TestVertexAccessTokenUsesCredentialProxyNotRequestProxy(t *testing.T) {
	var requestHits atomic.Int32
	var authHits atomic.Int32
	var globalHits atomic.Int32
	requestProxy := httptest.NewServer(
		http.HandlerFunc(
			func(w http.ResponseWriter, _ *http.Request) {
				requestHits.Add(1)
				http.Error(w, "request proxy", http.StatusBadGateway)
			},
		),
	)
	defer requestProxy.Close()
	authProxy := httptest.NewServer(
		http.HandlerFunc(
			func(w http.ResponseWriter, _ *http.Request) {
				authHits.Add(1)
				http.Error(w, "auth proxy", http.StatusBadGateway)
			},
		),
	)
	defer authProxy.Close()
	globalProxy := httptest.NewServer(
		http.HandlerFunc(
			func(w http.ResponseWriter, _ *http.Request) {
				globalHits.Add(1)
				http.Error(w, "global proxy", http.StatusBadGateway)
			},
		),
	)
	defer globalProxy.Close()

	ctx, cancel := context.WithTimeout(
		cliproxyexecutor.WithRequestProxyURL(context.Background(), requestProxy.URL),
		3*time.Second,
	)
	defer cancel()
	_, _ = vertexAccessToken(
		ctx,
		&config.Config{ProxyURL: globalProxy.URL},
		&cliproxyauth.Auth{ProxyURL: authProxy.URL},
		testVertexServiceAccountJSON(t, testVertexTokenURL),
	)

	if requestHits.Load() != 0 {
		t.Fatalf("token exchange used request proxy %d times", requestHits.Load())
	}
	if authHits.Load() == 0 {
		t.Fatal("token exchange did not use the credential proxy")
	}
	if globalHits.Load() != 0 {
		t.Fatalf("token exchange used global proxy %d times", globalHits.Load())
	}
}

func TestVertexAccessTokenUsesTrustedEndpoint(t *testing.T) {
	serviceAccount := googleImagenServiceAccount(t)
	for _, tc := range []struct {
		name, tokenURL string
		redirect       bool
		wantCalls      int
	}{
		{name: "standard", tokenURL: testVertexTokenURL, wantCalls: 1},
		{name: "omitted", wantCalls: 1},
		{name: "unsupported", tokenURL: testVertexTokenURL + "/alternate"},
		{name: "no redirect", tokenURL: testVertexTokenURL, redirect: true, wantCalls: 1},
	} {
		t.Run(
			tc.name, func(t *testing.T) {
				delete(serviceAccount, "token_uri")
				if tc.tokenURL != "" {
					serviceAccount["token_uri"] = tc.tokenURL
				}
				raw, err := json.Marshal(serviceAccount)
				if err != nil {
					t.Fatal(err)
				}
				calls := 0
				ctx := util.WithRoundTripper(
					t.Context(), googlePayloadTransport(
						func(req *http.Request) (*http.Response, error) {
							calls++
							if req.URL.String() != testVertexTokenURL || req.Method != http.MethodPost {
								t.Fatalf("unexpected token request: %s %s", req.Method, req.URL)
							}
							resp := googlePayloadResponse(req, `{"access_token":"fixture-token","expires_in":3600}`)
							if tc.redirect {
								resp.StatusCode = http.StatusFound
								resp.Header.Set("Location", "/alternate")
							}
							return resp, nil
						},
					),
				)
				token, err := vertexAccessToken(ctx, nil, nil, raw)
				wantError := tc.wantCalls == 0 || tc.redirect
				if (err != nil) != wantError || calls != tc.wantCalls || !wantError && token != "fixture-token" {
					t.Fatalf("token exchange: error = %v, calls = %d, token received = %t", err, calls, token != "")
				}
			},
		)
	}
}

func TestVertexRejectsInvalidSavedLocation(t *testing.T) {
	ctx := util.WithRoundTripper(
		t.Context(), googlePayloadTransport(
			func(req *http.Request) (*http.Response, error) {
				t.Fatalf("request dispatched for invalid location: %s", req.URL)
				return nil, nil
			},
		),
	)
	auth := &cliproxyauth.Auth{
		Provider: "vertex", Metadata: map[string]any{
			"project_id": "fixture-project", "location": "regional", "service_account": googleImagenServiceAccount(t),
		},
	}
	exec := NewGeminiVertexExecutor(&config.Config{})
	req := cliproxyexecutor.Request{Model: "gemini-2.5-flash", Payload: []byte(`{"contents":[]}`)}
	opts := cliproxyexecutor.Options{}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://aiplatform.googleapis.com", nil)
	if err != nil {
		t.Fatal(err)
	}
	_, executeErr := exec.Execute(ctx, auth, req, opts)
	_, streamErr := exec.ExecuteStream(ctx, auth, req, opts)
	_, countErr := exec.CountTokens(ctx, auth, req, opts)
	for name, err := range map[string]error{
		"prepare": exec.PrepareRequest(httpReq, auth), "execute": executeErr, "stream": streamErr, "count": countErr,
	} {
		if err == nil || !strings.Contains(err.Error(), "location") {
			t.Errorf("%s error = %v, want invalid location", name, err)
		}
	}
}

func testVertexServiceAccountJSON(t *testing.T, tokenURI string) []byte {
	t.Helper()
	key, errKey := rsa.GenerateKey(rand.Reader, 2048)
	if errKey != nil {
		t.Fatalf("generate key: %v", errKey)
	}
	pkcs8, errMarshal := x509.MarshalPKCS8PrivateKey(key)
	if errMarshal != nil {
		t.Fatalf("marshal key: %v", errMarshal)
	}
	raw, errJSON := json.Marshal(
		map[string]any{
			"type":           "service_account",
			"project_id":     "proxy-test",
			"private_key_id": "kid",
			"private_key":    string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: pkcs8})),
			"client_email":   "proxy-test@proxy-test.iam.gserviceaccount.com",
			"token_uri":      tokenURI,
		},
	)
	if errJSON != nil {
		t.Fatalf("marshal service account: %v", errJSON)
	}
	return raw
}
