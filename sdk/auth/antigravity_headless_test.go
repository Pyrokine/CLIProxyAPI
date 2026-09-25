package auth_test

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/auth"
)

type antigravityRoundTripper func(*http.Request) (*http.Response, error)

func (f antigravityRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

func antigravityResponse(statusCode int, body string) *http.Response {
	return &http.Response{
		StatusCode: statusCode,
		Header:     make(http.Header),
		Body:       io.NopCloser(strings.NewReader(body)),
	}
}

func TestAntigravityHeadlessOAuthUsesInjectedClientWithoutPersistence(t *testing.T) {
	credentialRoot := t.TempDir()
	t.Setenv("HOME", credentialRoot)
	t.Setenv("XDG_CONFIG_HOME", credentialRoot)

	redirectURI := "http://127.0.0.1:1/oauth-callback"

	var calls atomic.Int32
	client := &http.Client{Transport: antigravityRoundTripper(func(req *http.Request) (*http.Response, error) {
		calls.Add(1)
		switch {
		case req.URL.String() == "https://oauth2.googleapis.com/token":
			body, errRead := io.ReadAll(req.Body)
			if errRead != nil {
				t.Fatalf("read token request: %v", errRead)
			}
			values, errParse := url.ParseQuery(string(body))
			if errParse != nil {
				t.Fatalf("parse token request: %v", errParse)
			}
			if values.Get("redirect_uri") != redirectURI {
				t.Fatalf("redirect_uri = %q, want %q", values.Get("redirect_uri"), redirectURI)
			}
			return antigravityResponse(
				http.StatusOK,
				`{"access_token":"access-token","refresh_token":"refresh-token","expires_in":3600,"token_type":"Bearer"}`,
			), nil
		case strings.HasPrefix(req.URL.String(), "https://www.googleapis.com/oauth2/v2/userinfo"):
			return antigravityResponse(http.StatusOK, `{"email":"engineer@example.com"}`), nil
		case strings.Contains(req.URL.String(), ":loadCodeAssist"):
			return antigravityResponse(
				http.StatusOK,
				`{"cloudaicompanionProject":"project-123"}`,
			), nil
		default:
			t.Fatalf("unexpected request: %s %s", req.Method, req.URL.String())
			return nil, nil
		}
	})}

	record, errComplete := auth.CompleteAntigravityOAuth(
		context.Background(),
		"authorization-code",
		redirectURI,
		client,
	)
	if errComplete != nil {
		t.Fatalf("CompleteAntigravityOAuth() error = %v", errComplete)
	}
	if calls.Load() != 3 {
		t.Fatalf("HTTP calls = %d, want 3", calls.Load())
	}
	if record == nil || record.Label != "engineer@example.com" {
		t.Fatalf("credential = %#v", record)
	}
	if record.Metadata["project_id"] != "project-123" {
		t.Fatalf("project_id = %#v, want project-123", record.Metadata["project_id"])
	}
	entries, errReadDir := os.ReadDir(credentialRoot)
	if errReadDir != nil {
		t.Fatalf("read credential root: %v", errReadDir)
	}
	if len(entries) != 0 {
		t.Fatalf("headless OAuth wrote files under credential root: %#v", entries)
	}
}

func TestCompleteAntigravityOAuthPreservesTypedHTTPStatus(t *testing.T) {
	client := &http.Client{Transport: antigravityRoundTripper(func(*http.Request) (*http.Response, error) {
		return antigravityResponse(http.StatusForbidden, `{"error":"forbidden"}`), nil
	})}
	_, errComplete := auth.CompleteAntigravityOAuth(context.Background(), "code", "", client)
	if errComplete == nil {
		t.Fatal("expected upstream HTTP error")
	}
	var statusError interface{ StatusCode() int }
	if !errors.As(errComplete, &statusError) {
		t.Fatalf("error type = %T, want status-coded error", errComplete)
	}
	if statusError.StatusCode() != http.StatusForbidden {
		t.Fatalf("status code = %d, want %d", statusError.StatusCode(), http.StatusForbidden)
	}
}

func TestBuildAntigravityAuthURLUsesDefaultCallback(t *testing.T) {
	authURL, errParse := url.Parse(auth.BuildAntigravityAuthURL("state-123", ""))
	if errParse != nil {
		t.Fatalf("parse auth URL: %v", errParse)
	}
	if got := authURL.Query().Get("redirect_uri"); got != auth.AntigravityDefaultCallbackURI() {
		t.Fatalf("redirect_uri = %q, want %q", got, auth.AntigravityDefaultCallbackURI())
	}
	if got := authURL.Query().Get("state"); got != "state-123" {
		t.Fatalf("state = %q, want state-123", got)
	}
}
