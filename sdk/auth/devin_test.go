package auth

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	devinauth "github.com/Pyrokine/CLIProxyAPI/v6/internal/auth/devin"
	coreauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/config"
)

func TestParseDevinManualLoginInput(t *testing.T) {
	const state = "expected-state"
	tests := []struct {
		name      string
		input     string
		wantCode  string
		wantToken string
		accepted  bool
		wantError string
	}{
		{name: "empty", input: "  "},
		{
			name:      "prefixed session token",
			input:     "  devin-session-token$fixture  ",
			wantToken: "devin-session-token$fixture",
			accepted:  true,
		},
		{
			name:      "quoted session token",
			input:     " 'devin-session-token$fixture' ",
			wantToken: "devin-session-token$fixture",
			accepted:  true,
		},
		{
			name:      "JWT session token",
			input:     "eyJhbGciOiJub25lIn0.fixture.signature",
			wantToken: "eyJhbGciOiJub25lIn0.fixture.signature",
			accepted:  true,
		},
		{
			name:     "callback URL",
			input:    "http://127.0.0.1:1455/callback?code=fixture-code&state=" + state,
			wantCode: "fixture-code",
			accepted: true,
		},
		{
			name:     "headless URL without state",
			input:    "https://example.test/callback?code=fixture-code",
			wantCode: "fixture-code",
			accepted: true,
		},
		{
			name:      "callback state mismatch",
			input:     "http://127.0.0.1:1455/callback?code=fixture-code&state=wrong",
			wantError: "state mismatch",
		},
		{
			name:      "authorization denied",
			input:     "https://example.test/callback?error=access_denied",
			wantError: "OAuth error: access_denied",
		},
		{
			name:      "authorization denied with description",
			input:     "https://example.test/callback?error=access_denied&error_description=user%20denied",
			wantError: "OAuth error: access_denied: user denied",
		},
		{
			name:      "fragment authorization error",
			input:     "https://example.test/callback#error=access_denied&error_description=user%20denied",
			wantError: "OAuth error: access_denied: user denied",
		},
		{name: "raw code", input: "fixture-code", wantCode: "fixture-code", accepted: true},
		{name: "unrecognized structured input", input: "not a valid callback URL"},
	}

	for _, test := range tests {
		t.Run(
			test.name, func(t *testing.T) {
				code, token, accepted, err := parseDevinManualLoginInput(test.input, state)
				if test.wantError != "" {
					if err == nil || !strings.Contains(err.Error(), test.wantError) {
						t.Fatalf("parseDevinManualLoginInput() error = %v, want %q", err, test.wantError)
					}
					return
				}
				if err != nil {
					t.Fatalf("parseDevinManualLoginInput() error = %v", err)
				}
				if code != test.wantCode || token != test.wantToken || accepted != test.accepted {
					t.Fatalf(
						"parseDevinManualLoginInput() = %q/%q/%t, want %q/%q/%t",
						code,
						token,
						accepted,
						test.wantCode,
						test.wantToken,
						test.accepted,
					)
				}
			},
		)
	}
}

func TestDevinNoBrowserDoesNotBindCallbackPort(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func(closeResource func() error) { _ = closeResource() }(listener.Close)
	promptError := errors.New("fixture prompt failure")
	authenticator := &DevinAuthenticator{CallbackPort: listener.Addr().(*net.TCPAddr).Port}
	_, err = authenticator.Login(
		context.Background(), &config.Config{}, &LoginOptions{
			NoBrowser: true,
			Prompt: func(string) (string, error) {
				return "", promptError
			},
		},
	)
	if !errors.Is(err, promptError) {
		t.Fatalf("headless Login error = %v, want prompt error without binding callback port", err)
	}
}

func TestDevinNoBrowserPreservesOAuthErrorDescription(t *testing.T) {
	_, err := NewDevinAuthenticator().Login(
		context.Background(), &config.Config{}, &LoginOptions{
			NoBrowser: true,
			Prompt: func(string) (string, error) {
				return "https://example.test/callback?error=access_denied&error_description=user%20denied", nil
			},
		},
	)
	if err == nil || err.Error() != "devin OAuth error: access_denied: user denied" {
		t.Fatalf("Login() error = %v, want complete OAuth rejection", err)
	}
}

