package management

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	"github.com/gin-gonic/gin"
)

func TestOAuthSessionCompleteKeepsShortTombstone(t *testing.T) {
	store := newOAuthSessionStore(5 * time.Minute)
	store.register("state-1", "xai", "nonce-1")
	store.complete("state-1")

	session, ok := store.get("state-1")
	if !ok || !session.Completed {
		t.Fatalf("completed session = %#v ok=%v", session, ok)
	}
	if session.Status != "" || session.Saving {
		t.Fatalf("completed session state = %#v", session)
	}
	firstExpiry := session.ExpiresAt

	store.complete("state-1")
	session, ok = store.get("state-1")
	if !ok || !session.ExpiresAt.Equal(firstExpiry) {
		t.Fatalf("repeated complete expiry = %s, want %s", session.ExpiresAt, firstExpiry)
	}
	if store.isPending("state-1", "xai") {
		t.Fatal("completed session remained pending")
	}
	if store.cancel("state-1") {
		t.Fatal("completed session was cancelled")
	}
}

func TestOAuthSessionCancelStopsPollingAndPreventsSave(t *testing.T) {
	store := newOAuthSessionStore(time.Minute)
	store.register("state-1", "xai", "")
	ctx, cancel := context.WithCancel(context.Background())
	if !store.attachCancel("state-1", "xai", cancel) {
		t.Fatal("attachCancel returned false")
	}

	if !store.cancel("state-1") {
		t.Fatal("cancel returned false")
	}
	select {
	case <-ctx.Done():
	case <-time.After(time.Second):
		t.Fatal("poll context was not cancelled")
	}
	if store.cancel("state-1") {
		t.Fatal("repeated cancel returned true")
	}
	if store.claimForSave("state-1", "xai") {
		t.Fatal("cancelled session was claimed for save")
	}
}

func TestOAuthSessionSaveClaimMakesCancellationDeterministic(t *testing.T) {
	store := newOAuthSessionStore(time.Minute)
	store.register("state-1", "xai", "")
	ctx, cancel := context.WithCancel(context.Background())
	if !store.attachCancel("state-1", "xai", cancel) {
		t.Fatal("attachCancel returned false")
	}
	if !store.claimForSave("state-1", "xai") {
		t.Fatal("claimForSave returned false")
	}
	select {
	case <-ctx.Done():
	case <-time.After(time.Second):
		t.Fatal("poll context was not cancelled after save claim")
	}
	if store.cancel("state-1") {
		t.Fatal("saving session reported successful cancellation")
	}
	if store.claimForSave("state-1", "xai") {
		t.Fatal("saving session was claimed twice")
	}

	store.complete("state-1")
	session, ok := store.get("state-1")
	if !ok || !session.Completed || session.Saving {
		t.Fatalf("completed session = %#v ok=%v", session, ok)
	}
}

func TestOAuthSessionSaveClaimRefreshesExpiry(t *testing.T) {
	store := newOAuthSessionStore(time.Second)
	store.register("state-1", "xai", "")

	store.mu.Lock()
	session := store.sessions["state-1"]
	oldExpiry := time.Now().Add(10 * time.Millisecond)
	session.ExpiresAt = oldExpiry
	store.sessions["state-1"] = session
	store.mu.Unlock()

	if !store.claimForSave("state-1", "xai") {
		t.Fatal("claimForSave returned false")
	}
	claimed, ok := store.get("state-1")
	if !ok || !claimed.Saving {
		t.Fatalf("claimed session = %#v ok=%v", claimed, ok)
	}
	if !claimed.ExpiresAt.After(oldExpiry) {
		t.Fatalf("saving expiry = %s, want after old expiry %s", claimed.ExpiresAt, oldExpiry)
	}

	time.Sleep(20 * time.Millisecond)
	store.complete("state-1")
	completed, ok := store.get("state-1")
	if !ok || !completed.Completed || completed.Saving {
		t.Fatalf("completed session after original expiry = %#v ok=%v", completed, ok)
	}
}

func TestOAuthSessionErrorStopsPollingAndRejectsSave(t *testing.T) {
	store := newOAuthSessionStore(time.Minute)
	store.register("state-1", "xai", "")
	ctx, cancel := context.WithCancel(context.Background())
	if !store.attachCancel("state-1", "xai", cancel) {
		t.Fatal("attachCancel returned false")
	}
	store.setError("state-1", "denied")

	select {
	case <-ctx.Done():
	case <-time.After(time.Second):
		t.Fatal("poll context was not cancelled after session error")
	}
	session, ok := store.get("state-1")
	if !ok || session.Status != "denied" {
		t.Fatalf("errored session = %#v ok=%v", session, ok)
	}
	if store.claimForSave("state-1", "xai") {
		t.Fatal("errored session was claimed for save")
	}
}

