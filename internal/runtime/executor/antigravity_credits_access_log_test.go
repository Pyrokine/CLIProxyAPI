package executor

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/logging"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/util"
	cliproxyauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/usage"
	sdktranslator "github.com/Pyrokine/CLIProxyAPI/v6/sdk/translator"
	"github.com/gin-gonic/gin"
	log "github.com/sirupsen/logrus"
	"github.com/tidwall/gjson"
)

func TestAntigravityCreditsAccessLog(t *testing.T) {
	logger := log.StandardLogger()
	previousOutput, previousFormatter, previousLevel := logger.Out, logger.Formatter, logger.GetLevel()
	t.Cleanup(
		func() {
			logger.SetOutput(previousOutput)
			logger.SetFormatter(previousFormatter)
			logger.SetLevel(previousLevel)
		},
	)
	logger.SetFormatter(&log.JSONFormatter{})
	logger.SetLevel(log.InfoLevel)

	const responseBody = `{"response":{"candidates":[{"content":{"role":"model","parts":[{"text":"synthetic-` +
		`answer"}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":3,"candidatesTokenCount":2,"` +
		`totalTokenCount":5}}}`
	const failureBody = `{"error":{"code":400,"message":"synthetic rejection"}}`
	for _, mode := range []struct {
		name, model string
		stream      bool
	}{
		{name: "Execute", model: "gemini-2.5-flash"},
		{name: "ExecuteClaudeNonStream", model: "claude-sonnet-4-6"},
		{name: "ExecuteStream", model: "gemini-2.5-flash", stream: true},
	} {
		for _, legacy := range []bool{false, true} {
			for _, requestLog := range []bool{false, true} {
				t.Run(
					fmt.Sprintf(
						"%s/legacy=%t/request-log=%t",
						mode.name,
						legacy,
						requestLog,
					),
					func(t *testing.T) {
						credential := antigravityCountTokensAuth("https://synthetic.invalid", t.Name())
						cliproxyauth.SetAntigravityCreditsHint(
							credential.ID, cliproxyauth.AntigravityCreditsHint{Known: true},
						)
						var output bytes.Buffer
						logger.SetOutput(&output)
						engine := gin.New()
						engine.Use(logging.GinLogrusLogger())
						requestIDs := make(map[string]bool)
						var baselineKeys map[any]bool
						var handle gin.HandlerFunc
						engine.NoRoute(func(c *gin.Context) { handle(c) })
						for _, scenario := range []struct {
							name               string
							requested, enabled bool
							executions         int
							failure            bool
						}{
							{name: "credits", requested: true, enabled: true, executions: 1},
							{name: "ordinary-after-credits", enabled: true, executions: 1},
							{name: "disabled", requested: true, executions: 1},
							{name: "multiple-credit-executions", requested: true, enabled: true, executions: 2},
							{name: "upstream-rejection", requested: true, enabled: true, executions: 1, failure: true},
							{name: "ordinary-after-rejection", enabled: true, executions: 1},
						} {
							t.Run(
								scenario.name, func(t *testing.T) {
									output.Reset()
									manager := usage.NewManager(8)
									defer manager.Stop()
									plugin := &captureUsagePlugin{records: make(chan usage.Record, 8)}
									manager.Register(plugin)
									cfg := &config.Config{
										RequestLog: requestLog,
										QuotaExceeded: config.QuotaExceeded{
											AntigravityCredits: scenario.enabled,
										},
									}
									exec := NewAntigravityExecutor(cfg)
									wantCredits := scenario.requested && scenario.enabled
									calls := 0
									var requestID string
									handle = func(c *gin.Context) {
										if _, exists := c.Get("__antigravity_credits_used__"); exists {
											t.Fatal("credits marker leaked from a previous request")
										}
										requestID = logging.GetGinRequestID(c)
										ctx := util.WithGinContext(c.Request.Context(), c)
										if legacy {
											//lint:ignore SA1029 验证已发布的 gin 字符串键兼容路径
											ctx = context.WithValue(c.Request.Context(), "gin", c)
										}
										ctx = usage.WithManager(ctx, manager)
										if scenario.requested {
											ctx = cliproxyauth.WithAntigravityCredits(ctx)
										}
										ctx = util.WithRoundTripper(
											ctx, antigravityCountTokensRoundTripFunc(
												func(req *http.Request) (*http.Response, error) {
													calls++
													wantPath := antigravityGeneratePath
													if mode.stream || mode.name == "ExecuteClaudeNonStream" {
														wantPath = antigravityStreamPath
													}
													if req.URL.Host != "synthetic.invalid" ||
														req.URL.Path != wantPath ||
														req.Method != http.MethodPost ||
														req.Header.Get(
															"Authorization",
														) != "Bearer test-token" ||
														req.Header.Get("Content-Type") != "application/json" {
														t.Fatalf(
															"upstream request changed: %s %s headers=%v",
															req.Method,
															req.URL,
															req.Header,
														)
													}
													body, err := io.ReadAll(req.Body)
													if err != nil {
														t.Fatal(err)
													}
													_ = req.Body.Close()
													credits := gjson.GetBytes(body, "enabledCreditTypes")
													if credits.Exists() != wantCredits ||
														(wantCredits && credits.Raw != `["GOOGLE_ONE_AI"]`) ||
														gjson.GetBytes(
															body, "request.contents.0.parts.0.text",
														).String() != "synthetic-private-body" {
														t.Fatalf(
															"credits=%t upstream body=%s",
															wantCredits,
															body,
														)
													}
													status, contentType, bodyText := http.StatusOK, "application/"+
														"json", responseBody
													if scenario.failure {
														status, bodyText = http.StatusBadRequest, failureBody
													} else if wantPath == antigravityStreamPath {
														contentType, bodyText = "text/event-stream", "data: "+
															responseBody+
															"\n\n"
													}
													return &http.Response{
														StatusCode: status, Request: req, Header: http.Header{
															"Content-Type": {
																contentType,
															}, "X-Synthetic": {"keep"},
														}, Body: io.NopCloser(strings.NewReader(bodyText)),
													}, nil
												},
											),
										)
										for range scenario.executions {
											payload := []byte(
												`{"request":{"contents":[{"role":"user","parts":[{"text":` +
													`"synthetic-private-body"}]}]}}`,
											)
											original := bytes.Clone(payload)
											req := cliproxyexecutor.Request{Model: mode.model, Payload: payload}
											opts := cliproxyexecutor.Options{
												SourceFormat: sdktranslator.FormatAntigravity, Stream: mode.stream,
											}
											var body []byte
											var headers http.Header
											var err error
											if mode.stream {
												var result *cliproxyexecutor.StreamResult
												result, err = exec.ExecuteStream(ctx, credential, req, opts)
												if err == nil {
													headers = result.Headers
													for chunk := range result.Chunks {
														if chunk.Err != nil {
															t.Fatalf("stream error: %v", chunk.Err)
														}
														body = append(body, chunk.Payload...)
													}
												}
											} else {
												var result cliproxyexecutor.Response
												result, err = exec.Execute(ctx, credential, req, opts)
												body, headers = result.Payload, result.Headers
											}
											if !bytes.Equal(payload, original) {
												t.Fatal("caller payload mutated")
											}
											if scenario.failure {
												var status interface{ StatusCode() int }
												if !errors.As(
													err, &status,
												) ||
													status.StatusCode() !=
														http.StatusBadRequest ||
													err.Error() !=
														failureBody {
													t.Fatalf("upstream rejection changed: %v", err)
												}
												_ = c.Error(errors.New("synthetic\r\nprivate error"))
												c.Data(
													http.StatusBadRequest,
													"application/json",
													[]byte(err.Error()),
												)
											} else {
												if err !=
													nil ||
													headers.Get("X-Synthetic") !=
														"keep" ||
													!bytes.Contains(
														body, []byte(`"text":"synthetic-answer"`),
													) || !bytes.Contains(body, []byte(`"totalTokenCount":5`)) {
													t.Fatalf(
														"response changed: err=%v headers=%v body=%s", err, headers,
														body,
													)
												}
												c.Data(http.StatusOK, "application/json", body)
											}
										}
										marker, exists := c.Get("__antigravity_credits_used__")
										if exists != wantCredits || (exists && marker != true) {
											t.Fatalf("marker=%v exists=%t want=%t", marker, exists, wantCredits)
										}
										keys := make(map[any]bool)
										for key := range c.Keys {
											if key != "__antigravity_credits_used__" {
												keys[key] = true
											}
										}
										if baselineKeys == nil {
											baselineKeys = keys
										} else if !scenario.failure && !maps.Equal(keys, baselineKeys) {
											t.Fatalf("request fields expanded: %v, baseline %v", keys, baselineKeys)
										}
									}
									recorder := httptest.NewRecorder()
									//goland:noinspection SpellCheckingInspection
									engine.ServeHTTP(
										recorder, httptest.NewRequest(
											http.MethodPost,
											"/v1/credits%0Arequest?api_key=synthetic-query-secret&model=demo",
											nil,
										),
									)
									manager.Stop()
									wantStatus := http.StatusOK
									if scenario.failure {
										wantStatus = http.StatusBadRequest
										if recorder.Body.String() != failureBody {
											t.Fatalf("HTTP rejection body=%s", recorder.Body.String())
										}
									}
									if calls != scenario.executions || recorder.Code != wantStatus ||
										len(requestID) != 32 ||
										requestIDs[requestID] {
										t.Fatalf(
											"calls=%d status=%d request_id=%q",
											calls,
											recorder.Code,
											requestID,
										)
									}
									requestIDs[requestID] = true
									if len(plugin.records) != scenario.executions {
										t.Fatalf(
											"usage record count=%d want=%d",
											len(plugin.records),
											scenario.executions,
										)
									}
									for range scenario.executions {
										record := <-plugin.records
										if record.Failed != scenario.failure ||
											record.AuthID != credential.ID ||
											record.Provider != "antigravity" ||
											record.Model != mode.model ||
											record.RequestID != requestID {
											t.Fatalf("usage attribution changed: %+v", record)
										}
										if !scenario.failure &&
											(record.Detail.InputTokens !=
												3 ||
												record.Detail.OutputTokens !=
													2 ||
												record.Detail.TotalTokens !=
													5) {
											t.Fatalf("usage tokens changed: %+v", record.Detail)
										}
									}
									var entry map[string]any
									if err := json.Unmarshal(bytes.TrimSpace(output.Bytes()), &entry); err != nil {
										t.Fatalf("expected one access log: %v; output=%s", err, output.String())
									}
									message, _ := entry["msg"].(string)
									wantMarkers := 0
									if wantCredits {
										wantMarkers = 1
									}
									if len(entry) != 4 || entry["request_id"] != requestID || strings.Count(
										message, "[credits]",
									) != wantMarkers || (wantCredits && !strings.HasSuffix(message, " [credits]")) {
										t.Fatalf("access log fields or marker changed: %v", entry)
									}
									if strings.ContainsAny(message, "\r\n") || !strings.Contains(
										message, `credits\nrequest`,
									) || strings.Contains(
										message, "synthetic-query-secret",
									) || strings.Contains(
										message, "synthetic-private-body",
									) || strings.Contains(message, "test-token") || !strings.Contains(
										message, "model=demo",
									) || !strings.Contains(message, fmt.Sprintf("%d |", wantStatus)) {
										t.Fatalf("access log lost escaping, masking or status: %q", message)
									}
								},
							)
						}
					},
				)
			}
		}
	}
}
