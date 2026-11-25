package management

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	coreauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/pluginapi"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

func pluginMutationEngine(t *testing.T, h *Handler) *gin.Engine {
	t.Helper()
	engine := resourceMutationEngine(t, h)
	engine.GET("/v0/management/get-auth-status", h.GetAuthStatus)
	engine.GET("/v0/management/mutations/:mutation_id", h.GetMutationResult)
	engine.POST("/v0/management/oauth-callback", h.PostOAuthCallback)
	engine.DELETE("/v0/management/oauth-session", h.CancelAuthSession)
	engine.NoRoute(
		func(c *gin.Context) {
			if !h.ServePluginAuthURL(c) {
				c.Status(http.StatusNotFound)
			}
		},
	)
	return engine
}

func waitPluginOAuthTerminal(t *testing.T, sessions *oauthSessionStore, state string) oauthSession {
	t.Helper()
	deadline := time.NewTimer(3 * time.Second)
	defer deadline.Stop()
	ticker := time.NewTicker(time.Millisecond)
	defer ticker.Stop()
	for {
		if session, ok := sessions.get(state); ok && (session.Completed || session.Status != "") {
			return session
		}
		select {
		case <-deadline.C:
			t.Fatal("plugin OAuth worker did not reach a terminal state")
		case <-ticker.C:
		}
	}
}

