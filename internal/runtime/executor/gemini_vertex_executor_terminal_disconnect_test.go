package executor

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/util"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
	core "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/usage"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/translator"
)

func TestGeminiVertexStream_ClientDisconnectAfterTerminalEventIsNotFailed(t *testing.T) {
	for _, terminal := range []struct{ name, reason, event string }{
		{"completed", "STOP", "response.completed"},
		{"incomplete", "MAX_TOKENS", "response.incomplete"},
	} {
		t.Run(
			terminal.name, func(t *testing.T) {
				wire := "data: " + `{"candidates":[{"content":{"parts":[{"text":"hello"}]},"finishReason":"` +
					terminal.reason + `"}],"usageMetadata":{"promptTokenCount":10,"candidatesTokenCount":5,` +
					(`"totalTokenCount":15},"modelVersion":"gemini-3.7-flash","responseId":` +
						`"vertex-terminal-disconnect"}`) +
					"\n\n"
				for _, provider := range []string{"api_key", "service_account"} {
					t.Run(
						provider, func(t *testing.T) {
							var tokenCalls, streamCalls atomic.Int32
							disconnected := make(chan struct{}, 2)
							server := httptest.NewServer(
								http.HandlerFunc(
									func(w http.ResponseWriter, request *http.Request) {
										if request.URL.Path == "/token" {
											tokenCalls.Add(1)
											w.Header().Set("Content-Type", "application/json")
											_, _ = io.WriteString(
												w,
												`{"access_token":"local-token","token_type":"Bearer",`+
													`"expires_in":`+
													`3600}`,
											)
											return
										}
										streamCalls.Add(1)
										if !strings.HasSuffix(
											request.URL.Path, "/gemini-3.7-flash:streamGenerateContent",
										) {
											t.Errorf("unexpected stream endpoint: %s", request.URL.Path)
										}
										if provider == "api_key" &&
											request.Header.Get("X-Goog-Api-Key") != "test-key" {
											t.Error("API key request lost its selected synthetic credential")
										}
										if provider == "service_account" &&
											request.Header.Get("Authorization") != "Bearer local-token" {
											t.Error("service account request lost its synthetic token")
										}
										w.Header().Set("Content-Type", "text/event-stream")
										_, _ = io.WriteString(w, wire)
										w.(http.Flusher).Flush()
										<-request.Context().Done()
										disconnected <- struct{}{}
									},
								),
							)
							defer server.Close()
							ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
							defer cancel()
							target, err := url.Parse(server.URL)
							if err != nil {
								t.Fatal(err)
							}
							transport := http.DefaultTransport.(*http.Transport).Clone()
							transport.Proxy = nil
							defer transport.CloseIdleConnections()
							ctx = util.WithRoundTripper(
								ctx, devinTestRoundTripper(
									func(request *http.Request) (*http.Response, error) {
										redirected := request.Clone(request.Context())
										redirected.URL.Scheme, redirected.URL.Host = target.Scheme, target.Host
										return transport.RoundTrip(redirected)
									},
								),
							)
							credential := &auth.Auth{ID: t.Name(), Provider: "vertex"}
							if provider == "api_key" {
								credential.Attributes = map[string]string{
									"api_key":  "test-key",
									"base_url": server.URL,
								}
							} else {
								serviceAccount := googleImagenServiceAccount(t)
								credential.Metadata = map[string]any{
									"project_id": "proxy-test", "location": "global", "service_account": serviceAccount,
								}
							}
							capture := &captureUsagePlugin{records: make(chan usage.Record, 2)}
							registerTestUsagePlugin(t, capture)
							result, err := NewGeminiVertexExecutor(&config.Config{}).ExecuteStream(
								ctx, credential,
								core.Request{
									Model: "gemini-3.7-flash", Payload: []byte(`{"input":"hello","stream":true}`),
								},
								core.Options{
									SourceFormat:   translator.FormatOpenAIResponse,
									ResponseFormat: translator.FormatOpenAIResponse, Stream: true,
								},
							)
							if err != nil {
								t.Fatal(err)
							}
							terminalReceived := false
							for chunk := range result.Chunks {
								if chunk.Err != nil {
									t.Fatalf("unexpected stream error: %v", chunk.Err)
								}
								if strings.Contains(string(chunk.Payload), terminal.event) {
									terminalReceived = true
									cancel()
									break
								}
							}
							if !terminalReceived {
								t.Fatalf("expected %s before client cancellation", terminal.event)
							}
							record := testWaitForDevinUsageRecord(t, capture.records)
							if record.Provider != "vertex" || record.AuthID != credential.ID || record.Failed ||
								record.Detail.InputTokens !=
									10 ||
								record.Detail.OutputTokens !=
									5 ||
								record.Detail.TotalTokens !=
									15 {
								t.Fatalf("terminal cancellation usage=%+v, want successful 10/5/15", record)
							}
							select {
							case <-disconnected:
							case <-time.After(time.Second):
								t.Fatal("upstream HTTP request remained open after downstream cancellation")
							}
							select {
							case chunk, open := <-result.Chunks:
								if open {
									t.Fatalf("stream did not finish after cancellation: %+v", chunk)
								}
							case <-time.After(time.Second):
								t.Fatal("executor did not finish after terminal cancellation")
							}
							wantTokenCalls := int32(0)
							if provider == "service_account" {
								wantTokenCalls = 1
							}
							if streamCalls.Load() != 1 || tokenCalls.Load() != wantTokenCalls {
								t.Fatalf(
									"stream calls=%d token calls=%d",
									streamCalls.Load(),
									tokenCalls.Load(),
								)
							}
						},
					)
				}
			},
		)
	}
}
