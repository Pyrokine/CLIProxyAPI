package management

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	coreauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

type resetCleanupFailureStore struct{ coreauth.Store }

func (*resetCleanupFailureStore) Save(context.Context, *coreauth.Auth) (string, error) {
	return "", errors.New("synthetic local cleanup persistence failure")
}

func TestCodexResetOutcomeDistinguishesConsumptionFromLocalCleanup(t *testing.T) {
	for _, name := range []string{
		"reset", "already_redeemed", "binding_changed", "local_cleanup_failed", "already_redeemed_local_cleanup_failed",
		"unknown_code", "invalid_json", "upstream_failure", "probe_failed", "already_redeemed_noop",
	} {
		t.Run(
			name, func(t *testing.T) {
				f := newCodexResetProbeFixture(t, nil)
				status, outcome := 200, "succeeded"
				consumed, phase := true, "completed"
				effects := []string{"quota.consumed", "credential.published"}
				switch name {
				case "already_redeemed":
					f.consumeHook = func(
						w http.ResponseWriter,
						_ *http.Request,
					) {
						_, _ = w.Write([]byte(`{"code":"already_redeemed"}`))
					}
					effects = []string{"credential.published"}
				case "already_redeemed_noop":
					if _, _, err := f.manager.ResetQuota(context.Background(), f.auth.ID); err != nil {
						t.Fatal(err)
					}
					f.consumeHook = func(
						w http.ResponseWriter,
						_ *http.Request,
					) {
						_, _ = w.Write([]byte(`{"code":"already_redeemed"}`))
					}
					effects, outcome = nil, "uncertain"
				case "binding_changed":
					f.consumeHook = func(w http.ResponseWriter, _ *http.Request) {
						next := f.auth.Clone()
						next.Metadata["access_token"] = "synthetic-replacement-after-consumption"
						if _, err := f.manager.Update(context.Background(), next); err != nil {
							t.Error(err)
						}
						_, _ = w.Write([]byte(`{"code":"reset"}`))
					}
					status, outcome, phase, effects = 409, "partial", "local_cleanup", []string{"quota.consumed"}
				case "local_cleanup_failed":
					f.manager.SetStore(&resetCleanupFailureStore{})
					status, outcome, phase, effects = 500, "partial", "local_cleanup", []string{"quota.consumed"}
				case "already_redeemed_local_cleanup_failed":
					f.manager.SetStore(&resetCleanupFailureStore{})
					f.consumeHook = func(
						w http.ResponseWriter,
						_ *http.Request,
					) {
						_, _ = w.Write([]byte(`{"code":"already_redeemed"}`))
					}
					status, outcome, phase, effects = 500, "uncertain", "local_cleanup", nil
				case "unknown_code", "invalid_json", "upstream_failure":
					f.consumeHook = func(w http.ResponseWriter, _ *http.Request) {
						body := `{"code":"unknown"}`
						if name == "invalid_json" {
							body = `{"code":`
						}
						if name == "upstream_failure" {
							w.WriteHeader(503)
							body = `{"code":"reset"}`
						}
						_, _ = w.Write([]byte(body))
					}
					status, outcome, consumed, phase, effects = 502, "uncertain", false, "", nil
				case "probe_failed":
					f.handler.quotaScheduler = nil
					status, outcome, consumed, phase, effects = 502, "uncertain", false, "", nil
				}
				engine := resourceMutationEngine(t, f.handler)
				engine.POST("/reset-quota", f.handler.ResetQuota)
				engine.POST("/routing/cooldown/reset", f.handler.ResetCooldown)
				body := fmt.Sprintf(`{"auth_index":%q}`, f.auth.Index)
				response := callResetOutcomeRequest(t, engine, "/reset-quota", body)
				if response.Code != status {
					t.Fatalf("status=%d want=%d body=%s", response.Code, status, response.Body.String())
				}
				var payload map[string]any
				if err := json.Unmarshal(response.Body.Bytes(), &payload); err != nil {
					t.Fatal(err)
				}
				if consumed {
					if payload["consumed"] != true || payload["phase"] != phase ||
						payload["auth_index"] != f.auth.Index {
						t.Fatalf("confirmed consumption response=%v", payload)
					}
				} else if payload["consumed"] == true || payload["phase"] == "completed" {
					t.Fatalf("unknown consumption claimed success=%v", payload)
				}
				result := decodeMutationHeader(t, response)
				if result.Outcome != outcome || len(result.Effects) != len(effects) {
					t.Fatalf("receipt=%+v want outcome=%s effects=%v", result, outcome, effects)
				}
				for _, effect := range result.Effects {
					found := false
					for _, kind := range effects {
						found = found || effect.Kind == kind
					}
					if !found {
						t.Fatalf("unexpected effect=%+v", effect)
					}
				}
				if name == "probe_failed" && f.consumeCalls.Load() != 0 {
					t.Fatal("read-only failure attempted credit consumption")
				}
				if name == "local_cleanup_failed" {
					assertCodexResetProbeCooldown(t, f)
					f.manager.SetStore(nil)
					recovered := callResetOutcomeRequest(t, engine, "/routing/cooldown/reset", body)
					if recovered.Code != 200 || f.consumeCalls.Load() != 1 || f.probeCalls.Load() != 1 {
						t.Fatalf("local recovery status=%d consume calls=%d", recovered.Code, f.consumeCalls.Load())
					}
					var recoveredPayload map[string]any
					if err := json.Unmarshal(recovered.Body.Bytes(), &recoveredPayload); err != nil {
						t.Fatal(err)
					}
					if _, exists := recoveredPayload["consumed"]; exists {
						t.Fatal("local-only recovery changed its response contract")
					}
					if _, exists := recoveredPayload["phase"]; exists {
						t.Fatal("local-only recovery gained a credit consumption phase")
					}
				}
			},
		)
	}
}

func callResetOutcomeRequest(t *testing.T, engine *gin.Engine, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	request := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-CPA-Mutation-ID", uuid.NewString())
	response := httptest.NewRecorder()
	engine.ServeHTTP(response, request)
	return response
}
