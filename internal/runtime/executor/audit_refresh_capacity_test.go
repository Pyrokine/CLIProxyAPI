package executor

import (
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/registry"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/util"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
	core "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/usage"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/translator"
)

func TestAuditAntigravityStreamUsageTracksRefreshedCredential(t *testing.T) {
	for _, mode := range []string{"active", "refreshed", "read-error", "refresh-rejected"} {
		t.Run(
			mode, func(t *testing.T) {
				plugin := &captureUsagePlugin{records: make(chan usage.Record, 2)}
				registerTestUsagePlugin(t, plugin)
				expiry := time.Now().Add(-time.Hour)
				if mode == "active" {
					expiry = time.Now().Add(time.Hour)
				}
				credential := &auth.Auth{
					ID: t.Name(), Provider: "antigravity",
					Attributes: map[string]string{"base_url": "https://antigravity.synthetic.invalid"},
					Metadata: map[string]any{
						"access_token": "synthetic-old-access", "refresh_token": "synthetic-refresh-" + t.Name(),
						"project_id": "synthetic-project", "expired": expiry.Format(time.RFC3339),
					},
				}
				oldHash := auth.AccessTokenSHA256(credential)
				wantToken := "synthetic-new-access"
				if mode == "active" || mode == "refresh-rejected" {
					wantToken = "synthetic-old-access"
				}
				refreshCalls, upstreamCalls := 0, 0
				var creditsCalls atomic.Int32
				creditsStarted := make(chan struct{}, 1)
				ctx := util.WithRoundTripper(
					t.Context(), antigravityRoundTripFunc(
						func(request *http.Request) (*http.Response, error) {
							if request.URL.Host == "oauth2.googleapis.com" {
								refreshCalls++
								status, body := http.StatusOK, `{"access_token":"synthetic-new-access",`+
									`"expires_in":3600,"token_type":`+
									`"Bearer"}`
								if mode == "refresh-rejected" {
									status, body = http.StatusUnauthorized, `{"error":"invalid_grant"}`
								}
								return &http.Response{
									StatusCode: status, Header: make(http.Header),
									Body: io.NopCloser(strings.NewReader(body)), Request: request,
								}, nil
							}
							if request.URL.Host != "antigravity.synthetic.invalid" ||
								request.Header.Get("Authorization") != "Bearer "+wantToken {
								t.Errorf(
									"upstream=%s authorization=%q",
									request.URL,
									request.Header.Get("Authorization"),
								)
							}
							if request.URL.Path == antigravityLoadCodeAssistPath {
								if creditsCalls.Add(1) == 1 {
									close(creditsStarted)
								}
								return &http.Response{
									StatusCode: http.StatusOK, Header: make(http.Header), Request: request,
									Body: io.NopCloser(
										strings.NewReader(
											`{"paidTier":{"id":"synthetic-tier","availableCredits":[]}}`,
										),
									),
								}, nil
							}
							upstreamCalls++
							wire := "data: {\"response\":{\"candidates\":[{\"content\":{\"role\":\"model\"," +
								"\"parts\":[{\"text\":\"synthetic\"}]},\"finishReason\":\"STOP\"}],\"us" +
								"ageMetadata\":{\"promptTokenCount\":7,\"candidatesTokenCount\":5,\"tot" +
								"alTokenCount\":12}}}\n\n"
							var body io.Reader = strings.NewReader(wire)
							if mode == "read-error" {
								body = io.MultiReader(body, &auditImageFinalRead{err: io.ErrUnexpectedEOF})
							}
							return &http.Response{
								StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"text/event-stream"}},
								Body: io.NopCloser(body), Request: request,
							}, nil
						},
					),
				)
				err := auditFinalUsageCall(
					ctx, NewAntigravityExecutor(&config.Config{}), credential, core.Request{
						Model: "gemini-2.5-flash",
						Payload: []byte(
							`{"request":{"contents":[{"role":"user","parts":[{"text":"synthetic"}]}]}}`,
						),
					}, core.Options{
						SourceFormat: translator.FormatAntigravity, ResponseFormat: translator.FormatAntigravity,
					}, true,
				)
				failed := mode == "read-error" || mode == "refresh-rejected"
				if (err != nil) != failed {
					t.Fatalf("mode=%s error=%v", mode, err)
				}
				r := testWaitForDevinUsageRecord(t, plugin.records)
				wantHash := auth.AccessTokenSHA256(
					&auth.Auth{Metadata: map[string]any{"access_token": wantToken}},
				)
				if r.Failed != failed || r.AuthID != credential.ID || r.AccessTokenSHA256 != wantHash {
					t.Fatalf("record=%+v want token identity=%s", r, wantHash)
				}
				wantRefresh := 1
				if mode == "active" {
					wantRefresh = 0
				}
				wantUpstream := 1
				if mode == "refresh-rejected" {
					wantUpstream = 0
				}
				wantCredits := int32(0)
				if mode == "refreshed" || mode == "read-error" {
					wantCredits = 1
					select {
					case <-creditsStarted:
					case <-time.After(time.Second):
						t.Fatal("refreshed stream did not queue its balance observation")
					}
				}
				if refreshCalls != wantRefresh || upstreamCalls != wantUpstream ||
					creditsCalls.Load() != wantCredits ||
					auth.AccessTokenSHA256(credential) != oldHash {
					t.Fatalf(
						"refresh=%d upstream=%d credits=%d expected credits=%d caller token identity changed=%t",
						refreshCalls,
						upstreamCalls,
						creditsCalls.Load(),
						wantCredits,
						auth.AccessTokenSHA256(credential) != oldHash,
					)
				}
				if wantUpstream > 0 &&
					(r.Detail.InputTokens != 7 || r.Detail.OutputTokens != 5 || r.Detail.TotalTokens != 12) {
					t.Fatalf("observed usage=%+v", r.Detail)
				}
			},
		)
	}
}

