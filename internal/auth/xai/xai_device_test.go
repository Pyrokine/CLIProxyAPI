package xai

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"
)

func TestRequestDeviceCodePostsClientIDAndScope(t *testing.T) {
	var gotForm url.Values
	auth := &Auth{
		httpClient: &http.Client{
			Transport: xaiRoundTripFunc(
				func(req *http.Request) (*http.Response, error) {
					if req.Method != http.MethodPost {
						t.Fatalf("method = %s, want POST", req.Method)
					}
					if err := req.ParseForm(); err != nil {
						t.Fatalf("ParseForm() error = %v", err)
					}
					gotForm = req.PostForm
					return xaiJSONResponse(
						req, http.StatusOK, map[string]any{
							"device_code":               "device-abc",
							"user_code":                 "ABCD-1234",
							"verification_uri":          "https://accounts.x.ai/oauth2/device",
							"verification_uri_complete": "https://accounts.x.ai/oauth2/device?user_code=ABCD-1234",
							"expires_in":                1800,
							"interval":                  5,
						},
					), nil
				},
			),
		},
	}

	deviceCode, err := auth.requestDeviceCode(
		context.Background(),
		"https://auth.x.ai/oauth2/device/code",
		"https://auth.x.ai/oauth2/token",
	)
	if err != nil {
		t.Fatalf("RequestDeviceCode() error = %v", err)
	}
	if deviceCode.DeviceCode != "device-abc" || deviceCode.UserCode != "ABCD-1234" {
		t.Fatalf("device response = %#v", deviceCode)
	}
	if deviceCode.TokenEndpoint != "https://auth.x.ai/oauth2/token" {
		t.Fatalf("TokenEndpoint = %q", deviceCode.TokenEndpoint)
	}
	if gotForm.Get("client_id") != clientID {
		t.Fatalf("client_id = %q, want %q", gotForm.Get("client_id"), clientID)
	}
	if gotForm.Get("scope") != scope {
		t.Fatalf("scope = %q, want %q", gotForm.Get("scope"), scope)
	}
}

func TestRequestDeviceCodeRejectsUntrustedEndpoints(t *testing.T) {
	auth := &Auth{
		httpClient: &http.Client{
			Transport: xaiRoundTripFunc(
				func(*http.Request) (*http.Response, error) {
					t.Fatal("unexpected HTTP request")
					return nil, nil
				},
			),
		},
	}

	if _, err := auth.requestDeviceCode(
		context.Background(),
		"https://evil.example/device",
		"https://auth.x.ai/oauth2/token",
	); err == nil {
		t.Fatal("untrusted device endpoint was accepted")
	}
	if _, err := auth.requestDeviceCode(
		context.Background(),
		"https://auth.x.ai/oauth2/device",
		"http://auth.x.ai/oauth2/token",
	); err == nil {
		t.Fatal("non-HTTPS token endpoint was accepted")
	}
}

func TestPollForTokenImmediateSuccess(t *testing.T) {
	var gotForm url.Values
	auth := &Auth{
		httpClient: &http.Client{
			Transport: xaiRoundTripFunc(
				func(req *http.Request) (*http.Response, error) {
					if err := req.ParseForm(); err != nil {
						t.Fatalf("ParseForm() error = %v", err)
					}
					gotForm = req.PostForm
					return xaiJSONResponse(
						req, http.StatusOK, map[string]any{
							"access_token":  "access-1",
							"refresh_token": "refresh-1",
							"token_type":    "Bearer",
							"expires_in":    3600,
						},
					), nil
				},
			),
		},
	}

	tokenData, err := auth.pollForToken(
		context.Background(), &DeviceCodeResponse{
			DeviceCode:    "device-abc",
			ExpiresIn:     60,
			Interval:      5,
			TokenEndpoint: "https://auth.x.ai/oauth2/token",
		},
	)
	if err != nil {
		t.Fatalf("PollForToken() error = %v", err)
	}
	if tokenData.AccessToken != "access-1" || tokenData.RefreshToken != "refresh-1" {
		t.Fatalf("token data = %#v", tokenData)
	}
	if gotForm.Get("grant_type") != deviceCodeGrantType {
		t.Fatalf("grant_type = %q, want %q", gotForm.Get("grant_type"), deviceCodeGrantType)
	}
	if gotForm.Get("device_code") != "device-abc" {
		t.Fatalf("device_code = %q, want device-abc", gotForm.Get("device_code"))
	}
	if gotForm.Get("client_id") != clientID {
		t.Fatalf("client_id = %q, want %q", gotForm.Get("client_id"), clientID)
	}
}