func TestOAuthSessionExpiryStopsPolling(t *testing.T) {
	store := newOAuthSessionStore(20 * time.Millisecond)
	store.register("state-1", "xai", "")
	ctx, cancel := context.WithCancel(context.Background())
	if !store.attachCancel("state-1", "xai", cancel) {
		t.Fatal("attachCancel returned false")
	}
	time.Sleep(30 * time.Millisecond)
	if _, ok := store.get("state-1"); ok {
		t.Fatal("expired session still exists")
	}
	select {
	case <-ctx.Done():
	case <-time.After(time.Second):
		t.Fatal("poll context was not cancelled after expiry")
	}
}

func TestGuardOAuthSessionPendingForSaveClaimsSession(t *testing.T) {
	previous := oauthSessions
	oauthSessions = newOAuthSessionStore(time.Minute)
	t.Cleanup(func() { oauthSessions = previous })

	registerOAuthSession("state-1", "xai", "nonce-1")
	if err := guardOAuthSessionPendingForSave("state-1", "xai"); err != nil {
		t.Fatalf("guardOAuthSessionPendingForSave() error = %v", err)
	}
	if err := guardOAuthSessionPendingForSave("state-1", "xai"); !errors.Is(err, errOAuthSessionNotPending) {
		t.Fatalf("second guard error = %v, want errOAuthSessionNotPending", err)
	}
	if cancelOAuthSession("state-1") {
		t.Fatal("claimed session reported successful cancellation")
	}
}

func TestWriteOAuthCallbackFileRejectsCancelledSession(t *testing.T) {
	previous := oauthSessions
	oauthSessions = newOAuthSessionStore(time.Minute)
	t.Cleanup(func() { oauthSessions = previous })

	registerOAuthSessionForAuthDir("state-1", "codex", "", t.TempDir())
	if !cancelOAuthSession("state-1") {
		t.Fatal("cancelOAuthSession returned false")
	}
	_, err := WriteOAuthCallbackFileForPendingSession("codex", "state-1", "code", "")
	if !errors.Is(err, errOAuthSessionNotPending) {
		t.Fatalf("WriteOAuthCallbackFileForPendingSession() error = %v", err)
	}
}

