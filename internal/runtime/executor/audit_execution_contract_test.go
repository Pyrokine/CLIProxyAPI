package executor

import (
	"context"
	"errors"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/util"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/usage"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/translator"
	"github.com/tidwall/gjson"
)

func TestAuditRetryAfterDoesNotOverflow(t *testing.T) {
	for _, value := range []string{"9223372037", "9223372036854775807"} {
		err := newUpstreamStatusErr(
			http.StatusTooManyRequests, []byte(`{"error":"rate limit"}`), http.Header{"Retry-After": {value}},
		)
		delay := err.RetryAfter()
		if delay == nil || *delay != time.Duration(int64(math.MaxInt64)/int64(time.Second))*time.Second {
			t.Fatalf("Retry-After %s = %v", value, delay)
		}
	}
}

type auditHomeRefreshClient struct {
	auth.HomeClient
	*testHomeRefreshClient
}

func (c *auditHomeRefreshClient) HeartbeatOK() bool { return c.testHomeRefreshClient.HeartbeatOK() }
func (c *auditHomeRefreshClient) GetRefreshAuth(ctx context.Context, index, hash string) ([]byte, error) {
	return c.testHomeRefreshClient.GetRefreshAuth(ctx, index, hash)
}

func TestAuditHomeRefreshRejectsEmptySuccessWithoutReplacingCredential(t *testing.T) {
	for _, payload := range []string{`null`, `{}`, `{"auth":null}`, `{"auth":{}}`} {
		t.Run(
			payload, func(t *testing.T) {
				cfg := &config.Config{Home: config.HomeConfig{Enabled: true}}
				manager := auth.NewManager(nil, nil, nil)
				homeClient := &auditHomeRefreshClient{
					testHomeRefreshClient: &testHomeRefreshClient{
						heartbeat: true, payload: []byte(payload),
					},
				}
				manager.SetHomeClientProvider(func() auth.HomeClient { return homeClient })
				manager.SetConfig(cfg)
				manager.RegisterExecutor(NewGeminiExecutor(cfg))
				original := &auth.Auth{
					ID: t.Name(), Provider: "gemini", Index: "external-index",
					Metadata: map[string]any{"access_token": "synthetic-old", "refresh_token": "synthetic-refresh"},
				}
				ctx := auth.WithHomeRefreshClient(
					t.Context(), &testHomeRefreshClient{heartbeat: true, payload: []byte(payload)},
				)
				if _, err := manager.Register(ctx, original); err != nil {
					t.Fatal(err)
				}
				if _, err := manager.ForceRefreshAuth(ctx, original.ID); err == nil {
					t.Fatal("empty Home response accepted")
				}
				if homeClient.index != "external-index" {
					t.Fatalf("Home refresh was not called for the requested credential: %q", homeClient.index)
				}
				current, ok := manager.GetByID(original.ID)
				if !ok || current.Provider != original.Provider ||
					current.Metadata["access_token"] != "synthetic-old" ||
					!current.LastRefreshedAt.IsZero() {
					t.Fatalf("credential replaced after failed refresh: %+v", current)
				}
			},
		)
	}
}

func TestAuditProtectedClientEnvironmentProxy(t *testing.T) {
	if os.Getenv("CPA_AUDIT_ENV_PROXY_CHILD") == "1" {
		calls := 0
		server := httptest.NewServer(
			http.HandlerFunc(
				func(w http.ResponseWriter, req *http.Request) {
					calls++
					if req.URL.Host != "audit-upstream.invalid" {
						t.Errorf("target = %s", req.URL)
					}
					_, _ = io.WriteString(w, "synthetic proxy response")
				},
			),
		)
		defer server.Close()
		t.Setenv("HTTP_PROXY", server.URL)
		t.Setenv("HTTPS_PROXY", server.URL)
		t.Setenv("NO_PROXY", "")
		t.Setenv("no_proxy", "")
		client := newProtectedUtlsHTTPClient(t.Context(), nil, nil, time.Second)
		response, err := client.Get("http://audit-upstream.invalid/path")
		if err != nil {
			t.Fatal(err)
		}
		defer func(closeResource func() error) {
			if err := closeResource(); err != nil {
				t.Errorf("close proxy fixture response: %v", err)
			}
		}(response.Body.Close)
		body, err := io.ReadAll(response.Body)
		if err != nil || calls != 1 || string(body) != "synthetic proxy response" {
			t.Fatalf("calls=%d body=%s err=%v", calls, body, err)
		}
		return
	}
	command := exec.CommandContext(
		t.Context(), os.Args[0], "-test.run=^TestAuditProtectedClientEnvironmentProxy$", "-test.timeout=15s",
	)
	command.Env = append(os.Environ(), "CPA_AUDIT_ENV_PROXY_CHILD=1")
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("environment proxy: %v\n%s", err, output)
	}
}