func TestPollForTokenStopsAtDeviceExpiryBeforeNextInterval(t *testing.T) {
	requests := 0
	auth := &Auth{
		httpClient: &http.Client{
			Transport: xaiRoundTripFunc(
				func(req *http.Request) (*http.Response, error) {
					requests++
					return xaiJSONResponse(
						req, http.StatusBadRequest, map[string]any{
							"error": "authorization_pending",
						},
					), nil
				},
			),
		},
	}

	started := time.Now()
	_, err := auth.pollForToken(
		context.Background(), &DeviceCodeResponse{
			DeviceCode:    "device-abc",
			ExpiresIn:     1,
			Interval:      5,
			TokenEndpoint: "https://auth.x.ai/oauth2/token",
		},
	)
	elapsed := time.Since(started)
	if err == nil || !strings.Contains(err.Error(), "expired") {
		t.Fatalf("PollForToken() error = %v, want expired", err)
	}
	if elapsed > 2*time.Second {
		t.Fatalf("PollForToken() elapsed = %s, want device deadline near 1s", elapsed)
	}
	if requests != 1 {
		t.Fatalf("token requests = %d, want 1 immediate request", requests)
	}
}

func TestPollForTokenCancelsInFlightRequestAtDeviceExpiry(t *testing.T) {
	requestCancelled := make(chan struct{}, 1)
	auth := &Auth{
		httpClient: &http.Client{
			Transport: xaiRoundTripFunc(
				func(req *http.Request) (*http.Response, error) {
					<-req.Context().Done()
					requestCancelled <- struct{}{}
					return nil, req.Context().Err()
				},
			),
		},
	}

	_, err := auth.pollForToken(
		context.Background(), &DeviceCodeResponse{
			DeviceCode:    "device-abc",
			ExpiresIn:     1,
			Interval:      5,
			TokenEndpoint: "https://auth.x.ai/oauth2/token",
		},
	)
	if err == nil || !strings.Contains(err.Error(), "expired") {
		t.Fatalf("PollForToken() error = %v, want expired", err)
	}
	select {
	case <-requestCancelled:
	case <-time.After(time.Second):
		t.Fatal("token request context was not cancelled at device expiry")
	}
}

func TestExchangeDeviceCodeOAuthStates(t *testing.T) {
	tests := []struct {
		name             string
		status           int
		payload          map[string]any
		wantContinue     bool
		wantInterval     time.Duration
		wantErrorContain string
	}{
		{
			name:         "authorization pending",
			status:       http.StatusBadRequest,
			payload:      map[string]any{"error": "authorization_pending"},
			wantContinue: true,
			wantInterval: 5 * time.Second,
		},
		{
			name:         "slow down",
			status:       http.StatusBadRequest,
			payload:      map[string]any{"error": "slow_down"},
			wantContinue: true,
			wantInterval: 10 * time.Second,
		},
		{
			name:             "access denied",
			status:           http.StatusBadRequest,
			payload:          map[string]any{"error": "access_denied"},
			wantInterval:     5 * time.Second,
			wantErrorContain: "authorization denied",
		},
		{
			name:             "expired token",
			status:           http.StatusBadRequest,
			payload:          map[string]any{"error": "expired_token"},
			wantInterval:     5 * time.Second,
			wantErrorContain: "expired",
		},
	}

	for _, tt := range tests {
		t.Run(
			tt.name, func(t *testing.T) {
				auth := &Auth{
					httpClient: &http.Client{
						Transport: xaiRoundTripFunc(
							func(req *http.Request) (*http.Response, error) {
								return xaiJSONResponse(req, tt.status, tt.payload), nil
							},
						),
					},
				}
				token, interval, shouldContinue, err := auth.exchangeDeviceCode(
					context.Background(),
					"https://auth.x.ai/oauth2/token",
					"device-abc",
					5*time.Second,
				)
				if token != nil {
					t.Fatalf("token = %#v, want nil", token)
				}
				if shouldContinue != tt.wantContinue {
					t.Fatalf("shouldContinue = %v, want %v", shouldContinue, tt.wantContinue)
				}
				if interval != tt.wantInterval {
					t.Fatalf("interval = %s, want %s", interval, tt.wantInterval)
				}
				if tt.wantErrorContain == "" {
					if err != nil {
						t.Fatalf("error = %v, want nil", err)
					}
				} else if err == nil || !strings.Contains(err.Error(), tt.wantErrorContain) {
					t.Fatalf("error = %v, want containing %q", err, tt.wantErrorContain)
				}
			},
		)
	}
}

func TestBuildTokenDataOmitsExpireWhenExpiresInZero(t *testing.T) {
	tokenData := buildTokenData("access", "refresh", "", "Bearer", 0, "user@x.ai", "sub-1")
	if tokenData.Expire != "" {
		t.Fatalf("Expire = %q, want empty", tokenData.Expire)
	}
	tokenData = buildTokenData("access", "refresh", "", "Bearer", 60, "user@x.ai", "sub-1")
	if tokenData.Expire == "" {
		t.Fatal("Expire empty, want RFC3339 timestamp")
	}
}

func xaiJSONResponse(req *http.Request, status int, payload any) *http.Response {
	body, _ := json.Marshal(payload)
	return &http.Response{
		StatusCode: status,
		Body:       io.NopCloser(strings.NewReader(string(body))),
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Request:    req,
	}
}