func TestPostOAuthCallbackUsesSessionAuthDirectory(t *testing.T) {
	previous := oauthSessions
	oauthSessions = newOAuthSessionStore(time.Minute)
	t.Cleanup(func() { oauthSessions = previous })

	authDirAtStart := t.TempDir()
	currentAuthDir := t.TempDir()
	registerOAuthSessionForAuthDir("state-1", "codex", "", authDirAtStart)
	body := []byte(`{"provider":"codex","state":"state-1","code":"callback-code"}`)
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodPost, "/oauth-callback", bytes.NewReader(body))
	ctx.Request.Header.Set("Content-Type", "application/json")
	(&Handler{cfg: &config.Config{AuthDir: currentAuthDir}}).PostOAuthCallback(ctx)
	if recorder.Code != http.StatusOK {
		t.Fatalf("PostOAuthCallback() status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	fileName := ".oauth-codex-state-1.oauth"
	if _, errStat := os.Stat(filepath.Join(authDirAtStart, fileName)); errStat != nil {
		t.Fatalf("callback file missing from session auth directory: %v", errStat)
	}
	if _, errStat := os.Stat(filepath.Join(currentAuthDir, fileName)); !errors.Is(errStat, os.ErrNotExist) {
		t.Fatalf("callback file written to current auth directory: %v", errStat)
	}
}

func TestNormalizeOAuthProviderIncludesDeviceProviders(t *testing.T) {
	for input, want := range map[string]string{
		"grok": "xai",
		"kimi": "kimi",
	} {
		got, err := normalizeOAuthProvider(input)
		if err != nil || got != want {
			t.Fatalf("normalizeOAuthProvider(%q) = %q, %v; want %q", input, got, err, want)
		}
	}
}

func TestGetAuthStatusRejectsUnknownState(t *testing.T) {
	previous := oauthSessions
	oauthSessions = newOAuthSessionStore(time.Minute)
	t.Cleanup(func() { oauthSessions = previous })

	response := performOAuthSessionRequest(t, http.MethodGet, "/?state=missing", (&Handler{}).GetAuthStatus)
	if response.Code != http.StatusOK {
		t.Fatalf("status code = %d, want %d", response.Code, http.StatusOK)
	}
	payload := decodeOAuthSessionResponse(t, response)
	if payload["status"] != "error" || payload["error"] != "unknown or expired state" {
		t.Fatalf("response = %#v", payload)
	}
}

func TestGetAuthStatusReturnsCompletedTombstone(t *testing.T) {
	previous := oauthSessions
	oauthSessions = newOAuthSessionStore(time.Minute)
	t.Cleanup(func() { oauthSessions = previous })

	registerOAuthSession("state-1", "xai", "nonce-1")
	completeOAuthSession("state-1")
	response := performOAuthSessionRequest(
		t,
		http.MethodGet,
		"/?state=state-1&nonce=nonce-1",
		(&Handler{}).GetAuthStatus,
	)
	payload := decodeOAuthSessionResponse(t, response)
	if payload["status"] != "ok" {
		t.Fatalf("response = %#v", payload)
	}
}

func TestCancelAuthSessionRequiresMatchingNonce(t *testing.T) {
	previous := oauthSessions
	oauthSessions = newOAuthSessionStore(time.Minute)
	t.Cleanup(func() { oauthSessions = previous })

	registerOAuthSession("state-1", "xai", "expected-nonce")
	ctx, cancel := context.WithCancel(context.Background())
	if !setOAuthSessionCancel("state-1", "xai", cancel) {
		t.Fatal("setOAuthSessionCancel returned false")
	}

	response := performOAuthSessionRequest(t, http.MethodDelete, "/?state=state-1", (&Handler{}).CancelAuthSession)
	if response.Code != http.StatusUnauthorized {
		t.Fatalf("missing nonce status = %d, want %d", response.Code, http.StatusUnauthorized)
	}
	select {
	case <-ctx.Done():
		t.Fatal("missing nonce cancelled polling")
	default:
	}

	response = performOAuthSessionRequest(
		t, http.MethodDelete, "/?state=state-1&nonce=wrong", (&Handler{}).CancelAuthSession,
	)
	if response.Code != http.StatusUnauthorized {
		t.Fatalf("wrong nonce status = %d, want %d", response.Code, http.StatusUnauthorized)
	}
	select {
	case <-ctx.Done():
		t.Fatal("wrong nonce cancelled polling")
	default:
	}

	response = performOAuthSessionRequest(
		t, http.MethodDelete, "/?state=state-1&nonce=expected-nonce", (&Handler{}).CancelAuthSession,
	)
	payload := decodeOAuthSessionResponse(t, response)
	if payload["status"] != "ok" || payload["cancelled"] != true {
		t.Fatalf("matching nonce response = %#v", payload)
	}
	select {
	case <-ctx.Done():
	case <-time.After(time.Second):
		t.Fatal("matching nonce did not cancel polling")
	}
}

func TestCancelAuthSessionStopsPollingAndInvalidatesState(t *testing.T) {
	previous := oauthSessions
	oauthSessions = newOAuthSessionStore(time.Minute)
	t.Cleanup(func() { oauthSessions = previous })

	registerOAuthSession("state-1", "xai", "")
	ctx, cancel := context.WithCancel(context.Background())
	if !setOAuthSessionCancel("state-1", "xai", cancel) {
		t.Fatal("setOAuthSessionCancel returned false")
	}

	response := performOAuthSessionRequest(t, http.MethodDelete, "/?state=state-1", (&Handler{}).CancelAuthSession)
	payload := decodeOAuthSessionResponse(t, response)
	if payload["status"] != "ok" || payload["cancelled"] != true {
		t.Fatalf("response = %#v", payload)
	}
	select {
	case <-ctx.Done():
	case <-time.After(time.Second):
		t.Fatal("poll context was not cancelled")
	}

	response = performOAuthSessionRequest(t, http.MethodGet, "/?state=state-1", (&Handler{}).GetAuthStatus)
	payload = decodeOAuthSessionResponse(t, response)
	if payload["status"] != "error" {
		t.Fatalf("status response = %#v", payload)
	}
}

func performOAuthSessionRequest(
	t *testing.T,
	method string,
	target string,
	handler gin.HandlerFunc,
) *httptest.ResponseRecorder {
	t.Helper()
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(method, target, nil)
	handler(ctx)
	return recorder
}

func decodeOAuthSessionResponse(t *testing.T, recorder *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var payload map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	return payload
}
