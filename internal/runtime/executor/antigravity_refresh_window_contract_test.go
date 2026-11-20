package executor

import (
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/cache"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/util"
	core "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/translator"
)

func TestAntigravityTokenWindowAndRelativeExpiryAtExecutionEntries(t *testing.T) {
	// 全局缓存清理协程使用真实时钟，避免在 synctest 内创建
	cache.StoreSignature(t.Name(), t.Name(), strings.Repeat("s", 64))
	t.Cleanup(func() { cache.ClearSignatureCache(t.Name()) })
	for _, encoding := range []string{
		"absolute", "legacy milliseconds", "issued seconds", "issued date", "camel issued date",
	} {
		for _, remaining := range []time.Duration{
			5*time.Minute + time.Second, 5 * time.Minute, 5*time.Minute - time.Second, 40 * time.Minute,
		} {
			for _, entry := range []string{"prepare", "HTTP", "generate", "claude aggregate", "stream", "count"} {
				t.Run(
					fmt.Sprintf("%s/%s/%s", encoding, remaining, entry), func(t *testing.T) {
						synctest.Test(
							t, func(t *testing.T) {
								ctx, _ := xaiOutcomeContext(t)
								credential := antigravityCountTokensAuth(
									"https://antigravity-window.synthetic.invalid", t.Name(),
								)
								credential.Metadata["access_token"] = "synthetic-current-token"
								credential.Metadata["refresh_token"] = "synthetic-refresh-" + t.Name()
								delete(credential.Metadata, "expired")
								issued := time.Now().Add(remaining - time.Hour)
								switch encoding {
								case "absolute":
									credential.Metadata["expired"] = time.Now().
										Add(remaining).
										Format(time.RFC3339Nano)
								case "legacy milliseconds":
									credential.Metadata["timestamp"] = issued.UnixMilli()
									credential.Metadata["expires_in"] = int64(3600)
								case "issued seconds":
									credential.Metadata["issued_at"] = issued.Unix()
									credential.Metadata["expires_in"] = int64(3600)
								case "issued date":
									credential.Metadata["issued_at"] = issued.Format(time.RFC3339)
									credential.Metadata["expires_in"] = "3600"
								case "camel issued date":
									credential.Metadata["issuedAt"] = issued.Format(time.RFC3339)
									credential.Metadata["expiresIn"] = "3600"
								}
								wantToken := "synthetic-current-token"
								wantRefresh := 0
								if remaining <= 5*time.Minute {
									wantToken = "synthetic-refreshed-token"
									wantRefresh = 1
								}
								refreshCalls, upstreamCalls, creditsCalls := 0, 0, 0
								transport := &auditClaudeContractTransport{
									respond: func(request *http.Request) (*http.Response, error) {
										if request.URL.Host == "oauth2.googleapis.com" &&
											request.URL.Path == "/token" {
											refreshCalls++
											return &http.Response{
												StatusCode: http.StatusOK, Header: make(http.Header), Request: request,
												Body: io.NopCloser(
													strings.NewReader(
														`{"access_token":"synthetic-refreshed-token",` +
															`"expires_in":3600,"token_type":` +
															`"Bearer"}`,
													),
												),
											}, nil
										}
										if request.URL.Host != "antigravity-window.synthetic.invalid" ||
											request.Header.Get("Authorization") != "Bearer "+wantToken {
											t.Fatalf(
												"unexpected endpoint or token at %s: %s",
												entry,
												request.URL,
											)
										}
										if request.URL.Path == antigravityLoadCodeAssistPath {
											creditsCalls++
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
										return &http.Response{
											StatusCode: http.StatusUnauthorized, Header: make(http.Header),
											Request: request,
											Body: io.NopCloser(
												strings.NewReader(
													`{"error":{"message":"synthetic window rejection"}}`,
												),
											),
										}, nil
									},
								}
								ctx = util.WithRoundTripper(ctx, transport)
								cfg := &config.Config{}
								if newAntigravityHTTPClient(ctx, cfg, credential, 0).Transport != transport {
									t.Fatal("Antigravity refresh window transport is not memory-isolated")
								}
								executor := NewAntigravityExecutor(cfg)
								request := core.Request{
									Model: "gemini-2.5-flash",
									Payload: []byte(
										`{"request":{"contents":[{"role":"user","parts":[{"text":"synthetic"}]}]}}`,
									),
								}
								opts := core.Options{
									SourceFormat:   translator.FormatAntigravity,
									ResponseFormat: translator.FormatAntigravity,
								}
								var err error
								switch entry {
								case "prepare", "HTTP":
									wire, errRequest := http.NewRequestWithContext(
										ctx,
										http.MethodPost,
										"https://antigravity-window.synthetic.invalid/synthetic",
										nil,
									)
									if errRequest != nil {
										t.Fatal(errRequest)
									}
									if entry == "prepare" {
										err = executor.PrepareRequest(wire, credential)
										if wire.Header.Get("Authorization") != "Bearer "+wantToken {
											t.Errorf("prepared token does not match the request window")
										}
									} else {
										var response *http.Response
										response, err = executor.HttpRequest(ctx, credential, wire)
										if response != nil {
											_ = response.Body.Close()
										}
									}
								case "count":
									_, err = executor.CountTokens(ctx, credential, request, opts)
								case "stream":
									_, err = executor.ExecuteStream(ctx, credential, request, opts)
								default:
									if entry == "claude aggregate" {
										request.Model = "gemini-3-pro-preview"
									}
									_, err = executor.Execute(ctx, credential, request, opts)
								}
								if entry == "prepare" || entry == "HTTP" {
									if err != nil {
										t.Errorf("credential preparation failed: %v", err)
									}
								} else if testExecutorStatusCode(err) != http.StatusUnauthorized {
									t.Errorf("execution error=%v expected the synthetic upstream 401", err)
								}
								wantUpstream := 1
								if entry == "prepare" {
									wantUpstream = 0
								}
								synctest.Wait()
								if refreshCalls != wantRefresh || upstreamCalls != wantUpstream ||
									creditsCalls != wantRefresh {
									t.Errorf(
										"refresh=%d expected=%d upstream=%d expected=%d credits=%d expected=%d",
										refreshCalls, wantRefresh, upstreamCalls, wantUpstream, creditsCalls,
										wantRefresh,
									)
								}
								if credential.Metadata["access_token"] != "synthetic-current-token" {
									t.Fatal("request refresh changed the caller credential")
								}
							},
						)
					},
				)
			}
		}
	}
}
