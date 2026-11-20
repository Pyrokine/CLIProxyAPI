package management

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	sdkAuth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/auth"
	coreauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/pluginapi"
)

type phaseFileStore struct {
	coreauth.Store
	entered, release chan struct{}
	fail             bool
	calls            atomic.Int32
}

func (s *phaseFileStore) Save(ctx context.Context, auth *coreauth.Auth) (string, error) {
	s.calls.Add(1)
	close(s.entered)
	select {
	case <-s.release:
	case <-ctx.Done():
		return "", ctx.Err()
	}
	if s.fail {
		return "", errors.New("injected file publication failure")
	}
	return s.Store.Save(ctx, auth)
}

type phaseAuthHook struct {
	coreauth.Hook
	entered, release chan struct{}
}

func (h *phaseAuthHook) OnAuthRegistered(context.Context, *coreauth.Auth) {
	close(h.entered)
	<-h.release
}

func TestOAuthStatusReportsRealExchangeAndPublicationPhases(t *testing.T) {
	for _, scenario := range []string{"success", "file_failure", "runtime_failure", "cancel_exchange"} {
		t.Run(
			scenario, func(t *testing.T) {
				isolateDevinOAuthSessions(t)
				dir := t.TempDir()
				state := "phase-" + scenario
				registerOAuthSessionForAuthDir(state, "devin", "nonce", dir)
				store := &phaseFileStore{
					Store: sdkAuth.NewFileTokenStore(), entered: make(chan struct{}), release: make(chan struct{}),
					fail: scenario == "file_failure",
				}
				hook := &phaseAuthHook{entered: make(chan struct{}), release: make(chan struct{})}
				manager := coreauth.NewManager(store, nil, hook)
				h := NewHandlerWithoutConfigFilePath(
					&config.Config{AuthDir: dir, Port: 8317}, manager, withTokenStore(store),
				)
				if scenario == "runtime_failure" {
					h.SetPostAuthPersistHook(
						func(
							context.Context,
							*coreauth.Auth,
						) error {
							return errors.New("injected runtime publication failure")
						},
					)
				}
				exchangeEntered, exchangeRelease := make(chan struct{}), make(chan struct{})
				service := &fakeDevinOAuthService{
					exchange: func(ctx context.Context, _, _ string) (string, error) {
						close(exchangeEntered)
						select {
						case <-exchangeRelease:
							return "synthetic-token", nil
						case <-ctx.Done():
							return "", ctx.Err()
						}
					},
				}
				done := make(chan struct{})
				go func() {
					defer close(done)
					h.completeDevinOAuth(context.Background(), dir, state, "synthetic-verifier", service, nil)
				}()
				router := newDevinOAuthTestRouter(h)
				request := func(method, path, body string) map[string]any {
					t.Helper()
					rec := httptest.NewRecorder()
					req := httptest.NewRequest(method, path, strings.NewReader(body))
					req.Header.Set("Content-Type", "application/json")
					router.ServeHTTP(rec, req)
					if rec.Code != http.StatusOK {
						t.Fatalf("HTTP %d: %s", rec.Code, rec.Body.String())
					}
					return decodeDevinOAuthResponse(t, rec)
				}
				status := func() map[string]any {
					return request(http.MethodGet, "/get-auth-status?state="+state+"&nonce=nonce", "")
				}
				if got := status(); got["phase"] != "pending" {
					t.Fatalf("initial=%v", got)
				}
				callback := request(
					http.MethodPost, "/oauth-callback",
					`{"provider":"devin","state":"`+state+`","code":"synthetic-code"}`,
				)
				if callback["phase"] != "accepted" {
					t.Fatalf("callback=%v", callback)
				}
				<-exchangeEntered
				if got := status(); got["status"] != "wait" || got["phase"] != "exchanging" {
					t.Fatalf("exchange=%v", got)
				}
				if scenario == "cancel_exchange" {
					if got := request(
						http.MethodDelete, "/cancel-auth?state="+state+"&nonce=nonce", "",
					); got["cancelled"] != true {
						t.Fatalf("cancel=%v", got)
					}
					<-done
					if got := status(); got["phase"] != "expired" {
						t.Fatalf("after cancel=%v", got)
					}
					if store.calls.Load() != 0 || len(manager.List()) != 0 {
						t.Fatal("cancelled exchange published a credential")
					}
					return
				}
				close(exchangeRelease)
				<-store.entered
				if got := status(); got["status"] != "wait" || got["phase"] != "publishing_file" {
					t.Fatalf("file publication=%v", got)
				}
				if _, err := os.Stat(filepath.Join(dir, "devin-test.json")); !os.IsNotExist(err) {
					t.Fatalf("credential published before store release: %v", err)
				}
				if len(manager.List()) != 0 {
					t.Fatal("runtime published before file")
				}
				if got := request(
					http.MethodDelete, "/cancel-auth?state="+state+"&nonce=nonce", "",
				); got["cancelled"] != false {
					t.Fatalf("publication cancellation=%v", got)
				}
				close(store.release)
				if scenario == "file_failure" {
					<-done
					if got := status(); got["status"] != "error" || got["phase"] != "error" {
						t.Fatalf("file failure=%v", got)
					}
					if len(manager.List()) != 0 {
						t.Fatal("failed file publication registered runtime auth")
					}
					return
				}
				<-hook.entered
				if got := status(); got["status"] != "wait" || got["phase"] != "publishing_auth" {
					t.Fatalf("runtime publication=%v", got)
				}
				if _, err := os.Stat(filepath.Join(dir, "devin-test.json")); err != nil {
					t.Fatalf("file not published: %v", err)
				}
				if _, ok := manager.GetByID("devin-test.json"); !ok {
					t.Fatal("manager registration hook has no published auth")
				}
				close(hook.release)
				<-done
				got := status()
				if scenario == "runtime_failure" {
					if got["status"] != "error" || got["phase"] != "error" {
						t.Fatalf("runtime failure=%v", got)
					}
				} else if got["status"] != "ok" || got["phase"] != "success" {
					t.Fatalf("completed=%v", got)
				}
			},
		)
	}
}