func TestWaitForDevinLoginResultPreservesCallbackErrorDescription(t *testing.T) {
	results := make(chan *devinauth.OAuthResult, 1)
	results <- &devinauth.OAuthResult{Error: "access_denied: user denied"}
	_, _, err := waitForDevinLoginResult(context.Background(), "state", nil, results, make(chan error))
	if err == nil || err.Error() != "devin OAuth error: access_denied: user denied" {
		t.Fatalf("waitForDevinLoginResult() error = %v, want complete OAuth rejection", err)
	}
}

// noinspection GoDirectComparisonOfErrors
func TestWaitForDevinLoginResultHonorsContextCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, _, err := waitForDevinLoginResult(
		ctx,
		"state",
		nil,
		make(chan *devinauth.OAuthResult),
		make(chan error),
	)
	if err == nil || err != context.Canceled {
		t.Fatalf("waitForDevinLoginResult() error = %v, want context canceled", err)
	}
}

type devinLoginRoundTripper func(*http.Request) (*http.Response, error)

func (transport devinLoginRoundTripper) RoundTrip(request *http.Request) (*http.Response, error) {
	return transport(request)
}

func loginDevinWithMockStatus(t *testing.T, input, code, token, profile string) *coreauth.Auth {
	t.Helper()
	blockedProxy := httptest.NewServer(
		http.HandlerFunc(
			func(response http.ResponseWriter, request *http.Request) {
				t.Errorf(
					"Login bypassed its injected authentication service: %s %s",
					request.Method,
					request.URL,
				)
				response.WriteHeader(http.StatusBadGateway)
			},
		),
	)
	t.Cleanup(blockedProxy.Close)
	var calls []string
	client := &http.Client{
		Timeout: time.Second, Transport: devinLoginRoundTripper(
			func(request *http.Request) (*http.Response, error) {
				response := httptest.NewRecorder()
				calls = append(calls, request.Method+" "+request.URL.Host+request.URL.Path)
				if request.URL.Scheme != "https" {
					t.Errorf("unexpected fixture scheme: %s", request.URL.Scheme)
					return nil, errors.New("unexpected Devin fixture request")
				}
				switch {
				case request.URL.Host == "devin-api-fixture.invalid" && request.URL.Path == "/auth/cli/token":
					if request.Method != http.MethodPost || code == "" {
						t.Errorf("unexpected code exchange: %s", request.Method)
						return nil, errors.New("unexpected Devin code exchange")
					}
					var body map[string]string
					if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
						return nil, err
					}
					if body["code"] != code || body["code_verifier"] == "" {
						t.Errorf("code exchange lost its original code or PKCE verifier: %v", body)
						return nil, errors.New("invalid Devin code exchange")
					}
					response.Header().Set("Content-Type", "application/json")
					if err := json.NewEncoder(response).Encode(map[string]string{"token": token}); err != nil {
						return nil, err
					}
				case request.URL.Host == "devin-api-fixture.invalid" && request.URL.Path == "/v3/self":
					if request.Method != http.MethodGet ||
						request.Header.Get("Authorization") != "Bearer "+devinauth.FormatSessionToken(token) {
						t.Error("profile request lost its method or session token")
						return nil, errors.New("invalid Devin profile request")
					}
					response.Header().Set("Content-Type", "application/json")
					_, _ = response.Write([]byte(profile))
				case request.URL.Host ==
					"devin-status-fixture.invalid" &&
					request.URL.Path ==
						devinauth.GetUserStatusPath:
					formatted := devinauth.FormatSessionToken(token)
					if request.Method !=
						http.MethodPost ||
						request.Header.Get("Authorization") !=
							"Basic "+
								formatted+
								"-"+
								formatted ||
						request.Header.Get("Connect-Protocol-Version") != "1" ||
						request.Header.Get("Content-Type") != "application/proto" {
						t.Error("status request lost its method, authentication or Connect protocol")
						return nil, errors.New("invalid Devin status request")
					}
					response.WriteHeader(http.StatusServiceUnavailable)
				default:
					t.Errorf("unexpected fixture endpoint: %s", request.URL)
					return nil, errors.New("unexpected Devin fixture endpoint")
				}
				return response.Result(), nil
			},
		),
	}
	service := devinauth.NewDevinAuthService(client)
	service.SetAppBaseURL("https://devin-login-fixture.invalid")
	service.SetAPIBaseURL("https://devin-api-fixture.invalid")
	service.SetServerBaseURL("https://devin-status-fixture.invalid")
	authenticator := NewDevinAuthenticator()
	authenticator.AuthService = service
	cfg := &config.Config{}
	cfg.ProxyURL = blockedProxy.URL
	record, err := authenticator.Login(
		t.Context(), cfg, &LoginOptions{
			NoBrowser: true,
			Prompt: func(string) (string, error) {
				return input, nil
			},
		},
	)
	if err != nil {
		t.Fatalf("Login failed: %v", err)
	}
	wantCalls := []string{
		"GET devin-api-fixture.invalid/v3/self",
		"POST devin-status-fixture.invalid" + devinauth.GetUserStatusPath,
	}
	if code != "" {
		wantCalls = append([]string{"POST devin-api-fixture.invalid/auth/cli/token"}, wantCalls...)
	}
	if !slices.Equal(calls, wantCalls) {
		t.Fatalf("Login requests = %v, want %v", calls, wantCalls)
	}
	if authenticator.AuthService != service {
		t.Fatal("Login replaced the caller's authentication service")
	}
	return record
}