func TestMutationPluginOAuthCreationRunsWorkerAndKeepsStatusReadOnly(t *testing.T) {
	for _, failSave := range []bool{false, true} {
		t.Run(
			map[bool]string{false: "saved", true: "rolled_back"}[failSave], func(t *testing.T) {
				isolateDevinOAuthSessions(t)
				sessions := oauthSessions
				dir := t.TempDir()
				oldPath := filepath.Join(dir, "first.json")
				old := []byte(`{"type":"custom-sso","value":"old"}`)
				if err := os.WriteFile(oldPath, old, 0o600); err != nil {
					t.Fatal(err)
				}
				store := &pluginOAuthFileStore{persisted: map[string][]byte{oldPath: old}}
				if failSave {
					store.failAt = 2
				}
				manager := coreauth.NewManager(store, nil, nil)
				previous, _, err := authFromFile(dir, oldPath, old)
				if err != nil {
					t.Fatal(err)
				}
				if _, err = manager.Register(coreauth.WithSkipPersist(context.Background()), previous); err != nil {
					t.Fatal(err)
				}
				var starts, polls atomic.Int32
				entered, release := make(chan struct{}), make(chan struct{})
				host := &managementPluginOAuthHost{provider: "custom-sso"}
				host.start = func(context.Context, string, string, map[string]any) (
					pluginapi.AuthLoginStartResponse,
					error,
				) {
					starts.Add(1)
					return pluginapi.AuthLoginStartResponse{
						Provider: "custom-sso", State: "worker-state", URL: "https://login.example.test",
						Metadata: map[string]any{"exchange": "synthetic"},
					}, nil
				}
				host.poll = func(
					ctx context.Context,
					provider, state string,
					metadata map[string]any,
				) (pluginapi.AuthLoginPollResponse, error) {
					polls.Add(1)
					close(entered)
					if provider != "custom-sso" || state != "worker-state" ||
						metadata["exchange"] != "synthetic" {
						return pluginapi.AuthLoginPollResponse{}, errors.New(
							"unexpected captured plugin metadata",
						)
					}
					if mutationFromContext(ctx) != nil {
						return pluginapi.AuthLoginPollResponse{}, errors.New(
							"creation mutation leaked to worker",
						)
					}
					select {
					case <-release:
					case <-ctx.Done():
						return pluginapi.AuthLoginPollResponse{}, ctx.Err()
					}
					return pluginapi.AuthLoginPollResponse{
						Status: pluginapi.AuthLoginStatusSuccess, Auths: []pluginapi.AuthData{
							{
								FileName:    "first.json",
								StorageJSON: []byte(`{"type":"custom-sso","value":"new"}`),
							},
							{
								FileName:    "second.json",
								StorageJSON: []byte(`{"type":"custom-sso","value":"second"}`),
							},
						},
					}, nil
				}
				h := &Handler{
					cfg: &config.Config{AuthDir: dir, Port: 8317}, pluginHost: host, tokenStore: store,
					authManager: manager,
				}
				engine := pluginMutationEngine(t, h)
				id := uuid.NewString()
				requestCtx, cancelRequest := context.WithCancel(context.Background())
				request := httptest.NewRequest(
					http.MethodGet, "/v0/management/custom-sso-auth-url?nonce=browser-nonce", nil,
				).WithContext(requestCtx)
				request.RemoteAddr = "127.0.0.1:12345"
				request.Header.Set(MutationIDHeader, id)
				response := httptest.NewRecorder()
				engine.ServeHTTP(response, request)
				cancelRequest()
				receipt := decodeMutationHeader(t, response)
				if response.Code != 200 || receipt.Outcome != "succeeded" || !receipt.AuditAvailable ||
					!reflect.DeepEqual(
						receipt.Effects, []MutationEffect{{Kind: "task.created", Target: "task"}},
					) {
					t.Fatalf("creation=%d %+v", response.Code, receipt)
				}
				select {
				case <-entered:
				case <-time.After(3 * time.Second):
					t.Fatal("worker did not start independently of status GET")
				}
				var route, target, resource string
				if err := h.stateStore.db.QueryRow(
					"SELECT route,target,resource FROM management_mutation_audit WHERE "+
						"mutation_id=? ORDER BY id DESC LIMIT "+
						"1",
					id,
				).Scan(
					&route, &target, &resource,
				); err !=
					nil ||
					route !=
						pluginOAuthMutationRoute ||
					target !=
						"provider:custom-sso" ||
					resource !=
						"credential" {
					t.Fatalf("route=%q target=%q err=%v", route, target, err)
				}
				if duplicate := mutationResultRequest(
					engine, http.MethodGet, request.URL.String(), id,
				); duplicate.Code != 409 || starts.Load() != 1 {
					t.Fatalf("duplicate=%d starts=%d", duplicate.Code, starts.Load())
				}
				for range 3 {
					read := mutationResultRequest(
						engine,
						http.MethodGet,
						"/v0/management/get-auth-status?state=worker-state&nonce=browser-nonce",
						uuid.NewString(),
					)
					payload := decodeOAuthSessionResponse(t, read)
					if payload["status"] != "wait" || payload["phase"] != "exchanging" ||
						read.Header().Get(MutationResultHeader) != "" ||
						polls.Load() != 1 {
						t.Fatalf("read=%v polls=%d headers=%v", payload, polls.Load(), read.Header())
					}
				}
				denied := mutationResultRequest(
					engine, http.MethodGet, "/v0/management/get-auth-status?state=worker-state&nonce=wrong", "",
				)
				if denied.Code != 401 || polls.Load() != 1 {
					t.Fatalf("nonce check=%d polls=%d", denied.Code, polls.Load())
				}
				callResourceMutation(
					t,
					engine,
					http.MethodPost,
					"/v0/management/oauth-callback",
					`{"provider":"custom-sso","state":"worker-state","code":"synthetic-code"}`,
					200,
					"succeeded",
					"task.persisted",
				)
				otherDir := t.TempDir()
				h.mu.Lock()
				h.cfg = &config.Config{AuthDir: otherDir, Port: 8317}
				h.mu.Unlock()
				close(release)
				session := waitPluginOAuthTerminal(t, sessions, "worker-state")
				if session.Completed == failSave {
					t.Fatalf("terminal=%+v failSave=%t", session, failSave)
				}
				current, exists := manager.GetByID(previous.ID)
				if !exists ||
					current.Metadata["value"] != map[bool]string{false: "new", true: "old"}[failSave] {
					t.Fatalf("runtime restoration/publication changed: exists=%t", exists)
				}
				_, secondErr := os.Stat(filepath.Join(dir, "second.json"))
				if failSave && !errors.Is(secondErr, os.ErrNotExist) || !failSave && secondErr != nil {
					t.Fatalf("second credential: %v", secondErr)
				}
				if files, err := os.ReadDir(otherDir); err != nil || len(files) != 0 {
					t.Fatalf("worker changed captured AuthDir: files=%d err=%v", len(files), err)
				}
				readback := mutationResultRequest(engine, http.MethodGet, "/v0/management/mutations/"+id, "")
				var persisted MutationResult
				if err := json.Unmarshal(readback.Body.Bytes(), &persisted); err != nil || !reflect.DeepEqual(
					persisted, receipt,
				) {
					t.Fatalf(
						"creation receipt changed after async save: %s err=%v",
						readback.Body.String(),
						err,
					)
				}
			},
		)
	}
}