func TestPluginOAuthPollingPhaseDoesNotBlockStatusOrDuplicatePoll(t *testing.T) {
	isolateDevinOAuthSessions(t)
	dir := t.TempDir()
	state := "plugin-blocked-phase"
	if err := registerPluginOAuthSession(state, "custom-sso", "nonce", dir, nil); err != nil {
		t.Fatal(err)
	}
	entered, release := make(chan struct{}), make(chan struct{})
	var calls atomic.Int32
	host := &managementPluginOAuthHost{
		provider: "custom-sso",
		poll: func(ctx context.Context, _, _ string, _ map[string]any) (pluginapi.AuthLoginPollResponse, error) {
			if calls.Add(1) == 1 {
				close(entered)
				select {
				case <-release:
				case <-ctx.Done():
					return pluginapi.AuthLoginPollResponse{}, ctx.Err()
				}
			}
			return pluginapi.AuthLoginPollResponse{Status: pluginapi.AuthLoginStatusPending}, nil
		},
	}
	h := NewHandlerWithoutConfigFilePath(&config.Config{AuthDir: dir}, nil)
	h.pluginHost = host
	firstDone := make(chan struct{})
	go func() {
		defer close(firstDone)
		session, _ := getOAuthSessionIntegrationDetails(state)
		h.pollPluginOAuthSession(
			withOAuthSessionContext(context.Background(), state, "custom-sso"), state, session, oauthSessions,
		)
	}()
	<-entered
	rec := performOAuthSessionRequest(t, http.MethodGet, "/?state="+state+"&nonce=nonce", h.GetAuthStatus)
	payload := decodeOAuthSessionResponse(t, rec)
	close(release)
	<-firstDone
	if payload["status"] != "wait" || payload["phase"] != "exchanging" || calls.Load() != 1 {
		t.Fatalf("status=%v calls=%d", payload, calls.Load())
	}
	session, ok := getOAuthSessionIntegrationDetails(state)
	if !ok || session.Polling || oauthSessionPhase(session) != "pending" {
		t.Fatalf("released session=%+v", session)
	}
}