func TestDevinAuthenticatorHeadlessManualTokenLogin(t *testing.T) {
	auth := loginDevinWithMockStatus(
		t, "devin-session-token$eyJmock.session.token", "", "devin-session-token$eyJmock.session.token",
		`{"user_name":"token-user","user_id":"uid-token","org_id":"org-token"}`,
	)
	if auth.Provider != "devin" {
		t.Errorf("auth.Provider = %q, want devin", auth.Provider)
	}
	if auth.Attributes["api_key"] != "devin-session-token$eyJmock.session.token" {
		t.Errorf("api_key = %q, want expected", auth.Attributes["api_key"])
	}
	if auth.Label != "Devin (token-user)" {
		t.Errorf("auth.Label = %q, want 'Devin (token-user)'", auth.Label)
	}
}

func TestDevinAuthenticatorHeadlessManualCodeLogin(t *testing.T) {
	auth := loginDevinWithMockStatus(
		t, "devin-cli-auth-code-123", "devin-cli-auth-code-123", "eyJtest.manual.code.token",
		`{"user_name":"test-user","user_id":"uid-123","org_id":"org-456"}`,
	)
	if auth.Provider != "devin" {
		t.Errorf("auth.Provider = %q, want devin", auth.Provider)
	}
	expectedToken := "devin-session-token$eyJtest.manual.code.token"
	if auth.Attributes["api_key"] != expectedToken {
		t.Errorf("api_key = %q, want %q", auth.Attributes["api_key"], expectedToken)
	}
	expectedLabel := "Devin (test-user)"
	if auth.Label != expectedLabel {
		t.Errorf("auth.Label = %q, want %q", auth.Label, expectedLabel)
	}
}

func TestDevinAuthenticatorHeadlessManualCallbackURLLogin(t *testing.T) {
	auth := loginDevinWithMockStatus(
		t, "http://127.0.0.1:12345/callback?code=parsed-callback-code", "parsed-callback-code",
		"eyJtest.callback.token",
		`{"user_name":"url-user","user_id":"uid-url","org_id":"org-url"}`,
	)
	expectedToken := "devin-session-token$eyJtest.callback.token"
	if auth.Attributes["api_key"] != expectedToken {
		t.Errorf("api_key = %q, want %q", auth.Attributes["api_key"], expectedToken)
	}
	expectedLabel := "Devin (url-user)"
	if auth.Label != expectedLabel {
		t.Errorf("auth.Label = %q, want %q", auth.Label, expectedLabel)
	}
}

