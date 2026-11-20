package management

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	coreauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
	"github.com/gin-gonic/gin"
)

type originalBatchRefreshExecutor struct {
	bulkRefreshExecutor
}

func (*originalBatchRefreshExecutor) Identifier() string { return "antigravity" }

func (e *originalBatchRefreshExecutor) Refresh(_ context.Context, auth *coreauth.Auth) (*coreauth.Auth, error) {
	e.calls.Add(1)
	current := e.current.Add(1)
	defer e.current.Add(-1)
	for {
		maximum := e.maxConcurrent.Load()
		if current <= maximum || e.maxConcurrent.CompareAndSwap(maximum, current) {
			break
		}
	}
	if e.entered != nil {
		e.entered <- struct{}{}
	}
	if e.release != nil {
		<-e.release
	}
	if auth.Metadata == nil {
		auth.Metadata = make(map[string]any)
	}
	auth.Metadata["access_token"] = "rotated-access-" + auth.ID
	auth.Metadata["refresh_token"] = "rotated-refresh-" + auth.ID
	return auth, nil
}

type originalBatchRefreshStore struct {
	memoryAuthStore
	onSave func(context.Context)
}

func (s *originalBatchRefreshStore) Save(ctx context.Context, auth *coreauth.Auth) (string, error) {
	if s.onSave != nil {
		s.onSave(ctx)
	}
	return s.memoryAuthStore.Save(ctx, auth)
}

