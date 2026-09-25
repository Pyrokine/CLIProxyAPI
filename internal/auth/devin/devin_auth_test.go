package devin

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestGeneratePKCECodesProducesValidS256Pair(t *testing.T) {
	codes, err := GeneratePKCECodes()
	if err != nil {
		t.Fatalf("GeneratePKCECodes(): %v", err)
	}
	if len(codes.CodeVerifier) < 43 || len(codes.CodeVerifier) > 128 {
		t.Fatalf("verifier length = %d, want RFC 7636 range", len(codes.CodeVerifier))
	}
	if _, err = base64.RawURLEncoding.DecodeString(codes.CodeVerifier); err != nil {
		t.Fatalf("verifier is not base64url: %v", err)
	}
	digest := sha256.Sum256([]byte(codes.CodeVerifier))
	if want := base64.RawURLEncoding.EncodeToString(digest[:]); codes.CodeChallenge != want {
		t.Fatalf("challenge = %q, want %q", codes.CodeChallenge, want)
	}
}

func TestBuildAuthorizationURLUsesPKCEAndState(t *testing.T) {
	service := NewDevinAuthService(nil)
	service.SetAppBaseURL("https://example.test/")
	authURL := service.BuildAuthorizationURL("http://127.0.0.1:1234/callback", "challenge+/", "state value")
	parsed, err := url.Parse(authURL)
	if err != nil {
		t.Fatalf("url.Parse(): %v", err)
	}
	if parsed.Path != "/auth/cli/continue" {
		t.Fatalf("path = %q", parsed.Path)
	}
	query := parsed.Query()
	if query.Get("redirect_uri") != "http://127.0.0.1:1234/callback" ||
		query.Get("code_challenge") != "challenge+/" ||
		query.Get("code_challenge_method") != "S256" || query.Get("state") != "state value" {
		t.Fatalf("authorization query = %v", query)
	}
}

func TestExchangeCodeForTokenValidatesRequestAndHidesFailureBody(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/auth/cli/token" || request.Method != http.MethodPost {
			t.Fatalf("request = %s %s", request.Method, request.URL.Path)
		}
		if request.Header.Get("Content-Type") != "application/json" {
			t.Fatalf("Content-Type = %q", request.Header.Get("Content-Type"))
		}
		var payload map[string]string
		if err := json.NewDecoder(request.Body).Decode(&payload); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		if payload["code"] != "code" || payload["code_verifier"] != "verifier" {
			t.Fatalf("request payload = %v", payload)
		}
		_, _ = response.Write([]byte(`{"token":"raw-token"}`))
	}))
	defer server.Close()
	service := NewDevinAuthService(server.Client())
	service.SetAPIBaseURL(server.URL)
	token, err := service.ExchangeCodeForToken(context.Background(), " code ", " verifier ")
	if err != nil {
		t.Fatalf("ExchangeCodeForToken(): %v", err)
	}
	if token != devinTokenPrefix+"raw-token" {
		t.Fatalf("token = %q", token)
	}

	failure := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
		response.WriteHeader(http.StatusUnauthorized)
		_, _ = response.Write([]byte(`{"error":"secret-upstream-body"}`))
	}))
	defer failure.Close()
	service.SetAPIBaseURL(failure.URL)
	_, err = service.ExchangeCodeForToken(context.Background(), "code", "verifier")
	if err == nil || strings.Contains(err.Error(), "secret-upstream-body") {
		t.Fatalf("failure error = %v", err)
	}
}

func TestFetchSelfProfileUsesBearerToken(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/v3/self" || request.Header.Get("Authorization") != "Bearer "+devinTokenPrefix+"token" {
			t.Fatalf("profile request = %s Authorization=%q", request.URL.Path, request.Header.Get("Authorization"))
		}
		_, _ = response.Write([]byte(`{"user_name":"alice","user_id":"user-1","org_id":"org-1"}`))
	}))
	defer server.Close()
	service := NewDevinAuthService(server.Client())
	service.SetAPIBaseURL(server.URL)
	userName, userID, orgID, err := service.FetchSelfProfile(context.Background(), "token")
	if err != nil {
		t.Fatalf("FetchSelfProfile(): %v", err)
	}
	if userName != "alice" || userID != "user-1" || orgID != "org-1" {
		t.Fatalf("profile = %q/%q/%q", userName, userID, orgID)
	}
}

func TestOAuthServerReceivesCallbackAndHonorsContext(t *testing.T) {
	server := NewOAuthServer(0)
	port, err := server.Start()
	if err != nil {
		t.Fatalf("Start(): %v", err)
	}
	defer func() {
		stopContext, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		if stopErr := server.Stop(stopContext); stopErr != nil {
			t.Errorf("Stop(): %v", stopErr)
		}
	}()

	response, err := http.Get("http://127.0.0.1:" + strconv.Itoa(port) + "/callback?code=auth-code&state=oauth-state")
	if err != nil {
		t.Fatalf("GET callback: %v", err)
	}
	_, _ = io.Copy(io.Discard, response.Body)
	_ = response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("callback status = %d", response.StatusCode)
	}
	result, err := server.WaitForCallbackWithContext(context.Background(), time.Second)
	if err != nil {
		t.Fatalf("WaitForCallbackWithContext(): %v", err)
	}
	if result.Code != "auth-code" || result.State != "oauth-state" || result.Error != "" {
		t.Fatalf("callback result = %+v", result)
	}

	cancelledServer := NewOAuthServer(0)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err = cancelledServer.WaitForCallbackWithContext(ctx, time.Minute); err == nil {
		t.Fatal("cancelled wait error = nil")
	}
}

func TestFormatSessionToken(t *testing.T) {
	if got := FormatSessionToken(" raw "); got != devinTokenPrefix+"raw" {
		t.Fatalf("FormatSessionToken(raw) = %q", got)
	}
	prefixed := devinTokenPrefix + "token"
	if got := FormatSessionToken(prefixed); got != prefixed {
		t.Fatalf("FormatSessionToken(prefixed) = %q", got)
	}
	if got := FormatSessionToken("  "); got != "" {
		t.Fatalf("FormatSessionToken(empty) = %q", got)
	}
}

func TestReadBoundedDevinBodyRejectsOversize(t *testing.T) {
	if _, err := readBoundedDevinBody(strings.NewReader("12345"), 4); err == nil {
		t.Fatal("readBoundedDevinBody() error = nil")
	}
}
