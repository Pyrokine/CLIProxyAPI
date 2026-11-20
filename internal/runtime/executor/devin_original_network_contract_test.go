package executor

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	devinauth "github.com/Pyrokine/CLIProxyAPI/v6/internal/auth/devin"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	cliproxyauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
	"google.golang.org/protobuf/encoding/protowire"
)

func TestDevinExecutorPrepareRequest(t *testing.T) {
	exec := NewDevinExecutor(&config.Config{})
	auth := &cliproxyauth.Auth{
		Attributes: map[string]string{
			"api_key": "my-secret-key",
		},
	}
	req, err := http.NewRequest(http.MethodPost, "https://server.codeium.com/test", nil)
	if err != nil {
		t.Fatalf("NewRequest failed: %v", err)
	}

	if err := exec.PrepareRequest(req, auth); err != nil {
		t.Fatalf("PrepareRequest failed: %v", err)
	}

	authHeader := req.Header.Get("Authorization")
	if authHeader != "Basic my-secret-key-my-secret-key" {
		t.Fatalf("Authorization = %q, want %q", authHeader, "Basic my-secret-key-my-secret-key")
	}
	if req.Header.Get("Content-Type") != "application/connect+proto" {
		t.Fatalf("Content-Type = %q, want application/connect+proto", req.Header.Get("Content-Type"))
	}
	if req.Header.Get("Connect-Protocol-Version") != "1" {
		t.Fatalf("Connect-Protocol-Version = %q, want 1", req.Header.Get("Connect-Protocol-Version"))
	}
	if req.Header.Get("Accept") != "*/*" {
		t.Fatalf("Accept = %q, want */*", req.Header.Get("Accept"))
	}
	sentryTrace := req.Header.Get("Sentry-Trace")
	if sentryTrace == "" {
		t.Fatalf("Sentry-Trace header missing")
	}
	parts := strings.Split(sentryTrace, "-")
	if len(parts) != 3 || len(parts[0]) != 32 || len(parts[1]) != 16 || parts[2] != "1" {
		t.Fatalf("invalid Sentry-Trace format: %q", sentryTrace)
	}

	// Verify User-Agent suppression on the wire
	var receivedUA []string
	ts := httptest.NewServer(
		http.HandlerFunc(
			func(w http.ResponseWriter, r *http.Request) {
				receivedUA = r.Header["User-Agent"]
			},
		),
	)
	defer ts.Close()

	wireReq, err := http.NewRequest(http.MethodPost, ts.URL, nil)
	if err != nil {
		t.Fatalf("NewRequest failed: %v", err)
	}
	if err := exec.PrepareRequest(wireReq, auth); err != nil {
		t.Fatalf("PrepareRequest failed: %v", err)
	}
	resp, err := ts.Client().Do(wireReq)
	if err != nil {
		t.Fatalf("Do request failed: %v", err)
	}
	_ = resp.Body.Close()

	if len(receivedUA) != 0 {
		t.Errorf("expected User-Agent to be completely omitted on wire, got: %v", receivedUA)
	}
}