func TestMutationPluginOAuthValidationAndInactiveGET(t *testing.T) {
	for _, scenario := range []string{
		"unknown_provider", "ordinary_get", "invalid_uuid", "provider_error", "wrong_provider", "invalid_state",
		"duplicate_state",
	} {
		t.Run(
			scenario, func(t *testing.T) {
				isolateDevinOAuthSessions(t)
				dir := t.TempDir()
				var starts atomic.Int32
				host := &managementPluginOAuthHost{
					provider: "custom-sso", start: func(context.Context, string, string, map[string]any) (
						pluginapi.AuthLoginStartResponse,
						error,
					) {
						starts.Add(1)
						response := pluginapi.AuthLoginStartResponse{
							Provider: "custom-sso",
							State:    "validation-state",
						}
						switch scenario {
						case "provider_error":
							return response, errors.New("synthetic plugin start failure")
						case "wrong_provider":
							response.Provider = "other-sso"
						case "invalid_state":
							response.State = "../invalid"
						}
						return response, nil
					},
				}
				h := &Handler{cfg: &config.Config{AuthDir: dir, Port: 8317}, pluginHost: host}
				engine := pluginMutationEngine(t, h)
				if scenario == "duplicate_state" {
					if err := registerPluginOAuthSession("validation-state", "custom-sso", "", dir, nil); err != nil {
						t.Fatal(err)
					}
				}
				path, id := "/v0/management/custom-sso-auth-url", uuid.NewString()
				wantStatus, wantStarts := 502, int32(1)
				switch scenario {
				case "unknown_provider":
					path, wantStatus, wantStarts = "/v0/management/inactive-auth-url", 404, 0
				case "ordinary_get":
					path, wantStatus, wantStarts = "/v0/management/ordinary", 404, 0
				case "invalid_uuid":
					id, wantStatus, wantStarts = "invalid-uuid", 400, 0
				}
				response := mutationResultRequest(engine, http.MethodGet, path, id)
				if response.Code != wantStatus || starts.Load() != wantStarts {
					t.Fatalf(
						"status=%d starts=%d body=%s",
						response.Code,
						starts.Load(),
						response.Body.String(),
					)
				}
				if scenario == "unknown_provider" || scenario == "ordinary_get" {
					if response.Header().Get(MutationResultHeader) != "" {
						t.Fatal("unknown GET received a mutation identity")
					}
					return
				}
				result := decodeMutationHeader(t, response)
				if result.ConfirmedEffect || len(result.Effects) != 0 ||
					scenario == "invalid_uuid" && result.Outcome != "rejected" {
					t.Fatalf("invalid creation fabricated effects: %+v", result)
				}
			},
		)
	}
}

func TestPluginOAuthWorkerCancellationExpiryAndReusedState(t *testing.T) {
	for _, scenario := range []string{"cancelled", "expired", "reused_state"} {
		t.Run(
			scenario, func(t *testing.T) {
				isolateDevinOAuthSessions(t)
				sessions := oauthSessions
				if scenario == "expired" {
					sessions.ttl = 20 * time.Millisecond
				}
				dir := t.TempDir()
				if err := registerPluginOAuthSession("late-state", "custom-sso", "nonce", dir, nil); err != nil {
					t.Fatal(err)
				}
				session, _ := sessions.get("late-state")
				ctx, cancel := context.WithDeadline(
					withOAuthSessionContext(
						context.Background(), "late-state", "custom-sso",
					), session.ExpiresAt,
				)
				defer cancel()
				if !sessions.attachCancel("late-state", "custom-sso", cancel) {
					t.Fatal("worker cancel was not attached")
				}
				entered, release, done := make(chan struct{}), make(chan struct{}), make(chan struct{})
				host := &managementPluginOAuthHost{
					provider: "custom-sso", poll: func(context.Context, string, string, map[string]any) (
						pluginapi.AuthLoginPollResponse,
						error,
					) {
						close(entered)
						<-release
						return pluginapi.AuthLoginPollResponse{
							Status: pluginapi.AuthLoginStatusSuccess, Auth: pluginapi.AuthData{
								FileName: "late.json", StorageJSON: []byte(`{"type":"custom-sso"}`),
							},
						}, nil
					},
				}
				h := &Handler{
					cfg:        &config.Config{AuthDir: dir},
					pluginHost: host,
					tokenStore: &pluginOAuthFileStore{},
				}
				go func() {
					defer close(done)
					h.pollPluginOAuthSession(ctx, "late-state", session, sessions)
				}()
				<-entered
				if scenario == "expired" {
					<-ctx.Done()
					sessions.get("late-state")
				} else if !sessions.cancel("late-state") {
					t.Fatal("pending worker was not cancellable")
				}
				if scenario == "reused_state" {
					if err := sessions.registerPlugin(
						"late-state", "custom-sso", "new-nonce", dir, nil, [32]byte{},
					); err != nil {
						t.Fatal(err)
					}
				}
				close(release)
				<-done
				if files, err := os.ReadDir(dir); err != nil || len(files) != 0 {
					t.Fatalf(
						"late cancelled/expired worker saved a credential: files=%d err=%v",
						len(files),
						err,
					)
				}
				if scenario == "reused_state" {
					sessions.setError("late-state", "stale error", session.CreatedAt)
					sessions.complete("late-state", session.CreatedAt)
					if sessions.claimForSave("late-state", "custom-sso", session.CreatedAt) {
						t.Fatal("old worker claimed the replacement session")
					}
					fresh, exists := sessions.get("late-state")
					if !exists || fresh.Completed || fresh.Status != "" || fresh.Saving || fresh.Polling ||
						!verifyOAuthNonce(
							"late-state", "new-nonce",
						) {
						t.Fatalf("old worker changed replacement session: %+v", fresh)
					}
				}
			},
		)
	}
}