func TestAuditCodexCapacityErrorKeepsExplicitStatusAuthority(t *testing.T) {
	for _, nested := range []bool{false, true} {
		for _, streaming := range []bool{false, true} {
			for _, explicit := range []int{0, http.StatusServiceUnavailable} {
				t.Run(
					fmt.Sprintf(
						"nested=%t/stream=%t/status=%d",
						nested,
						streaming,
						explicit,
					),
					func(t *testing.T) {
						payload := `{"type":"error","code":"model_at_capacity","message":"The model is at capacity"}`
						if nested {
							payload = `{"type":"response.failed","sequence_number":7,"response":{"error":{"ty` +
								`pe":"server_error","code":"model_at_capacity","message":"The model is ` +
								`at capacity"}}}`
						}
						if explicit != 0 {
							payload = fmt.Sprintf(`{"status":%d,`, explicit) + payload[1:]
						}
						ctx := util.WithRoundTripper(
							t.Context(), devinTestRoundTripper(
								func(request *http.Request) (*http.Response, error) {
									return &http.Response{
										StatusCode: http.StatusOK,
										Header:     http.Header{"Content-Type": {"text/event-stream"}},
										Body: io.NopCloser(
											strings.NewReader("data: " + payload + "\n\n"),
										),
										Request: request,
									}, nil
								},
							),
						)
						err := auditFinalUsageCall(
							ctx,
							newCodexExecutor(&config.Config{}),
							&auth.Auth{
								ID: t.Name(), Provider: "codex", Attributes: map[string]string{
									"api_key": "synthetic-key", "base_url": "https://codex.synthetic.invalid",
								},
							},
							core.Request{
								Model:   "gpt-5.4",
								Payload: []byte(`{"model":"gpt-5.4","input":"synthetic"}`),
							},
							core.Options{
								SourceFormat: translator.FormatCodex, ResponseFormat: translator.FormatOpenAIResponse,
							},
							streaming,
						)
						if testExecutorStatusCode(err) != http.StatusTooManyRequests || !strings.Contains(
							err.Error(), "model_at_capacity",
						) {
							t.Fatalf("error=%v want=%d", err, http.StatusTooManyRequests)
						}
						if explicit != 0 &&
							!strings.Contains(err.Error(), fmt.Sprintf(`"status":%d`, explicit)) {
							t.Fatalf("upstream status missing from envelope: %v", err)
						}
					},
				)
			}
		}
	}
	for _, streaming := range []bool{false, true} {
		t.Run(
			fmt.Sprintf("explicit_non_capacity/stream=%t", streaming), func(t *testing.T) {
				payload := `{"type":"response.failed","status":503,"response":{"error":{"type":"se` +
					`rver_error","code":"synthetic_unavailable","message":"synthetic server` +
					` unavailable"}}}`
				ctx := util.WithRoundTripper(
					t.Context(), devinTestRoundTripper(
						func(request *http.Request) (*http.Response, error) {
							return &http.Response{
								StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"text/event-stream"}},
								Body: io.NopCloser(
									strings.NewReader("data: " + payload + "\n\n"),
								), Request: request,
							}, nil
						},
					),
				)
				err := auditFinalUsageCall(
					ctx,
					newCodexExecutor(&config.Config{}),
					&auth.Auth{
						ID: t.Name(), Provider: "codex", Attributes: map[string]string{
							"api_key": "synthetic-key", "base_url": "https://codex.synthetic.invalid",
						},
					},
					core.Request{Model: "gpt-5.4", Payload: []byte(`{"model":"gpt-5.4","input":"synthetic"}`)},
					core.Options{
						SourceFormat:   translator.FormatCodex,
						ResponseFormat: translator.FormatOpenAIResponse,
					},
					streaming,
				)
				if testExecutorStatusCode(err) != http.StatusServiceUnavailable || !strings.Contains(
					err.Error(), "synthetic_unavailable",
				) {
					t.Fatalf("error=%v want=%d", err, http.StatusServiceUnavailable)
				}
			},
		)
	}
}