func TestDevinExecutor_Refresh(t *testing.T) {
	// Build mock protobuf response
	var planInfo []byte
	planInfo = protowire.AppendTag(planInfo, 2, protowire.BytesType)
	planInfo = protowire.AppendString(planInfo, "Pro")

	var orgInfo []byte
	orgInfo = protowire.AppendTag(orgInfo, 4, protowire.BytesType)
	orgInfo = protowire.AppendString(orgInfo, "org-test-devin")
	orgInfo = protowire.AppendTag(orgInfo, 8, protowire.BytesType)
	orgInfo = protowire.AppendString(orgInfo, "XCodeCLI")
	planInfo = protowire.AppendTag(planInfo, 33, protowire.BytesType)
	planInfo = protowire.AppendBytes(planInfo, orgInfo)

	var planStatus []byte
	planStatus = protowire.AppendTag(planStatus, 1, protowire.BytesType)
	planStatus = protowire.AppendBytes(planStatus, planInfo)
	planStatus = protowire.AppendTag(planStatus, 14, protowire.VarintType)
	planStatus = protowire.AppendVarint(planStatus, 95)
	planStatus = protowire.AppendTag(planStatus, 15, protowire.VarintType)
	planStatus = protowire.AppendVarint(planStatus, 45)
	planStatus = protowire.AppendTag(planStatus, 17, protowire.VarintType)
	planStatus = protowire.AppendVarint(planStatus, 1789200000)
	planStatus = protowire.AppendTag(planStatus, 18, protowire.VarintType)
	planStatus = protowire.AppendVarint(planStatus, 1789286400)

	var userStatus []byte
	userStatus = protowire.AppendTag(userStatus, 3, protowire.BytesType)
	//goland:noinspection SpellCheckingInspection
	userStatus = protowire.AppendString(userStatus, "refreshuser")
	userStatus = protowire.AppendTag(userStatus, 5, protowire.BytesType)
	userStatus = protowire.AppendString(userStatus, "team-xyz")
	userStatus = protowire.AppendTag(userStatus, 7, protowire.BytesType)
	userStatus = protowire.AppendString(userStatus, "refreshuser@example.com")
	userStatus = protowire.AppendTag(userStatus, 13, protowire.BytesType)
	userStatus = protowire.AppendBytes(userStatus, planStatus)
	userStatus = protowire.AppendTag(userStatus, 36, protowire.BytesType)
	userStatus = protowire.AppendString(userStatus, "user-id-999")

	var mockResp []byte
	mockResp = protowire.AppendTag(mockResp, 1, protowire.BytesType)
	mockResp = protowire.AppendBytes(mockResp, userStatus)

	server := httptest.NewServer(
		http.HandlerFunc(
			func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != devinauth.GetUserStatusPath {
					http.NotFound(w, r)
					return
				}
				w.Header().Set("Content-Type", "application/proto")
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write(mockResp)
			},
		),
	)
	defer server.Close()

	cfg := &config.Config{}
	exec := NewDevinExecutor(cfg)

	auth := &cliproxyauth.Auth{
		ID:       "devin-refresh.json",
		Provider: "devin",
		Attributes: map[string]string{
			"api_key":  "devin-session-token$test",
			"base_url": server.URL,
		},
		Metadata: map[string]any{
			"api_key":  "devin-session-token$test",
			"base_url": server.URL,
		},
	}

	updated, err := exec.Refresh(context.Background(), auth)
	if err != nil {
		t.Fatalf("exec.Refresh failed: %v", err)
	}

	if updated.Metadata["plan"] != "Pro" {
		t.Errorf("expected plan Pro, got %v", updated.Metadata["plan"])
	}
	if updated.Metadata["email"] != "refreshuser@example.com" {
		t.Errorf("expected email refreshuser@example.com, got %v", updated.Metadata["email"])
	}
	//goland:noinspection SpellCheckingInspection
	if updated.Metadata["user_name"] != "refreshuser" {
		//goland:noinspection SpellCheckingInspection
		t.Errorf("expected user_name refreshuser, got %v", updated.Metadata["user_name"])
	}
	if updated.Metadata["daily_quota_remaining_percent"] != nil {
		t.Errorf(
			"expected daily quota to not be in metadata, got %v", updated.Metadata["daily_quota_remaining_percent"],
		)
	}
	if updated.Metadata["weekly_quota_remaining_percent"] != nil {
		t.Errorf(
			"expected weekly quota to not be in metadata, got %v", updated.Metadata["weekly_quota_remaining_percent"],
		)
	}
	if updated.Quota.Signals["daily_quota_remaining_percent"] != "95%" {
		t.Errorf("expected quota signal 95%%, got %q", updated.Quota.Signals["daily_quota_remaining_percent"])
	}
	if updated.Quota.Signals["weekly_quota_remaining_percent"] != "45%" {
		t.Errorf("expected quota signal 45%%, got %q", updated.Quota.Signals["weekly_quota_remaining_percent"])
	}
	if updated.Quota.ObservedAt.IsZero() {
		t.Error("expected non-zero Quota.ObservedAt")
	}
}