func TestPluginOAuthWorkerReportsFailuresAndPendingMessage(t *testing.T) {
	for _, scenario := range []string{
		"pending", "poll_error", "provider_error", "invalid_status", "wrong_auth_provider", "empty_auths",
		"unavailable_provider",
	} {
		t.Run(
			scenario, func(t *testing.T) {
				isolateDevinOAuthSessions(t)
				sessions := oauthSessions
				dir := t.TempDir()
				if err := registerPluginOAuthSession("failure-state", "custom-sso", "nonce", dir, nil); err != nil {
					t.Fatal(err)
				}
				session, _ := sessions.get("failure-state")
				var calls atomic.Int32
				host := &managementPluginOAuthHost{
					provider: "custom-sso", poll: func(context.Context, string, string, map[string]any) (
						pluginapi.AuthLoginPollResponse,
						error,
					) {
						calls.Add(1)
						switch scenario {
						case "pending":
							return pluginapi.AuthLoginPollResponse{
								Status: pluginapi.AuthLoginStatusPending, Message: "Waiting for synthetic approval",
							}, nil
						case "poll_error":
							return pluginapi.AuthLoginPollResponse{}, errors.New("synthetic poll failure")
						case "provider_error":
							return pluginapi.AuthLoginPollResponse{
								Status: pluginapi.AuthLoginStatusError, Message: "synthetic provider failure",
							}, nil
						case "invalid_status":
							return pluginapi.AuthLoginPollResponse{Status: "invalid"}, nil
						case "empty_auths":
							return pluginapi.AuthLoginPollResponse{
								Status: pluginapi.AuthLoginStatusSuccess, Auths: []pluginapi.AuthData{},
							}, nil
						default:
							return pluginapi.AuthLoginPollResponse{
								Status: pluginapi.AuthLoginStatusSuccess,
								Auth:   pluginapi.AuthData{Provider: "other-sso", FileName: "invalid.json"},
							}, nil
						}
					},
				}
				if scenario == "unavailable_provider" {
					host.provider = "unavailable"
				}
				h := &Handler{cfg: &config.Config{AuthDir: dir}, pluginHost: host}
				pending := h.pollPluginOAuthSession(
					withOAuthSessionContext(
						context.Background(), "failure-state", "custom-sso",
					), "failure-state", session, sessions,
				)
				if pending != (scenario == "pending") {
					t.Fatalf("pending=%t", pending)
				}
				before := calls.Load()
				read := performOAuthSessionRequest(
					t, http.MethodGet, "/?state=failure-state&nonce=nonce", h.GetAuthStatus,
				)
				payload := decodeOAuthSessionResponse(t, read)
				if scenario == "pending" {
					if payload["status"] != "wait" || payload["message"] != "Waiting for synthetic approval" ||
						payload["phase"] != "pending" {
						t.Fatalf("pending response=%v", payload)
					}
				} else if payload["status"] != "error" || payload["phase"] != "error" || payload["error"] == "" {
					t.Fatalf("error response=%v", payload)
				}
				if calls.Load() != before {
					t.Fatal("status GET called the plugin")
				}
				if files, err := os.ReadDir(dir); err != nil || len(files) != 0 {
					t.Fatalf("invalid plugin response wrote files=%d err=%v", len(files), err)
				}
			},
		)
	}
}
