package handlers

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"path/filepath"
	"sync"
	"testing"
	"time"

	internalconfig "github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/constant"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/usage"
	coreauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
	coreexecutor "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
	coreusage "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/usage"
	sdkconfig "github.com/Pyrokine/CLIProxyAPI/v6/sdk/config"
)

func TestAPIKeyPolicySDKAndUsageShareSQLiteReservation(t *testing.T) {
	for _, channel := range []struct{ provider, allowed string }{
		{"openai", "openai"},
		{"openai-compatible-policy-fixture", "policy-fixture"},
	} {
		t.Run(
			channel.provider, func(t *testing.T) {
				dir := t.TempDir()
				persister := usage.NewPersister(dir, internalconfig.UsageRetention{})
				persister.Start(t.Context())
				t.Cleanup(persister.Stop)
				if !persister.IsReady() {
					t.Fatal("temporary policy/usage store did not initialize")
				}
				db, err := sql.Open("sqlite", filepath.Join(dir, "events.db"))
				if err != nil {
					t.Fatal(err)
				}
				defer func() { _ = db.Close() }()
				plugin := usage.NewLoggerPlugin(persister, nil, func() bool { return true })
				const model = "fixture-real-sqlite-policy-model"
				apiKey := t.Name()
				var mu sync.Mutex
				var reservations []string
				var executions int
				executor := &lifecycleCaptureExecutor{
					provider: channel.provider,
					execute: func(
						context.Context,
						*coreauth.Auth,
						coreexecutor.Request,
						coreexecutor.Options,
					) (coreexecutor.Response, error) {
						t.Error("a denied API key policy reached the executor")
						return coreexecutor.Response{}, errors.New("unexpected execution after policy denial")
					},
					stream: func(
						executionContext context.Context,
						_ *coreauth.Auth,
						_ coreexecutor.Request,
						_ coreexecutor.Options,
					) (*coreexecutor.StreamResult, error) {
						mu.Lock()
						executions++
						attempt := executions
						reservation := coreusage.APIKeyReservationFromContext(executionContext)
						reservations = append(reservations, reservation)
						mu.Unlock()
						chunks := make(chan coreexecutor.StreamChunk, 1)
						if attempt == 1 {
							chunks <- coreexecutor.StreamChunk{Err: apiKeyBootstrapRequestError{}}
						} else {
							plugin.HandleUsage(
								executionContext, coreusage.Record{
									Provider: channel.provider, Model: model,
									APIKey:        coreusage.APIKeyIdentityFromContext(executionContext),
									ReservationID: reservation,
									ExecutionID:   coreusage.ExecutionIDFromContext(executionContext),
									RequestID:     "fixture-real-policy-storage", RequestedAt: time.Now().UTC(),
									Detail: coreusage.Detail{InputTokens: 4, OutputTokens: 3, TotalTokens: 7},
								},
							)
							chunks <- coreexecutor.StreamChunk{
								Payload: []byte("data: {\"model\":\"" +
									model +
									"\"}\n\n"),
							}
						}
						close(chunks)
						return &coreexecutor.StreamResult{Chunks: chunks}, nil
					},
				}
				handler := newLifecycleHandler(t, model, executor)
				handler.UpdateClients(
					&sdkconfig.SDKConfig{
						Streaming: sdkconfig.StreamingConfig{BootstrapRetries: 1},
						APIKeyRules: map[string]internalconfig.APIKeyRule{
							apiKey: {
								Providers: []string{channel.allowed},
								Limits:    []internalconfig.APIKeyPolicyLimit{{Window: "1h", Requests: 1}},
							},
						},
					},
				)
				handler.SetAPIKeyPolicyStore(persister)
				chunks, _, failures := handler.ExecuteStreamChunksWithAuthManager(
					apiKeyPolicyContext(apiKey, "fixture-real-policy-retry"), constant.OpenAI, model,
					[]byte(`{"model":"`+model+`","stream":true}`), "",
				)
				payloads, terminals := 0, 0
				for chunk := range chunks {
					if chunk.Err != nil || chunk.Acknowledge == nil {
						t.Fatalf(
							"unexpected policy stream chunk: error=%v acknowledgement=%t", chunk.Err,
							chunk.Acknowledge != nil,
						)
					}
					if chunk.Terminal {
						terminals++
					} else {
						payloads++
					}
					chunk.Acknowledge(nil)
				}
				for failure := range failures {
					if failure != nil {
						t.Fatalf("policy stream failed: %v", failure)
					}
				}
				mu.Lock()
				attempts := executions
				ids := append([]string(nil), reservations...)
				mu.Unlock()
				if attempts != 2 || len(ids) != 2 || ids[0] == "" || ids[0] != ids[1] || payloads != 1 ||
					terminals != 1 {
					t.Fatalf(
						"attempts=%d reservations=%v payloads=%d terminals=%d",
						attempts,
						ids,
						payloads,
						terminals,
					)
				}
				ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
				defer cancel()
				ticker := time.NewTicker(20 * time.Millisecond)
				defer ticker.Stop()
				for {
					var reservationCount, eventCount, settlementCount int
					if err := db.QueryRowContext(
						ctx, "SELECT COUNT(*) FROM api_key_reservation",
					).Scan(&reservationCount); err != nil {
						t.Fatal(err)
					}
					if err := db.QueryRowContext(ctx, "SELECT COUNT(*) FROM events").Scan(&eventCount); err != nil {
						t.Fatal(err)
					}
					if err := db.QueryRowContext(
						ctx, "SELECT COUNT(*) FROM api_key_reservation_event",
					).Scan(&settlementCount); err != nil {
						t.Fatal(err)
					}
					if reservationCount == 1 && eventCount == 1 && settlementCount == 1 {
						var state string
						var requests, tokens int64
						if err := db.QueryRowContext(
							ctx, "SELECT state FROM api_key_reservation WHERE id=?", ids[0],
						).Scan(&state); err != nil {
							t.Fatal(err)
						}
						if err := db.QueryRowContext(
							ctx,
							"SELECT actual_requests, actual_tokens FROM api_key_reservation_window "+
								"WHERE reservation_id=?",
							ids[0],
						).Scan(&requests, &tokens); err != nil {
							t.Fatal(err)
						}
						if state != "settled" || requests != 1 || tokens != 7 {
							t.Fatalf("reservation state/requests/tokens=%s/%d/%d", state, requests, tokens)
						}
						break
					}
					select {
					case <-ctx.Done():
						t.Fatalf(
							"reservation/events/settlements=%d/%d/%d: %v", reservationCount, eventCount,
							settlementCount, ctx.Err(),
						)
					case <-ticker.C:
					}
				}
				_, _, failure := handler.ExecuteWithAuthManager(
					apiKeyPolicyContext(apiKey, "fixture-limit-exceeded"), constant.OpenAI, model,
					[]byte(`{"model":"`+model+`"}`), "",
				)
				if failure == nil || failure.StatusCode != http.StatusTooManyRequests ||
					!failure.DirectResponse {
					t.Fatalf("settled request did not consume the single-request budget: %v", failure)
				}
				persister.Stop()
				_, _, failure = handler.ExecuteWithAuthManager(
					apiKeyPolicyContext(apiKey, "fixture-store-stopped"), constant.OpenAI, model,
					[]byte(`{"model":"`+model+`"}`), "",
				)
				if failure == nil || failure.StatusCode != http.StatusServiceUnavailable ||
					!failure.DirectResponse {
					t.Fatalf("stopped policy store did not reject with 503: %v", failure)
				}
			},
		)
	}
}