func TestAuditCodexCapacityReachesManagerCredentialRetry(t *testing.T) {
	for _, streaming := range []bool{false, true} {
		t.Run(
			fmt.Sprintf("stream=%t", streaming), func(t *testing.T) {
				manager := auth.NewManager(nil, &auth.FillFirstSelector{}, nil)
				manager.SetHomeClientProvider(func() auth.HomeClient { return nil })
				manager.SetConfig(&config.Config{})
				manager.SetRetryConfig(0, 0, 2)
				manager.RegisterExecutor(newCodexExecutor(&config.Config{}))
				for _, id := range []string{"audit-capacity-a", "audit-capacity-b"} {
					_, err := manager.Register(
						t.Context(), &auth.Auth{
							ID: id, Provider: "codex",
							Attributes: map[string]string{
								"api_key":  id,
								"base_url": "https://codex.synthetic.invalid",
							},
						},
					)
					if err != nil {
						t.Fatal(err)
					}
					registry.GetGlobalRegistry().
						RegisterClient(id, "codex", []*registry.ModelInfo{{ID: "gpt-5.4"}})
					t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(id) })
				}
				var calls []string
				ctx := util.WithRoundTripper(
					t.Context(), devinTestRoundTripper(
						func(request *http.Request) (*http.Response, error) {
							key := strings.TrimPrefix(request.Header.Get("Authorization"), "Bearer ")
							calls = append(calls, key)
							wire := `{"type":"response.failed","response":{"error":{"type":"server_error","` +
								`code":"model_at_capacity","message":"The model is at capacity"}}}`
							if key == "audit-capacity-b" {
								wire = `{"type":"response.completed","response":{"id":"synthetic","status":` +
									`"completed","model":"gpt-5.4","output":` +
									`[]}}`
							}
							return &http.Response{
								StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"text/event-stream"}},
								Body: io.NopCloser(
									strings.NewReader("data: " + wire + "\n\n"),
								), Request: request,
							}, nil
						},
					),
				)
				request := core.Request{
					Model:   "gpt-5.4",
					Payload: []byte(`{"model":"gpt-5.4","input":"synthetic"}`),
				}
				opts := core.Options{
					SourceFormat: translator.FormatCodex, ResponseFormat: translator.FormatOpenAIResponse,
				}
				var err error
				if streaming {
					var result *core.StreamResult
					result, err = manager.ExecuteStream(ctx, []string{"codex"}, request, opts)
					if err == nil {
						for chunk := range result.Chunks {
							if chunk.Err != nil {
								err = chunk.Err
							}
						}
					}
				} else {
					_, err = manager.Execute(ctx, []string{"codex"}, request, opts)
				}
				if err != nil || len(calls) != 2 || calls[0] != "audit-capacity-a" ||
					calls[1] != "audit-capacity-b" {
					t.Fatalf("error=%v credentials=%v", err, calls)
				}
			},
		)
	}
}