func TestOriginalForceRefreshBatchInputs(t *testing.T) {
	for _, name := range []string{"refreshable only", "bounded workers", "already canceled", "cancel during exchange"} {
		t.Run(
			name, func(t *testing.T) {
				gin.SetMode(gin.TestMode)
				type contextKey struct{}
				ctx, cancel := context.WithCancel(context.WithValue(t.Context(), contextKey{}, "refresh-value"))
				defer cancel()
				store := &originalBatchRefreshStore{}
				if name == "cancel during exchange" {
					store.onSave = func(ctx context.Context) {
						if ctx.Err() != nil || ctx.Value(contextKey{}) != "refresh-value" {
							t.Error(
								"successful refresh persistence lost its context value or inherited cancellation",
							)
						}
					}
				}
				manager := coreauth.NewManager(store, nil, nil)
				manager.SetHomeClientProvider(func() coreauth.HomeClient { return nil })
				cfg := &config.Config{AuthAutoRefreshWorkers: 2}
				manager.SetConfig(cfg)
				executor := &originalBatchRefreshExecutor{}
				blocked := name == "bounded workers" || name == "cancel during exchange"
				if blocked {
					executor.entered, executor.release = make(chan struct{}, 6), make(chan struct{})
				}
				manager.RegisterExecutor(executor)
				count := 6
				if name == "refreshable only" {
					count = 2
				}
				for index := range count {
					id := fmt.Sprintf("ag-%d", index)
					metadata := map[string]any{"refresh_token": fmt.Sprintf("ref-%d", index)}
					if name == "refreshable only" {
						id = fmt.Sprintf("ag-%d", index+1)
						metadata["refresh_token"] = fmt.Sprintf("ref-%d", index+1)
					}
					if name == "cancel during exchange" {
						metadata["access_token"], metadata["refresh_token"] = "old-access", "old-refresh"
					}
					if _, err := manager.Register(
						coreauth.WithSkipPersist(t.Context()), &coreauth.Auth{
							ID: id, Provider: "antigravity", Status: coreauth.StatusActive, Metadata: metadata,
						},
					); err != nil {
						t.Fatal(err)
					}
				}
				if name == "refreshable only" {
					if _, err := manager.Register(
						coreauth.WithSkipPersist(t.Context()), &coreauth.Auth{
							ID: "ag-no-ref", Provider: "antigravity",
							Metadata: map[string]any{"access_token": "no-refresh"},
						},
					); err != nil {
						t.Fatal(err)
					}
				}
				handler := NewHandlerWithoutConfigFilePath(cfg, manager)
				router := gin.New()
				router.POST("/auth-files/refresh", handler.RefreshAuthFile)
				if name == "already canceled" {
					cancel()
				}
				request := httptest.NewRequest(
					http.MethodPost, "/auth-files/refresh", strings.NewReader(`{"all":true}`),
				).WithContext(ctx)
				request.Header.Set("Content-Type", "application/json")
				response := httptest.NewRecorder()
				done := make(chan struct{})
				go func() {
					defer close(done)
					router.ServeHTTP(response, request)
				}()
				release := sync.OnceFunc(
					func() {
						if executor.release != nil {
							close(executor.release)
						}
					},
				)
				defer func() { release(); <-done }()
				if blocked {
					for range 2 {
						select {
						case <-executor.entered:
						case <-done:
							t.Fatalf("batch ended before workers entered: %s", response.Body.String())
						case <-time.After(5 * time.Second):
							t.Fatal("original batch workers did not enter")
						}
					}
					if got := executor.calls.Load(); got != 2 {
						t.Fatalf("blocked batch entered=%d want=2", got)
					}
					if name == "cancel during exchange" {
						cancel()
					}
					release()
				}
				select {
				case <-done:
				case <-time.After(5 * time.Second):
					t.Fatal("original batch did not finish")
				}
				if response.Code != http.StatusOK {
					t.Fatalf("batch status=%d body=%s", response.Code, response.Body.String())
				}
				var body authFileRefreshBatchResponse
				if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil ||
					len(body.Results) != count {
					t.Fatalf("batch count=%d want=%d error=%v", len(body.Results), count, err)
				}
				succeeded, canceled := 0, 0
				for _, result := range body.Results {
					current, ok := manager.GetByID(result.FileName)
					if !ok || current == nil || result.Provider != "antigravity" || result.AuthIndex == "" {
						t.Fatalf("batch lost exact public identity: %+v", result)
					}
					store.mu.Lock()
					persisted := store.items[current.ID].Clone()
					store.mu.Unlock()
					if result.Success {
						succeeded++
						if result.Error != "" {
							t.Fatalf("successful refresh has an error: %+v", result)
						}
						for source, auth := range map[string]*coreauth.Auth{"manager": current, "store": persisted} {
							if auth == nil || auth.Metadata["access_token"] != "rotated-access-"+current.ID ||
								auth.Metadata["refresh_token"] != "rotated-refresh-"+current.ID {
								t.Errorf("%s lost rotated material for %s", source, current.ID)
							}
						}
					} else {
						canceled++
						if result.Error != "credential_refresh_cancelled" || persisted != nil {
							t.Fatalf("queued refresh result=%+v persisted=%t", result, persisted != nil)
						}
						if name ==
							("cancel during "+
								"exchange") &&
							(current.Metadata["access_token"] !=
								"old-access" ||
								current.Metadata["refresh_token"] !=
									"old-refresh") {
							t.Fatal("unstarted refresh changed the original material")
						}
					}
				}
				wantSuccess, wantCanceled := count, 0
				if name == "already canceled" {
					wantSuccess, wantCanceled = 0, 6
				}
				if name == "cancel during exchange" {
					wantSuccess, wantCanceled = 2, 4
				}
				if succeeded != wantSuccess || canceled != wantCanceled ||
					executor.calls.Load() != int32(wantSuccess) ||
					executor.maxConcurrent.Load() > 2 {
					t.Fatalf(
						"success=%d canceled=%d entered=%d maximum=%d",
						succeeded,
						canceled,
						executor.calls.Load(),
						executor.maxConcurrent.Load(),
					)
				}
				if name == "refreshable only" {
					skipped, _ := manager.GetByID("ag-no-ref")
					if skipped == nil || skipped.Metadata["access_token"] != "no-refresh" ||
						skipped.Metadata["refresh_token"] != nil {
						t.Fatal("credential without a refresh token was changed")
					}
				}
			},
		)
	}
}
