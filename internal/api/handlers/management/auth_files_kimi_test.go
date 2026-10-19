package management

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/auth/kimi"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	sdkAuth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/auth"
	coreauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
	"github.com/gin-gonic/gin"
)

func TestSaveKimiOAuthRecordSurvivesClaimCancellation(t *testing.T) {
	previousSessions := oauthSessions
	oauthSessions = newOAuthSessionStore(time.Minute)
	t.Cleanup(func() { oauthSessions = previousSessions })

	authDir := t.TempDir()
	state := "kimi-fixture-state"
	registerOAuthSessionForAuthDir(state, "kimi", "", authDir)
	pollCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if !setOAuthSessionCancel(state, "kimi", cancel) {
		t.Fatal("could not attach Kimi poll cancellation")
	}
	store := sdkAuth.NewFileTokenStore()
	store.SetBaseDir(authDir)
	handler := &Handler{tokenStore: store}
	fileName := "kimi-fixture.json"
	savedPath, err := handler.saveOAuthTokenRecord(pollCtx, authDir, state, "kimi", &coreauth.Auth{
		ID: fileName, FileName: fileName, Provider: "kimi",
		Metadata: map[string]any{"type": "kimi", "access_token": "fixture-token"},
	})
	if err != nil {
		t.Fatalf("save Kimi OAuth credential after claim: %v", err)
	}
	if pollCtx.Err() != context.Canceled {
		t.Fatalf("claim did not cancel polling: %v", pollCtx.Err())
	}
	if savedPath != filepath.Join(authDir, fileName) {
		t.Fatalf("Kimi credential path = %q", savedPath)
	}
	if _, err := os.Stat(savedPath); err != nil {
		t.Fatalf("Kimi credential not saved: %v", err)
	}
}

func TestRequestKimiAITokenPublishesDomainCredential(t *testing.T) {
	previousSessions := oauthSessions
	oauthSessions = newOAuthSessionStore(time.Minute)
	t.Cleanup(func() { oauthSessions = previousSessions })

	originalTransport := http.DefaultTransport
	http.DefaultTransport = roundTripFunc(func(req *http.Request) (*http.Response, error) {
		var body string
		switch req.URL.String() {
		case "https://auth.kimi.ai/api/oauth/device_authorization":
			body = `{"device_code":"fixture-device","user_code":"FIXTURE","verification_uri":"https://auth.kimi.ai/device","expires_in":30,"interval":1}`
		case "https://auth.kimi.ai/api/oauth/token":
			body = `{"access_token":"fixture-kimi-ai-token","refresh_token":"fixture-refresh-token","expires_in":3600}`
		default:
			return nil, errors.New("unexpected Kimi OAuth request")
		}
		return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body))}, nil
	})
	t.Cleanup(func() { http.DefaultTransport = originalTransport })

	authDir := t.TempDir()
	store := sdkAuth.NewFileTokenStore()
	store.SetBaseDir(authDir)
	manager := coreauth.NewManager(nil, nil, nil)
	handler := &Handler{cfg: &config.Config{AuthDir: authDir}, authManager: manager, tokenStore: store}

	response := performOAuthSessionRequest(t, http.MethodGet, "/kimi-ai-auth-url?nonce=fixture-nonce", handler.RequestKimiAIToken)
	if response.Code != http.StatusOK {
		t.Fatalf("request Kimi.ai token: status %d body %s", response.Code, response.Body.String())
	}
	payload := decodeOAuthSessionResponse(t, response)
	state, _ := payload["state"].(string)
	if !strings.HasPrefix(state, "kmi-ai-") || payload["flow"] != "device" {
		t.Fatalf("unexpected Kimi.ai device response: %#v", payload)
	}

	deadline := time.After(9 * time.Second)
	for {
		_, status, completed, ok := getOAuthSessionDetails(state)
		if !ok || completed || status != "" {
			if !ok || !completed || status != "" {
				t.Fatalf("Kimi.ai authorization did not complete: exists=%v completed=%v status=%q", ok, completed, status)
			}
			break
		}
		select {
		case <-deadline:
			cancelOAuthSession(state)
			t.Fatal("Kimi.ai authorization did not finish")
		case <-time.After(10 * time.Millisecond):
		}
	}
	status := performOAuthSessionRequest(t, http.MethodGet, "/auth-status?state="+state+"&nonce=fixture-nonce", handler.GetAuthStatus)
	if result := decodeOAuthSessionResponse(t, status); result["status"] != "ok" || result["phase"] != "success" {
		t.Fatalf("completed Kimi.ai session response: %#v", result)
	}

	auths := manager.List()
	if len(auths) != 1 || auths[0].Provider != "kimi-ai" || auths[0].Metadata["domain"] != kimi.KimiAIDomain ||
		auths[0].Metadata["base_url"] != kimi.KimiAIAPIBaseURL {
		t.Fatalf("runtime Kimi.ai credential: %+v", auths)
	}
	path := auths[0].Attributes[coreauth.AttributePath]
	if !strings.HasPrefix(filepath.Base(path), "kimi-ai-") {
		t.Fatalf("Kimi.ai credential path = %q", path)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read Kimi.ai credential: %v", err)
	}
	var saved map[string]any
	if err := json.Unmarshal(data, &saved); err != nil {
		t.Fatalf("decode Kimi.ai credential: %v", err)
	}
	if saved["type"] != "kimi-ai" || saved["domain"] != kimi.KimiAIDomain ||
		saved["base_url"] != kimi.KimiAIAPIBaseURL || saved["access_token"] != "fixture-kimi-ai-token" {
		t.Fatalf("saved Kimi.ai credential fields: %#v", saved)
	}
}

func TestRequestKimiTokenRejectsUnknownOAuthDomain(t *testing.T) {
	h := &Handler{}
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodGet, "/v0/management/kimi-auth-url?domain=other.example", nil)
	h.RequestKimiToken(ctx)
	if recorder.Code != http.StatusBadRequest || !strings.Contains(recorder.Body.String(), "unsupported Kimi domain") {
		t.Fatalf("invalid OAuth domain response = %d, %s", recorder.Code, recorder.Body.String())
	}
}