func TestManagerLoginPreservesDevinQuotaObservation(t *testing.T) {
	authDir := t.TempDir()
	token := devinauth.FormatSessionToken("synthetic-quota-token")
	fileName := devinauth.CredentialFileName("user-id-456", "org-test-123", token)
	//goland:noinspection SpellCheckingInspection
	record := &coreauth.Auth{
		ID: fileName, FileName: fileName, Provider: "devin", Status: coreauth.StatusActive,
		Metadata: map[string]any{
			"type": "devin", "api_key": token, "session_token": token,
			"user_name": "testuser", "user_id": "user-id-456", "org_id": "org-test-123", "plan": "Pro",
		},
	}
	observedAt := time.Date(2026, 9, 13, 12, 0, 0, 0, time.UTC)
	devinauth.ApplyQuotaStatus(
		&record.Quota, &devinauth.DevinUserStatus{
			Plan: "Pro", DailyQuotaPresent: true, WeeklyQuotaPresent: true,
			DailyQuotaRemainingPercent: 100, WeeklyQuotaRemainingPercent: 50,
			PlanStart: time.Unix(1789087364, 0), PlanEnd: time.Unix(1791679364, 0),
			DailyQuotaResetAt: time.Unix(1789200000, 0), WeeklyQuotaResetAt: time.Unix(1789286400, 0),
		}, observedAt,
	)
	want := map[string]string{
		"plan": "Pro", "daily_quota_present": "true", "weekly_quota_present": "true",
		"daily_quota_remaining_percent": "100%", "weekly_quota_remaining_percent": "50%",
		"plan_start":            time.Unix(1789087364, 0).UTC().Format(time.RFC3339),
		"plan_end":              time.Unix(1791679364, 0).UTC().Format(time.RFC3339),
		"daily_quota_reset_at":  time.Unix(1789200000, 0).UTC().Format(time.RFC3339),
		"weekly_quota_reset_at": time.Unix(1789286400, 0).UTC().Format(time.RFC3339),
	}
	manager := NewManager(NewFileTokenStore(), recordManagerAuthenticator{provider: "devin", record: record})
	returned, savedPath, err := manager.Login(t.Context(), "devin", &config.Config{AuthDir: authDir}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if savedPath != filepath.Join(
		authDir, fileName,
	) || !returned.Quota.ObservedAt.Equal(observedAt) || len(returned.Quota.Signals) != len(want) {
		t.Fatalf("SDK login observation/path = %+v/%q", returned.Quota, savedPath)
	}
	for key, value := range want {
		if returned.Quota.Signals[key] != value {
			t.Fatalf("SDK login quota %s = %q, want %q", key, returned.Quota.Signals[key], value)
		}
	}
	saved := readManagerCredential(t, savedPath)
	if saved["user_id"] != "user-id-456" || saved["org_id"] != "org-test-123" || saved["api_key"] != token ||
		saved["plan"] != "Pro" {
		t.Fatalf("saved Devin identity = %#v", saved)
	}
	for key := range want {
		if key == "plan" {
			continue
		}
		if _, exists := saved[key]; exists {
			t.Fatalf("transient quota %s was saved as credential metadata", key)
		}
	}
	payload, err := json.Marshal(returned)
	if err != nil {
		t.Fatal(err)
	}
	var decoded coreauth.Auth
	if err := json.Unmarshal(payload, &decoded); err != nil {
		t.Fatal(err)
	}
	if !decoded.Quota.ObservedAt.Equal(observedAt) || len(decoded.Quota.Signals) != len(want) {
		t.Fatalf("Auth JSON lost the quota observation: %+v", decoded.Quota)
	}
	for key, value := range want {
		if decoded.Quota.Signals[key] != value {
			t.Fatalf("Auth JSON quota %s = %q, want %q", key, decoded.Quota.Signals[key], value)
		}
	}
}