func TestAuditAntigravityHttpRequestStripsCloseWithoutMutatingCaller(t *testing.T) {
	server := httptest.NewServer(
		http.HandlerFunc(
			func(w http.ResponseWriter, req *http.Request) {
				if req.Close || req.Header.Get("Connection") != "" || req.Header.Get("X-Untrusted") != "" {
					t.Errorf("unfiltered request: close=%t headers=%v", req.Close, req.Header)
				}
				w.WriteHeader(http.StatusNoContent)
			},
		),
	)
	defer server.Close()
	request, err := http.NewRequestWithContext(t.Context(), http.MethodPost, server.URL, strings.NewReader(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	request.Close = true
	request.Header.Set("X-Untrusted", "keep-on-caller")
	response, err := NewAntigravityExecutor(&config.Config{}).HttpRequest(
		t.Context(), antigravityCountTokensAuth(server.URL, t.Name()), request,
	)
	if err != nil {
		t.Fatal(err)
	}
	_ = response.Body.Close()
	if !request.Close || request.Header.Get("X-Untrusted") != "keep-on-caller" {
		t.Fatal("caller request mutated")
	}
}

func TestAuditAntigravityGenerationPayloadDropsStreamFlag(t *testing.T) {
	for _, stream := range []bool{false, true} {
		antigravityExecutor := NewAntigravityExecutor(&config.Config{})
		credential := antigravityCountTokensAuth("https://synthetic.invalid", t.Name())
		raw := []byte(`{"request":{"stream":true,"contents":[{"role":"user","parts":[{"text":"keep"}]}]}}`)
		req := executor.Request{Model: "gemini-2.5-flash", Payload: raw}
		payload, err := antigravityExecutor.prepareAntigravityRequestPayload(
			t.Context(), credential, req, executor.Options{SourceFormat: translator.FormatAntigravity}, raw, raw,
		)
		if err != nil {
			t.Fatal(err)
		}
		request, err := antigravityExecutor.buildRequest(
			t.Context(), credential, "synthetic", payload, stream, "", "https://synthetic.invalid",
		)
		if err != nil {
			t.Fatal(err)
		}
		body, err := io.ReadAll(request.Body)
		_ = request.Body.Close()
		if err != nil || gjson.GetBytes(body, "request.stream").Exists() || !strings.Contains(string(body), "keep") {
			t.Fatalf("stream=%t body=%s err=%v", stream, body, err)
		}
	}
}

func TestAuditUpstreamTokenCountValidation(t *testing.T) {
	for _, provider := range []string{"claude", "antigravity"} {
		for _, value := range []string{"missing", "null", `"7"`, "-1", "1.5", "0", "7"} {
			t.Run(
				provider+"/"+value, func(t *testing.T) {
					field := "input_tokens"
					if provider == "antigravity" {
						field = "totalTokens"
					}
					body := `{}`
					if value != "missing" {
						body = `{"` + field + `":` + value + `}`
					}
					calls := 0
					ctx := util.WithRoundTripper(
						t.Context(), antigravityCountTokensRoundTripFunc(
							func(req *http.Request) (*http.Response, error) {
								calls++
								return &http.Response{
									StatusCode: http.StatusOK, Header: make(http.Header), Request: req,
									Body: io.NopCloser(strings.NewReader(body)),
								}, nil
							},
						),
					)
					var err error
					if provider == "claude" {
						_, err = NewClaudeExecutor(&config.Config{}).CountTokens(
							ctx, &auth.Auth{
								ID: t.Name(), Provider: "claude", Attributes: map[string]string{"api_key": "synthetic"},
							}, executor.Request{
								Model:   "claude-sonnet-4-6",
								Payload: []byte(`{"messages":[{"role":"user","content":"hi"}]}`),
							}, executor.Options{SourceFormat: translator.FormatClaude},
						)
					} else {
						_, err = NewAntigravityExecutor(&config.Config{}).CountTokens(
							ctx, antigravityCountTokensAuth("https://synthetic.invalid", t.Name()), executor.Request{
								Model:   "gemini-2.5-flash",
								Payload: []byte(`{"request":{"contents":[{"role":"user","parts":[{"text":"hi"}]}]}}`),
							}, executor.Options{SourceFormat: translator.FormatAntigravity},
						)
					}
					valid := value == "0" || value == "7"
					if calls != 1 || (err == nil) != valid {
						t.Fatalf("calls=%d err=%v valid=%t", calls, err, valid)
					}
					if !valid {
						var status interface{ StatusCode() int }
						if !errors.As(err, &status) || status.StatusCode() != http.StatusBadGateway {
							t.Fatalf("invalid count status=%v", err)
						}
					}
				},
			)
		}
	}
}

func TestAuditZeroUsageRetainsCredentialFingerprint(t *testing.T) {
	for _, canceled := range []bool{false, true} {
		plugin := &captureUsagePlugin{records: make(chan usage.Record, 1)}
		registerTestUsagePlugin(t, plugin)
		ctx, cancel := context.WithCancel(t.Context())
		credential := &auth.Auth{
			ID: t.Name(), Provider: "gemini", Metadata: map[string]any{"access_token": "synthetic-fingerprint"},
		}
		reporter := newUsageReporter(ctx, "gemini", "synthetic-model", credential)
		if canceled {
			cancel()
		}
		reporter.ensurePublished(ctx)
		cancel()
		select {
		case record := <-plugin.records:
			if record.AccessTokenSHA256 != auth.AccessTokenSHA256(credential) || record.Failed != canceled ||
				record.Detail.TotalTokens != 0 {
				t.Fatalf("record=%+v", record)
			}
		case <-time.After(time.Second):
			t.Fatal("usage record not published")
		}
	}
}

func TestAuditResponseModelRecognizesCompactAndGeminiCLI(t *testing.T) {
	for _, test := range []struct{ provider, body, model string }{
		{
			"codex", `{"object":"response.compaction","model":"gpt-synthetic","usage":{"input_tokens":7}}`,
			"gpt-synthetic",
		},
		{
			"gemini-cli", `{"response":{"modelVersion":"gemini-synthetic","candidates":[{"finishReason":"STOP"}]}}`,
			"gemini-synthetic",
		},
	} {
		model, terminal := responseModelEvent([]byte(test.body), test.provider)
		if model != test.model || !terminal {
			t.Fatalf("provider=%s model=%s terminal=%t", test.provider, model, terminal)
		}
	}
}

func TestAuditClaudeUsagePreservesUnknownCacheCreationTokens(t *testing.T) {
	var buffer claudeStreamUsageBuffer
	buffer.observe(
		[]byte(
			`data: {"type":"message_start","message":{"usage":{"input_tokens":7,"cache_creation_input_tokens":15}}}`,
		),
	)
	buffer.observe(
		[]byte((`data: {"type":"message_delta","usage":{"output_tokens":5,"cache_creation":{"` +
			`ephemeral_5m_input_tokens":8,"ephemeral_1h_input_tokens":5}}}`)),
	)
	detail, ok := buffer.detailValue()
	if !ok || detail.CacheCreationTokens != 15 || detail.CacheCreation5mInputTokens != 8 ||
		detail.CacheCreation1hInputTokens != 5 {
		t.Fatalf("detail=%+v", detail)
	}
}

func TestAuditAntigravityExhaustionRejectsOldCredentialGeneration(t *testing.T) {
	for _, mutation := range []string{"replace", "recreate", "delete"} {
		t.Run(
			mutation, func(t *testing.T) {
				manager := auth.NewManager(nil, nil, nil)
				manager.SetHomeClientProvider(func() auth.HomeClient { return nil })
				old := antigravityCountTokensAuth("https://synthetic.invalid", t.Name())
				if _, err := manager.Register(t.Context(), old); err != nil {
					t.Fatal(err)
				}
				snapshot, _ := manager.GetByID(old.ID)
				calls := 0
				client := &http.Client{
					Transport: antigravityCountTokensRoundTripFunc(
						func(req *http.Request) (*http.Response, error) {
							calls++
							if mutation == "delete" || mutation == "recreate" {
								if err := manager.Remove(t.Context(), old.ID); err != nil {
									t.Fatal(err)
								}
							}
							if mutation != "delete" {
								replacement := old.Clone()
								replacement.Metadata["access_token"] = "synthetic-replacement"
								var err error
								if mutation == "replace" {
									_, err = manager.Update(t.Context(), replacement)
								} else {
									_, err = manager.Register(auth.WithAllowAuthRecreate(t.Context()), replacement)
								}
								if err != nil {
									t.Fatal(err)
								}
								current, _ := manager.GetByID(old.ID)
								if !current.PublishAntigravityCreditsHint(
									auth.AntigravityCreditsHint{
										Known: true, Available: true, CreditAmount: 1500,
									},
								) {
									t.Fatal("current balance rejected")
								}
							}
							return &http.Response{
								StatusCode: http.StatusTooManyRequests, Header: make(http.Header),
								Body: io.NopCloser(
									strings.NewReader(
										`{"error":{"details":[{"@type":"type.googleapis.com/google.rpc.ErrorInf` +
											`o","reason":"INSUFFICIENT_G1_CREDITS_BALANCE"}]}}`,
									),
								),
								Request: req,
							}, nil
						},
					),
				}
				ctx := auth.WithAntigravityCredits(t.Context())
				_, err := NewAntigravityExecutor(
					&config.Config{QuotaExceeded: config.QuotaExceeded{AntigravityCredits: true}},
				).sendWithRetry(
					ctx, client, []string{"https://synthetic.invalid"}, 1, "claude-synthetic", snapshot,
					func(base string) (*http.Request, error) {
						return http.NewRequestWithContext(ctx, http.MethodPost, base, nil)
					},
				)
				if err == nil || calls != 1 {
					t.Fatalf("calls=%d error=%v", calls, err)
				}
				hint, ok := auth.GetAntigravityCreditsHint(old.ID)
				if mutation == "delete" {
					if ok {
						t.Fatalf("deleted credential balance recreated: %+v", hint)
					}
				} else if !ok || !hint.Available || hint.CreditAmount != 1500 {
					t.Fatalf("new credential balance overwritten: %+v", hint)
				}
			},
		)
	}
}

func TestAuditAntigravityCreditDiscoveryUsesLoadShape(t *testing.T) {
	for _, test := range []struct {
		name, attributeBase, metadataBase, expectedBase, generationBase string
	}{
		{
			name: "default-prod-query", expectedBase: "https://cloudcode-pa.googleapis.com",
			generationBase: antigravityBaseURLDaily,
		},
		{
			name: "original-explicit-base", attributeBase: "https://synthetic.invalid",
			expectedBase: "https://synthetic.invalid", generationBase: "https://synthetic.invalid",
		},
		{
			name: "attribute-prefix", attributeBase: "https://synthetic.invalid/proxy/",
			expectedBase: "https://synthetic.invalid/proxy", generationBase: "https://synthetic.invalid/proxy",
		},
		{
			name: "metadata-prefix", metadataBase: "https://metadata.synthetic.invalid/proxy/",
			expectedBase:   "https://metadata.synthetic.invalid/proxy",
			generationBase: "https://metadata.synthetic.invalid/proxy",
		},
		{
			name: "attribute-priority", attributeBase: "https://synthetic.invalid",
			metadataBase: "https://metadata.synthetic.invalid", expectedBase: "https://synthetic.invalid",
			generationBase: "https://synthetic.invalid",
		},
	} {
		t.Run(
			test.name, func(t *testing.T) {
				credential := antigravityCountTokensAuth(test.attributeBase, t.Name())
				if test.metadataBase != "" {
					credential.Metadata["base_url"] = test.metadataBase
				}
				calls := 0
				ctx := util.WithRoundTripper(
					t.Context(), antigravityCountTokensRoundTripFunc(
						func(req *http.Request) (*http.Response, error) {
							calls++
							if req.URL.String() != test.expectedBase+antigravityLoadCodeAssistPath {
								t.Fatalf(
									"credits URL=%s, want %s", req.URL, test.expectedBase+antigravityLoadCodeAssistPath,
								)
							}
							raw, err := io.ReadAll(req.Body)
							if err != nil {
								t.Fatal(err)
							}
							if req.Header.Get("Accept") != "*/*" || req.Header.Get("X-Goog-Api-Client") != "" ||
								gjson.GetBytes(
									raw, "metadata.ideType",
								).String() != "ANTIGRAVITY" ||
								len(gjson.GetBytes(raw, "metadata").Map()) != 1 {
								t.Fatalf("headers=%v body=%s", req.Header, raw)
							}
							return &http.Response{
								StatusCode: http.StatusOK, Header: make(http.Header),
								Body: io.NopCloser(
									strings.NewReader(`{"paidTier":{"id":"synthetic","availableCredits":[]}}`),
								),
								Request: req,
							}, nil
						},
					),
				)
				NewAntigravityExecutor(
					&config.Config{QuotaExceeded: config.QuotaExceeded{AntigravityCredits: true}},
				).updateAntigravityCreditsBalance(
					ctx, credential, "synthetic",
				)
				if calls != 1 {
					t.Fatalf("calls=%d", calls)
				}
				bases := antigravityBaseURLFallbackOrder(credential)
				if len(bases) != 1 || bases[0] != test.generationBase ||
					resolveAntigravityRequestBaseURL(credential) != test.generationBase {
					t.Fatalf("generation endpoints=%v, want only %s", bases, test.generationBase)
				}
			},
		)
	}
}
