package executor

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/wsrelay"
	"github.com/gorilla/websocket"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/runtime/executor/helps"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/util"
	cliproxyauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/usage"
	sdktranslator "github.com/Pyrokine/CLIProxyAPI/v6/sdk/translator"
	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
)

func requestObservationCapture(t *testing.T) (context.Context, <-chan usage.Record) {
	t.Helper()
	manager := usage.NewManager(16)
	plugin := &captureUsagePlugin{records: make(chan usage.Record, 16)}
	manager.Register(plugin)
	t.Cleanup(manager.Stop)
	ctx := usage.WithManager(context.Background(), manager)
	ctx = usage.WithRequestedModelAlias(ctx, "client-alias")
	return ctx, plugin.records
}

func requestObservationRecord(t *testing.T, records <-chan usage.Record) usage.Record {
	t.Helper()
	select {
	case record := <-records:
		return record
	case <-time.After(3 * time.Second):
		t.Fatal("usage record was not published")
		return usage.Record{}
	}
}

func TestRequestObservationsCacheWriteAggregatePresence(t *testing.T) {
	for _, protocol := range []string{"openai", "claude", "interactions"} {
		for _, value := range []string{"", "0", "7", "null", `"0"`, "-1", "1.5", "1e3", "9223372036854775808"} {
			t.Run(
				protocol+"/"+value, func(t *testing.T) {
					field := ""
					if value != "" {
						name := "cache_write_tokens"
						if protocol == "claude" {
							name = "cache_creation_input_tokens"
						}
						field = `,"` + name + `":` + value
					}
					var detail usage.Detail
					switch protocol {
					case "openai":
						detail = parseOpenAIUsage([]byte(`{"model":"gpt-6.2-sol","usage":{"input_tokens":100,"output_tokens":1,"input_tokens_details":{"cached_tokens":0` + field + `}}}`))
					case "claude":
						detail = parseClaudeUsage([]byte(`{"usage":{"input_tokens":100,"output_tokens":1` + field + `}}`))
					case "interactions":
						detail = parseInteractionsUsage([]byte(`{"usage":{"input_tokens":100,"output_tokens":1` + field + `}}`))
					}
					want := value == "0" || value == "7"
					if detail.CacheWriteReported == nil || *detail.CacheWriteReported != want {
						t.Fatalf("write presence=%v for %s %s", detail.CacheWriteReported, protocol, value)
					}
					if value == "7" && detail.CacheCreationTokens != 7 {
						t.Fatalf("write total=%d", detail.CacheCreationTokens)
					}
					if protocol == "openai" && (!detail.CacheReadReported || detail.CacheReadTokens != 0) {
						t.Fatal("explicit zero cache read was lost")
					}
				},
			)
		}
	}
	for _, present := range []bool{false, true} {
		detail := devinUsageDetail(&helps.DevinUsage{PromptTokens: 10, CacheWriteTokensPresent: present})
		if detail.CacheWriteReported == nil || *detail.CacheWriteReported != present {
			t.Fatalf("native write presence=%+v", detail)
		}
	}
	gemini := parseGeminiUsage([]byte(`{"usageMetadata":{"promptTokenCount":10,"candidatesTokenCount":1}}`))
	if gemini.CacheWriteReported == nil || *gemini.CacheWriteReported {
		t.Fatal("Gemini cache read protocol acquired a write quantity")
	}
}

func TestRequestObservationsCacheWriteTTLCompleteness(t *testing.T) {
	for _, family := range []string{"claude", "qwen-chat", "qwen-responses", "openai"} {
		for _, second := range []string{"", "0", "4", "null", `"0"`, "-1"} {
			t.Run(
				family+"/"+second, func(t *testing.T) {
					firstKey, secondKey := "ttl_30m_tokens", "ttl_12h_tokens"
					if family == "claude" || family == "qwen-chat" {
						firstKey, secondKey = "ephemeral_5m_input_tokens", "ephemeral_1h_input_tokens"
					}
					if family == "qwen-responses" {
						firstKey, secondKey = "ttl_5m_tokens", "ttl_1h_tokens"
					}
					fields := `"` + firstKey + `":3`
					if second != "" {
						fields += `,"` + secondKey + `":` + second
					}
					var detail usage.Detail
					switch family {
					case "claude":
						detail = parseClaudeUsage([]byte(`{"usage":{"input_tokens":100,"cache_creation":{` + fields + `}}}`))
					case "qwen-chat":
						detail = parseOpenAIUsage([]byte(`{"usage":{"prompt_tokens":100,"prompt_tokens_details":{"cache_creation":{` + fields + `}}}}`))
					default:
						detail = parseOpenAIUsage([]byte(`{"usage":{"input_tokens":100,"input_tokens_details":{"cache_write_tokens_details":{` + fields + `}}}}`))
					}
					known := second == "0" || second == "4"
					wantTotal := int64(3)
					if second == "4" {
						wantTotal = 7
					}
					if detail.CacheWriteReported == nil || *detail.CacheWriteReported != known || detail.CacheCreationTokens != wantTotal {
						t.Fatalf("partial TTL became a known total: %+v", detail)
					}
					if family == "openai" && detail.CacheCreation30mInputTokens != 3 || family != "openai" && detail.CacheCreation5mInputTokens != 3 {
						t.Fatal("reported TTL quantity was lost")
					}
				},
			)
		}
	}
	zero := parseOpenAIUsage([]byte(`{"usage":{"input_tokens":100,"input_tokens_details":{"cache_write_tokens":0,"cache_write_tokens_details":{"ttl_30m_tokens":3}}}}`))
	if zero.CacheWriteReported == nil || !*zero.CacheWriteReported || zero.CacheCreationTokens != 0 || zero.CacheCreation30mInputTokens != 3 {
		t.Fatalf("explicit aggregate zero changed: %+v", zero)
	}
}

func TestRequestObservationsClaudeContextNeedsReportedCacheTotals(t *testing.T) {
	for _, test := range []struct {
		fields  string
		context *int64
	}{
		{``, nil},
		{`,"cache_read_input_tokens":0`, nil},
		{`,"cache_creation_input_tokens":0`, nil},
		{`,"cache_read_input_tokens":0,"cache_creation_input_tokens":0`, new(int64(10))},
		{`,"cache_read_input_tokens":0,"cache_creation":{"ephemeral_5m_input_tokens":3}`, nil},
		{
			`,"cache_read_input_tokens":0,"cache_creation":{"ephemeral_5m_input_tokens":3,"ephemeral_1h_input_tokens":0}`,
			new(int64(13)),
		},
	} {
		detail := parseClaudeUsage([]byte(`{"usage":{"input_tokens":10` + test.fields + `}}`))
		if detail.BillingMetadata == nil || !reflect.DeepEqual(
			detail.BillingMetadata.ContextInputTokens, test.context,
		) {
			t.Fatalf("context invented unknown cache quantities: %+v", detail.BillingMetadata)
		}
	}
	var buffer claudeStreamUsageBuffer
	buffer.observe([]byte(`data: {"type":"message_start","message":{"usage":{"input_tokens":0,"cache_read_input_tokens":0,"cache_creation":{"ephemeral_5m_input_tokens":3}}}}`))
	if buffer.detail.BillingMetadata.ContextInputTokens != nil {
		t.Fatal("partial TTL was treated as a complete context")
	}
	buffer.observe([]byte(`data: {"type":"message_delta","usage":{"cache_creation":{"ephemeral_1h_input_tokens":0}}}`))
	if buffer.detail.BillingMetadata.ContextInputTokens == nil || *buffer.detail.BillingMetadata.ContextInputTokens != 3 {
		t.Fatal("stream lost explicitly reported zero input")
	}
}

func TestRequestObservationsCacheWriteStreamPresence(t *testing.T) {
	var claude claudeStreamUsageBuffer
	claude.observe([]byte(`data: {"type":"message_start","message":{"usage":{"input_tokens":10,"cache_creation":{"ephemeral_5m_input_tokens":3}}}}`))
	if claude.detail.CacheWriteReported == nil || *claude.detail.CacheWriteReported {
		t.Fatal("partial Claude TTL total is known")
	}
	claude.observe([]byte(`data: {"type":"message_delta","usage":{"output_tokens":1,"cache_creation":{"ephemeral_1h_input_tokens":4}}}`))
	claude.observe([]byte(`data: {"type":"message_delta","usage":{"output_tokens":2}}`))
	if !*claude.detail.CacheWriteReported || claude.detail.CacheCreationTokens != 7 || claude.detail.TotalTokens != 19 {
		t.Fatalf("Claude TTL stream=%+v", claude.detail)
	}
	var openai streamUsageBuffer
	openai.observeOpenAIStream([]byte(`data: {"usage":{"input_tokens":100,"output_tokens":1,"input_tokens_details":{"cache_write_tokens_details":{"ttl_30m_tokens":3}}}}`))
	if openai.detail.CacheWriteReported == nil || *openai.detail.CacheWriteReported {
		t.Fatal("partial OpenAI TTL total is known")
	}
	openai.observeOpenAIStream([]byte(`data: {"usage":{"input_tokens":100,"output_tokens":2,"input_tokens_details":{"cache_write_tokens_details":{"ttl_12h_tokens":4}}}}`))
	openai.observeOpenAIStream([]byte(`data: {"usage":{"input_tokens":100,"output_tokens":3}}`))
	if !*openai.detail.CacheWriteReported || openai.detail.CacheCreationTokens != 7 || openai.detail.TokenBreakdown.Input.CacheWriteTokens != 7 {
		t.Fatalf("OpenAI TTL stream=%+v", openai.detail)
	}
	openai.observeOpenAIStream([]byte(`data: {"usage":{"input_tokens":100,"output_tokens":3,"input_tokens_details":{"cache_write_tokens":0}}}`))
	if !*openai.detail.CacheWriteReported || openai.detail.CacheCreationTokens != 0 {
		t.Fatal("explicit zero did not replace the previous total")
	}
	var aggregate streamUsageBuffer
	aggregate.observeOpenAIStream([]byte(`data: {"usage":{"input_tokens":100,"input_tokens_details":{"cache_write_tokens":7}}}`))
	aggregate.observeOpenAIStream([]byte(`data: {"usage":{"input_tokens":100,"output_tokens":1}}`))
	if !*aggregate.detail.CacheWriteReported || aggregate.detail.CacheCreationTokens != 7 {
		t.Fatal("missing field cleared a known aggregate")
	}
	aggregate.observeOpenAIStream([]byte(`data: {"usage":{"input_tokens":100,"input_tokens_details":{"cache_write_tokens":null}}}`))
	if *aggregate.detail.CacheWriteReported {
		t.Fatal("invalid aggregate retained a known total")
	}
}

func TestRequestObservationsCacheWriteDeliveryAndHeaders(t *testing.T) {
	for _, present := range []int{0, 1, 2} {
		ctx, records := requestObservationCapture(t)
		reporter := newUsageReporter(ctx, "kimi", "routed-model", nil)
		ctx = reporter.withContext(ctx)
		recordUpstreamRequest(
			ctx, nil, "https://synthetic.example.test/chat/completions", nil, []byte(`{}`), "kimi", nil,
		)
		headers := make(http.Header)
		if present > 0 {
			headers.Set("Msh-Usage-Cache-Write-Tokens-5m", "3")
		}
		if present > 1 {
			headers.Set("Msh-Usage-Cache-Write-Tokens-1h", "0")
		}
		recordAPIResponseMetadata(ctx, nil, 200, headers)
		reporter.publishAdditionalModel(ctx, "image-child", usage.Detail{})
		child := requestObservationRecord(t, records)
		if child.CacheWriteReported != nil || child.Detail.CacheCreationTokens != 0 {
			t.Fatal("child acquired main request writes")
		}
		reporter.publish(ctx, usage.Detail{InputTokens: 100, CacheWriteReported: new(false)})
		got := requestObservationRecord(t, records)
		if got.CacheWriteReported == nil || *got.CacheWriteReported != (present == 2) {
			t.Fatalf("Kimi header total=%+v", got.Detail)
		}
	}
	ctx, records := requestObservationCapture(t)
	reported := true
	if err := usage.PublishRecord(ctx, usage.Record{Detail: usage.Detail{CacheWriteReported: &reported}}); err != nil {
		t.Fatal(err)
	}
	reported = false
	got := requestObservationRecord(t, records)
	if got.CacheWriteReported == nil || !*got.CacheWriteReported || got.Detail.CacheWriteReported == nil || !*got.Detail.CacheWriteReported {
		t.Fatal("queued report state shared the caller's pointer")
	}
	*got.Detail.CacheWriteReported = false
	if !*got.CacheWriteReported {
		t.Fatal("record and detail share a mutable report flag")
	}
	for _, flag := range []*bool{nil, new(false), new(true)} {
		detail := usage.EnsureTokenBreakdownForProvider(
			usage.Detail{
				InputTokens: 100, CacheCreationTokens: 7, CacheWriteReported: flag,
			}, "openai", "",
		)
		want := flag == nil || *flag
		if detail.CacheWriteReported == nil || *detail.CacheWriteReported != want {
			t.Fatal("legacy normalization changed explicit unknown")
		}
	}
}

func TestRequestObservationsCaptureWithoutRequestLog(t *testing.T) {
	for _, cfg := range []*config.Config{nil, {}, {CommercialMode: true}} {
		ctx, records := requestObservationCapture(t)
		reporter := newUsageReporter(ctx, "codex", "routed-model", nil)
		ctx = reporter.withContext(ctx)
		recordUpstreamRequest(
			ctx, cfg, "https://name:secret@upstream.example.test/v1/responses?api_key=secret&region=test", nil,
			[]byte(`{"model":"forwarded-model","service_tier":"priority"}`), "codex", nil,
		)
		finalRequest, err := http.NewRequest(
			http.MethodPost, "https://user:secret@final.example.test/result?api_key=secret&region=test#secret", nil,
		)
		if err != nil {
			t.Fatal(err)
		}
		recordAPIResponseMetadata(
			ctx, cfg, http.StatusOK, http.Header{"X-Upstream-Provider": {"header-channel"}},
			&http.Response{Request: finalRequest},
		)
		appendAPIResponseChunk(
			ctx, cfg,
			[]byte("data: {\"type\":\"response.completed\",\"response\":{\"model\":\"returned-model\",\"provider\":\"reported-channel\",\"service_tier\":\"default\"}}\n\n"),
		)
		reporter.publish(ctx, usage.Detail{TotalTokens: 1})
		got := requestObservationRecord(t, records)
		if got.Model != "routed-model" || got.ClientModel != "client-alias" || got.UpstreamModel != "forwarded-model" || got.ResponseModel != "returned-model" ||
			got.RequestServiceTier != "priority" || got.ResponseServiceTier != "default" || got.ResponseProvider != "reported-channel" || got.ResponseProviderSource != "body.response.provider" {
			t.Fatalf("request/response observations = %+v", got)
		}
		if got.ResponseURL != util.SanitizeUpstreamURL(finalRequest.URL.String()) || got.ResponseURLStatus != "observed" || got.ResponseURLSource != "http_response_request_url" || got.UpstreamNextHopStatus != "not_reported" {
			t.Fatalf("final response address = %+v", got)
		}
		if strings.Contains(got.UpstreamURL, "secret") || strings.Contains(
			got.UpstreamURL, "name",
		) || !strings.Contains(got.UpstreamURL, "region=test") {
			t.Fatalf("unsafe or incomplete outbound URL: %q", got.UpstreamURL)
		}
	}
}

func TestRequestObservationsResetOnRetryAndIsolateRoutes(t *testing.T) {
	ctx, records := requestObservationCapture(t)
	ginCtx := &gin.Context{}
	ctx = util.WithGinContext(ctx, ginCtx)
	first := newUsageReporter(ctx, "openai", "route-one", nil)
	firstCtx := first.withContext(ctx)
	second := newUsageReporter(ctx, "openai", "route-two", nil)
	secondCtx := second.withContext(ctx)
	recordUpstreamRequest(
		firstCtx, nil, "https://first.example.test/v1/responses", nil,
		[]byte(`{"model":"one","service_tier":"priority"}`), "openai", nil,
	)
	recordUpstreamRequest(
		secondCtx, nil, "https://second.example.test/v1/responses", nil, []byte(`{"model":"two"}`), "openai", nil,
	)
	appendAPIResponseChunk(
		firstCtx, nil,
		[]byte(`{"object":"chat.completion","model":"one-returned","provider":"one-channel","service_tier":"priority"}`),
	)
	first.publish(firstCtx, usage.Detail{})
	one := requestObservationRecord(t, records)
	if one.UpstreamModel != "one" || one.ResponseModel != "one-returned" {
		t.Fatalf("second route replaced first route: %+v", one)
	}
	staleRequest, _ := http.NewRequest(http.MethodPost, "https://stale.example.test/result", nil)
	recordAPIResponseMetadata(secondCtx, nil, http.StatusBadGateway, nil, &http.Response{Request: staleRequest})
	appendAPIResponseChunk(
		secondCtx, nil,
		[]byte(`{"model":"two-stale","provider":"stale","service_tier":"priority","usage":{"speed":"fast"}}`),
	)
	recordUpstreamRequest(
		secondCtx, nil, "https://retry.example.test/v1/responses", nil, []byte(`{"model":"retry-model"}`), "openai",
		nil,
	)
	second.publishFailure(secondCtx, errors.New("synthetic transport failure"))
	two := requestObservationRecord(t, records)
	if two.UpstreamURL != "https://retry.example.test/v1/responses" || two.UpstreamModel != "retry-model" || two.RequestServiceTier != "default" ||
		two.ResponseURL != "" || two.ResponseURLSource != "" || two.ResponseURLStatus != "no_response" || two.UpstreamNextHopStatus != "not_reported" || two.ResponseModel != "" || two.ResponseProvider != "" || two.ResponseProviderSource != "" || two.ResponseServiceTier != "" || two.ResponseSpeed != "" || !two.Failed {
		t.Fatalf("retry retained old response or lost failure destination: %+v", two)
	}
}

func TestRequestObservationsSplitSSEAndIndependentSpeed(t *testing.T) {
	ctx, records := requestObservationCapture(t)
	reporter := newUsageReporter(ctx, "claude", "routed-model", nil)
	ctx = reporter.withContext(ctx)
	recordUpstreamRequest(
		ctx, nil, "https://upstream.example.test/v1/messages", nil,
		[]byte(`{"model":"forwarded-model","speed":"fast"}`), "claude", nil,
	)
	payload := "data: {\"type\":\"message_start\",\"message\":{\"model\":\"returned-model\",\"usage\":{\"service_tier\":\"standard\",\"speed\":\"fast\"}}}\n\n" +
		"data: {\"type\":\"message_stop\"}\n\n" +
		"data: {\"type\":\"message_delta\",\"usage\":{\"speed\":\"standard\"}}\n\n"
	for offset := 0; offset < len(payload); offset += 7 {
		end := min(offset+7, len(payload))
		appendAPIResponseChunk(ctx, nil, []byte(payload[offset:end]))
	}
	reporter.publish(ctx, usage.Detail{})
	got := requestObservationRecord(t, records)
	if got.RequestSpeed != "fast" || got.ResponseSpeed != "standard" || got.ResponseServiceTier != "standard" || got.ResponseModel != "returned-model" {
		t.Fatalf("split stream observations = %+v", got)
	}
}

func TestRequestObservationsBoundPartialFramesAndReadHeaderCase(t *testing.T) {
	ctx, records := requestObservationCapture(t)
	reporter := newUsageReporter(ctx, "openai", "routed-model", nil)
	ctx = reporter.withContext(ctx)
	recordUpstreamRequest(
		ctx, nil, "https://upstream.example.test/v1/responses", nil, []byte(`{"model":"forwarded-model"}`), "openai",
		nil,
	)
	recordAPIResponseMetadata(ctx, nil, 200, http.Header{"x-upstream-provider": {"reported-channel"}})
	appendAPIResponseChunk(ctx, nil, []byte("data: "+strings.Repeat("x", maxStreamModelEventBytes+1)))
	if len(reporter.observationStream.pending) != 0 || !reporter.observationStream.pendingOverflow {
		t.Fatal("partial observation frame exceeded the model-event bound")
	}
	appendAPIResponseChunk(
		ctx, nil, []byte("\n\ndata: {\"model\":\"returned-model\",\"service_tier\":\"priority\"}\n\n"),
	)
	reporter.publish(ctx, usage.Detail{})
	got := requestObservationRecord(t, records)
	if got.ResponseProvider != "reported-channel" || got.ResponseProviderSource != "header.X-Upstream-Provider" || got.ResponseModel != "returned-model" || got.ResponseServiceTier != "priority" {
		t.Fatalf("observation did not recover after bounded frame: %+v", got)
	}
}

func TestRequestObservationsAdditionalModelKeepsUnknownSources(t *testing.T) {
	ctx, records := requestObservationCapture(t)
	reporter := newUsageReporter(ctx, "codex", "main-model", nil)
	ctx = reporter.withContext(ctx)
	recordUpstreamRequest(
		ctx, nil, "https://upstream.example.test/v1/responses", nil,
		[]byte(`{"model":"main-forwarded","service_tier":"priority","speed":"fast"}`), "codex", nil,
	)
	appendAPIResponseChunk(
		ctx, nil,
		[]byte(`{"type":"response.completed","response":{"model":"main-returned","provider":"main-provider","service_tier":"priority","usage":{"speed":"fast"}}}`),
	)
	finalRequest, _ := http.NewRequest(http.MethodPost, "https://final.example.test/responses", nil)
	recordAPIResponseMetadata(ctx, nil, http.StatusOK, nil, &http.Response{Request: finalRequest})
	reporter.publishAdditionalModel(ctx, "gpt-image-2", usage.Detail{ResponseServiceTier: "image-tier"})
	image := requestObservationRecord(t, records)
	if image.Model != "gpt-image-2" || image.Alias != image.Model || image.ResponseServiceTier != "image-tier" ||
		image.ResponseURL != "" || image.ResponseURLSource != "" || image.ResponseURLStatus != "" || image.UpstreamNextHopStatus != "" || image.ClientModel != "" || image.UpstreamURL != "" || image.UpstreamModel != "" || image.ResponseModel != "" ||
		image.RequestSpeed != "" || image.ResponseSpeed != "" || image.ResponseProvider != "" || image.ResponseProviderSource != "" {
		t.Fatalf("child usage acquired main execution observations: %+v", image)
	}
	reporter.publish(ctx, usage.Detail{})
	main := requestObservationRecord(t, records)
	if main.ResponseURL != "https://final.example.test/responses" || main.ResponseURLStatus != "observed" || main.UpstreamModel != "main-forwarded" || main.ResponseModel != "main-returned" || main.ResponseProvider != "main-provider" || main.ResponseServiceTier != "priority" {
		t.Fatalf("child usage consumed main observations: %+v", main)
	}
}

func TestRequestObservationsDevinStreamKeepsNativeProvenance(t *testing.T) {
	for _, outcome := range []string{
		"completed", "unavailable", "missing-eos", "http-error", "transport-error", "missing-model",
	} {
		t.Run(
			outcome, func(t *testing.T) {
				ctx, records := requestObservationCapture(t)
				ctx = util.WithGinContext(ctx, &gin.Context{})
				ctx = util.WithRoundTripper(
					ctx, devinTestRoundTripper(
						func(req *http.Request) (*http.Response, error) {
							if reporter := usageReporterFromContext(req.Context()); reporter == nil || !reporter.stream {
								t.Error("outbound context has no streaming reporter")
							}
							return requestObservationDevinResponse(req, outcome)
						},
					),
				)
				cfg := &config.Config{}
				cfg.RequestLog = true
				stream, err := NewDevinExecutor(cfg).ExecuteStream(
					ctx,
					&cliproxyauth.Auth{
						Attributes: map[string]string{
							"api_key": "synthetic-session", "base_url": "https://devin.synthetic.invalid",
						},
					},
					cliproxyexecutor.Request{
						Model:   "devin/swe-2",
						Payload: []byte(`{"input":[{"type":"user_input","content":"synthetic"}]}`),
					},
					cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatInteractions},
				)
				if err == nil {
					for chunk := range stream.Chunks {
						if chunk.Err != nil {
							err = chunk.Err
						}
					}
				}
				failed := outcome != "completed" && outcome != "missing-model"
				got := requestObservationRecord(t, records)
				if (err != nil) != failed || !got.Stream || got.Failed != failed || got.Completed == failed {
					t.Fatalf("outcome=%s error=%v record=%+v", outcome, err, got)
				}
				if got.Model != "devin/swe-2" || got.ClientModel != "client-alias" || got.UpstreamModel != "swe-2" ||
					got.Provider != "devin" || got.UpstreamURL != "https://devin.synthetic.invalid"+helps.DevinChatPath {
					t.Fatalf("request provenance=%+v", got)
				}
				expectedModel, expectedProvider := "devin/swe-3", "native-channel"
				if outcome == "missing-model" || outcome == "http-error" || outcome == "transport-error" {
					expectedModel = ""
				}
				if outcome == "transport-error" {
					expectedProvider = ""
				}
				if got.ResponseModel != expectedModel || got.ResponseProvider != expectedProvider {
					t.Fatalf("native response provenance=%+v", got)
				}
			},
		)
	}
}

func requestObservationDevinResponse(req *http.Request, outcome string) (*http.Response, error) {
	if outcome == "transport-error" {
		return nil, errors.New("synthetic transport failure")
	}
	metrics := testDevinVarintField(nil, 2, 7)
	metrics = testDevinVarintField(metrics, 3, 3)
	metrics = testDevinVarintField(metrics, 5, 2)
	if outcome != "missing-model" {
		metrics = testDevinBytesField(metrics, 9, []byte("devin/swe-3"))
	}
	frame := testDevinBytesField(nil, 3, []byte("synthetic answer"))
	frame = testDevinBytesField(frame, 7, metrics)
	wire := helps.WrapConnectEnvelope(frame)
	if outcome != "missing-eos" {
		trailer := `{}`
		if outcome == "unavailable" {
			trailer = `{"error":{"code":"unavailable","message":"synthetic failure"}}`
		}
		wire = append(wire, helps.WrapConnectEnvelopeWithFlag(helps.ConnectFlagEndStream, []byte(trailer))...)
	}
	status := http.StatusOK
	if outcome == "http-error" {
		status = http.StatusServiceUnavailable
		wire = []byte(`{"error":{"message":"synthetic HTTP failure"}}`)
	}
	return &http.Response{
		StatusCode: status,
		Header: http.Header{
			"Content-Type": {"application/connect+proto"}, "X-Upstream-Provider": {"native-channel"},
		},
		Body: io.NopCloser(bytes.NewReader(wire)), Request: req,
	}, nil
}

func TestClaudeTokenCountSkipsModelSpeedConfig(t *testing.T) {
	cfg := &config.Config{
		SDKConfig: config.SDKConfig{
			ModelCatalog: config.ModelCatalogConfig{
				Models: []config.ModelCatalogModel{
					{
						Name: "claude-opus-5-5", Channel: "claude", Speed: "fast",
					},
				},
			},
		},
	}
	var sent []byte
	ctx := util.WithRoundTripper(
		context.Background(), protectedUtlsRoundTripFunc(
			func(req *http.Request) (*http.Response, error) {
				var err error
				sent, err = io.ReadAll(req.Body)
				if err != nil {
					return nil, err
				}
				if strings.Contains(req.Header.Get("Anthropic-Beta"), claudeFastModeBeta) {
					t.Error("token counting acquired fast beta")
				}
				return &http.Response{
					StatusCode: 200, Header: make(http.Header),
					Body: io.NopCloser(strings.NewReader(`{"input_tokens":7}`)), Request: req,
				}, nil
			},
		),
	)
	req := cliproxyexecutor.Request{
		Model:   "claude-opus-5-5",
		Payload: []byte(`{"model":"claude-opus-5-5","messages":[{"role":"user","content":"hello"}]}`),
	}
	opts := cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatClaude}
	executor := NewClaudeExecutor(cfg)
	if _, err := executor.CountTokens(
		ctx, &cliproxyauth.Auth{Attributes: map[string]string{"api_key": "synthetic-key"}}, req, opts,
	); err != nil {
		t.Fatal(err)
	}
	if len(sent) == 0 || gjson.GetBytes(sent, "speed").Exists() {
		t.Fatalf("model speed changed CountTokens request: %s", sent)
	}
	if _, err := executor.CountTokens(context.Background(), nil, req, opts); err != nil {
		t.Fatalf("local CountTokens contract changed: %v", err)
	}
}

func TestRequestObservationsActualExecutors(t *testing.T) {
	for _, provider := range []string{"openai", "claude", "iflow", "qwen", "kimi"} {
		for _, stream := range []bool{false, true} {
			t.Run(
				provider+"/"+map[bool]string{false: "nonstream", true: "stream"}[stream], func(t *testing.T) {
					ctx, records := requestObservationCapture(t)
					var sentModel string
					ctx = util.WithRoundTripper(
						ctx, protectedUtlsRoundTripFunc(
							func(req *http.Request) (*http.Response, error) {
								body, err := io.ReadAll(req.Body)
								if err != nil {
									return nil, err
								}
								sentModel = gjson.GetBytes(body, "model").String()
								payload := `{"id":"synthetic","object":"chat.completion","model":"returned-model","provider":"reported-channel","service_tier":"default","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`
								contentType := "application/json"
								if provider == "claude" {
									payload = `{"id":"synthetic","type":"message","role":"assistant","model":"returned-model","provider":"reported-channel","content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1,"service_tier":"standard","speed":"standard"}}`
								}
								if stream {
									contentType = "text/event-stream"
									payload = "data: {\"id\":\"synthetic\",\"object\":\"chat.completion.chunk\",\"model\":\"returned-model\",\"provider\":\"reported-channel\",\"service_tier\":\"default\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"ok\"},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":1,\"completion_tokens\":1,\"total_tokens\":2}}\n\ndata: [DONE]\n\n"
									if provider == "claude" {
										payload = "data: {\"type\":\"message_start\",\"message\":{\"id\":\"synthetic\",\"model\":\"returned-model\",\"provider\":\"reported-channel\",\"usage\":{\"input_tokens\":1,\"service_tier\":\"standard\",\"speed\":\"standard\"}}}\n\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":1}}\n\ndata: {\"type\":\"message_stop\"}\n\n"
									}
								}
								payload = strings.ReplaceAll(payload, `"provider":"reported-channel",`, "")
								return &http.Response{
									StatusCode: 200, Header: http.Header{
										"Content-Type": {contentType}, "X-Upstream-Provider": {"reported-channel"},
									}, Body: io.NopCloser(strings.NewReader(payload)), Request: req,
								}, nil
							},
						),
					)
					auth := &cliproxyauth.Auth{
						Attributes: map[string]string{
							"api_key": "synthetic-key", "base_url": "https://upstream.example.test",
						}, Metadata: map[string]any{"access_token": "synthetic-key"},
					}
					var executor interface {
						Execute(
							context.Context,
							*cliproxyauth.Auth,
							cliproxyexecutor.Request,
							cliproxyexecutor.Options,
						) (cliproxyexecutor.Response, error)
						ExecuteStream(
							context.Context,
							*cliproxyauth.Auth,
							cliproxyexecutor.Request,
							cliproxyexecutor.Options,
						) (*cliproxyexecutor.StreamResult, error)
					}
					executor = NewOpenAICompatExecutor("openai", &config.Config{})
					format := sdktranslator.FormatOpenAI
					switch provider {
					case "iflow":
						executor = NewIFlowExecutor(&config.Config{})
					case "qwen":
						executor = NewQwenExecutor(&config.Config{})
					case "kimi":
						executor = NewKimiExecutor(&config.Config{})
					}
					if provider == "claude" {
						executor = NewClaudeExecutor(&config.Config{})
						format = sdktranslator.FormatClaude
					}
					req := cliproxyexecutor.Request{
						Model:   "routed-model",
						Payload: []byte(`{"model":"client-alias","messages":[{"role":"user","content":"hi"}],"max_tokens":8,"service_tier":"priority","speed":"fast"}`),
					}
					opts := cliproxyexecutor.Options{SourceFormat: format}
					if stream {
						response, err := executor.ExecuteStream(ctx, auth, req, opts)
						if err != nil {
							t.Fatal(err)
						}
						for chunk := range response.Chunks {
							if chunk.Err != nil {
								t.Fatal(chunk.Err)
							}
						}
					} else {
						if _, err := executor.Execute(ctx, auth, req, opts); err != nil {
							t.Fatal(err)
						}
					}
					got := requestObservationRecord(t, records)
					if got.Model != "routed-model" || got.ClientModel != "client-alias" || got.UpstreamModel != sentModel || got.ResponseModel != "returned-model" ||
						got.ResponseURL != got.UpstreamURL || got.ResponseURLStatus != "observed" || got.ResponseURLSource != "http_response_request_url" || got.ResponseProvider != "reported-channel" || got.ResponseProviderSource != "header.X-Upstream-Provider" || !strings.HasPrefix(
						got.UpstreamURL, "https://upstream.example.test/",
					) || got.Failed || !got.Completed {
						t.Fatalf("actual executor observations = %+v, sent model %q", got, sentModel)
					}
					if provider == "claude" && (got.RequestSpeed != "fast" || got.ResponseSpeed != "standard" || got.ResponseServiceTier != "standard") {
						t.Fatalf("Claude speed/tier independence = %+v", got)
					}
					if provider == "openai" && (got.RequestServiceTier != "priority" || got.ResponseServiceTier != "default") {
						t.Fatalf("OpenAI tier downgrade = %+v", got)
					}
				},
			)
		}
	}
}

func TestRequestObservationsHTTPResponseAddress(t *testing.T) {
	for _, outcome := range []string{
		"direct", "redirects", "redirect-limit", "http-error", "transport-error", "missing-request",
	} {
		t.Run(
			outcome, func(t *testing.T) {
				ctx, records := requestObservationCapture(t)
				var initialURL string
				calls := 0
				ctx = util.WithRoundTripper(
					ctx, protectedUtlsRoundTripFunc(
						func(req *http.Request) (*http.Response, error) {
							calls++
							if initialURL == "" {
								initialURL = req.URL.String()
							}
							if outcome == "transport-error" {
								return nil, errors.New("synthetic transport failure")
							}
							response := &http.Response{
								StatusCode: http.StatusOK, Header: make(http.Header), Request: req,
								Body: io.NopCloser(strings.NewReader(`{"model":"returned-model","choices":[{"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`)),
							}
							if outcome == "redirects" && calls < 3 || outcome == "redirect-limit" {
								response.StatusCode = http.StatusTemporaryRedirect
								response.Header.Set(
									"Location",
									"https://initial.example.test/result?api_key=synthetic-secret&stage="+map[bool]string{
										true: "one", false: "two",
									}[calls == 1],
								)
							}
							if outcome == "http-error" {
								response.StatusCode = http.StatusServiceUnavailable
							}
							if outcome == "missing-request" {
								response.Request = nil
							}
							return response, nil
						},
					),
				)
				_, err := NewOpenAICompatExecutor("openai", &config.Config{}).Execute(
					ctx,
					&cliproxyauth.Auth{
						Attributes: map[string]string{
							"api_key": "synthetic-key", "base_url": "https://initial.example.test",
						},
					},
					cliproxyexecutor.Request{
						Model:   "routed-model",
						Payload: []byte(`{"model":"routed-model","messages":[{"role":"user","content":"hi"}]}`),
					},
					cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatOpenAI},
				)
				failed := outcome == "http-error" || outcome == "transport-error" || outcome == "redirect-limit"
				if (err != nil) != failed {
					t.Fatalf("error = %v, want failure %v", err, failed)
				}
				got := requestObservationRecord(t, records)
				if got.UpstreamURL != initialURL || got.Failed != failed {
					t.Fatalf("initial URL or outcome changed: %+v", got)
				}
				wantURL, wantStatus := initialURL, "observed"
				switch outcome {
				case "redirects":
					wantURL = util.SanitizeUpstreamURL("https://initial.example.test/result?api_key=synthetic-secret&stage=two")
					if calls != 3 {
						t.Fatalf("redirect calls = %d", calls)
					}
				case "redirect-limit":
					wantURL = util.SanitizeUpstreamURL("https://initial.example.test/result?api_key=synthetic-secret&stage=two")
					if calls != 10 {
						t.Fatalf("default redirect limit changed: %d requests", calls)
					}
				case "transport-error":
					wantURL, wantStatus = "", "no_response"
				case "missing-request":
					wantURL, wantStatus = "", "unavailable"
				}
				if got.ResponseURL != wantURL || got.ResponseURLStatus != wantStatus || strings.Contains(
					got.ResponseURL, "synthetic-secret",
				) {
					t.Fatalf("final URL = %+v, want %q (%s)", got, wantURL, wantStatus)
				}
			},
		)
	}
}

func TestRequestObservationsPreserveRedirectPolicy(t *testing.T) {
	ctx, records := requestObservationCapture(t)
	reporter := newUsageReporter(ctx, "openai", "routed-model", nil)
	ctx = reporter.withContext(ctx)
	endpoint := "https://initial.example.test/responses"
	recordUpstreamRequest(ctx, nil, endpoint, nil, nil, "openai", nil)
	client := newProxyAwareHTTPClient(
		util.WithRoundTripper(
			ctx, protectedUtlsRoundTripFunc(
				func(req *http.Request) (*http.Response, error) {
					return &http.Response{
						StatusCode: http.StatusTemporaryRedirect,
						Header:     http.Header{"Location": {"https://other.example.test/response"}},
						Body:       io.NopCloser(strings.NewReader("")), Request: req,
					}, nil
				},
			),
		), nil, nil, 0,
	)
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	tracked := reporter.trackHTTPClient(client)
	request, _ := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	response, err := tracked.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	recordAPIResponseMetadata(ctx, nil, response.StatusCode, response.Header, response)
	reporter.publish(ctx, usage.Detail{})
	got := requestObservationRecord(t, records)
	if got.ResponseURL != endpoint || got.ResponseURLStatus != "observed" || client.CheckRedirect == nil {
		t.Fatalf("redirect policy changed: %+v", got)
	}
}

func TestRequestObservationsWebsocketAddress(t *testing.T) {
	for _, outcome := range []string{"handshake", "reused", "rejected", "rejected-logging", "transport-error"} {
		t.Run(
			outcome, func(t *testing.T) {
				ctx, records := requestObservationCapture(t)
				reporter := newUsageReporter(ctx, "codex", "routed-model", nil)
				ctx = reporter.withContext(ctx)
				info := codexWebsocketRequestLog(
					"wss://initial.example.test/responses", nil, []byte(`{"model":"forwarded-model"}`), "codex", nil,
				)
				recordAPIWebsocketRequest(ctx, nil, info)
				request, _ := http.NewRequest(
					http.MethodGet, "https://handshake.example.test/responses?key=synthetic-secret", nil,
				)
				response := &http.Response{
					StatusCode: http.StatusSwitchingProtocols, Header: make(http.Header), Request: request,
				}
				switch outcome {
				case "handshake":
					recordCodexWebsocketHandshake(ctx, nil, response)
				case "reused":
					recordCodexWebsocketHandshake(ctx, nil, nil)
				case "rejected", "rejected-logging":
					response.StatusCode = http.StatusForbidden
					var cfg *config.Config
					if outcome == "rejected-logging" {
						cfg = &config.Config{}
						cfg.RequestLog = true
						ctx = util.WithGinContext(ctx, &gin.Context{})
					}
					recordAPIWebsocketUpgradeRejection(
						ctx, cfg, websocketUpgradeRequestLog(info), response.StatusCode, response.Header, nil, response,
					)
				}
				reporter.publish(ctx, usage.Detail{})
				got := requestObservationRecord(t, records)
				if got.UpstreamURL != info.URL || got.UpstreamModel != "forwarded-model" {
					t.Fatalf("handshake logging reset initial target: %+v", got)
				}
				if outcome == "reused" {
					if got.ResponseURL != "" || got.ResponseURLStatus != "connection_reused" {
						t.Fatalf("reused handshake invented URL: %+v", got)
					}
				} else if outcome == "transport-error" {
					if got.ResponseURL != "" || got.ResponseURLStatus != "no_response" {
						t.Fatalf("failed handshake invented URL: %+v", got)
					}
				} else if got.ResponseURL != util.SanitizeUpstreamURL(request.URL.String()) || got.ResponseURLSource != "ws_handshake_request_url" || got.ResponseURLStatus != "observed" {
					t.Fatalf("handshake URL = %+v", got)
				}
			},
		)
	}
}

func TestRequestObservationsAIStudioBridgeFinalURL(t *testing.T) {
	for _, mode := range []string{"execute", "stream", "stream-final-url", "arbitrary", "http-error"} {
		for _, finalURL := range []string{
			"", "https://user:synthetic-secret@final.example.test/result?api_key=synthetic-secret&region=test",
		} {
			t.Run(
				mode+map[bool]string{true: "/provided", false: "/legacy"}[finalURL != ""], func(t *testing.T) {
					ctx, records := requestObservationCapture(t)
					ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
					defer cancel()
					connected := make(chan struct{})
					manager := wsrelay.NewManager(
						wsrelay.Options{
							ProviderFactory: func(*http.Request) (
								string,
								error,
							) {
								return "synthetic-bridge", nil
							}, OnConnected: func(string) { close(connected) },
						},
					)
					server := httptest.NewServer(manager.Handler())
					t.Cleanup(server.Close)
					t.Cleanup(func() { _ = manager.Stop(context.Background()) })
					client, handshake, err := websocket.DefaultDialer.Dial(
						"ws"+strings.TrimPrefix(
							server.URL, "http",
						)+manager.Path(), nil,
					)
					if handshake != nil && handshake.Body != nil {
						_ = handshake.Body.Close()
					}
					if err != nil {
						t.Fatal(err)
					}
					t.Cleanup(func() { _ = client.Close() })
					select {
					case <-connected:
					case <-ctx.Done():
						t.Fatal(ctx.Err())
					}
					clientDone := make(chan error, 1)
					go func() {
						var envelope map[string]any
						if err := client.ReadJSON(&envelope); err != nil {
							clientDone <- err
							return
						}
						payload := map[string]any{
							"status": http.StatusOK,
							"body":   `{"candidates":[{"content":{"role":"model","parts":[{"text":"ok"}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":1,"candidatesTokenCount":1,"totalTokenCount":2}}`,
						}
						if finalURL != "" {
							payload["final_url"] = finalURL
						}
						if mode == "http-error" {
							payload["status"] = http.StatusServiceUnavailable
						}
						if mode == "stream-final-url" {
							if err := client.WriteJSON(
								map[string]any{
									"id": envelope["id"], "type": wsrelay.MessageTypeStreamStart,
									"payload": map[string]any{"status": http.StatusOK},
								},
							); err != nil {
								clientDone <- err
								return
							}
							clientDone <- client.WriteJSON(
								map[string]any{
									"id": envelope["id"], "type": wsrelay.MessageTypeHTTPResp, "payload": payload,
								},
							)
							return
						}
						if mode == "stream" {
							if err := client.WriteJSON(
								map[string]any{
									"id": envelope["id"], "type": wsrelay.MessageTypeStreamStart, "payload": payload,
								},
							); err != nil {
								clientDone <- err
								return
							}
							if err := client.WriteJSON(
								map[string]any{
									"id": envelope["id"], "type": wsrelay.MessageTypeStreamChunk,
									"payload": map[string]any{"data": payload["body"]},
								},
							); err != nil {
								clientDone <- err
								return
							}
							clientDone <- client.WriteJSON(
								map[string]any{
									"id": envelope["id"], "type": wsrelay.MessageTypeStreamEnd,
								},
							)
							return
						}
						clientDone <- client.WriteJSON(
							map[string]any{
								"id": envelope["id"], "type": wsrelay.MessageTypeHTTPResp, "payload": payload,
							},
						)
					}()
					executor := NewAIStudioExecutor(&config.Config{}, "aistudio", manager)
					auth := &cliproxyauth.Auth{ID: "synthetic-bridge"}
					req := cliproxyexecutor.Request{
						Model:   "gemini-3.7-flash",
						Payload: []byte(`{"contents":[{"role":"user","parts":[{"text":"hi"}]}]}`),
					}
					opts := cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatGemini}
					if mode == "arbitrary" {
						reporter := newUsageReporter(ctx, "aistudio", req.Model, nil)
						ctx = reporter.withContext(ctx)
						request, _ := http.NewRequestWithContext(
							ctx, http.MethodPost, "https://initial.example.test/generate", nil,
						)
						recordUpstreamRequest(ctx, nil, request.URL.String(), nil, nil, "aistudio", nil)
						response, requestErr := executor.HttpRequest(ctx, auth, request)
						if requestErr != nil {
							t.Fatal(requestErr)
						}
						defer response.Body.Close()
						recordAPIResponseMetadata(ctx, nil, response.StatusCode, response.Header, response)
						reporter.publish(ctx, usage.Detail{})
					} else if mode == "stream" || mode == "stream-final-url" {
						stream, streamErr := executor.ExecuteStream(ctx, auth, req, opts)
						if streamErr != nil {
							t.Fatal(streamErr)
						}
						for chunk := range stream.Chunks {
							if chunk.Err != nil {
								t.Fatal(chunk.Err)
							}
						}
					} else {
						_, requestErr := executor.Execute(ctx, auth, req, opts)
						if (requestErr != nil) != (mode == "http-error") {
							t.Fatalf("execute error = %v", requestErr)
						}
					}
					select {
					case err := <-clientDone:
						if err != nil {
							t.Fatal(err)
						}
					case <-ctx.Done():
						t.Fatal(ctx.Err())
					}
					got := requestObservationRecord(t, records)
					if finalURL == "" {
						if got.ResponseURL != "" || got.ResponseURLSource != "" || got.ResponseURLStatus != "not_provided" {
							t.Fatalf("legacy bridge invented URL: %+v", got)
						}
					} else if got.ResponseURL != util.SanitizeUpstreamURL(finalURL) || got.ResponseURLSource != "bridge_final_url" || got.ResponseURLStatus != "observed" {
						t.Fatalf("bridge URL = %+v", got)
					}
				},
			)
		}
	}
}

func TestRequestObservationsCacheReadPresence(t *testing.T) {
	for _, provider := range []string{"openai", "codex", "claude", "gemini", "interactions"} {
		for _, value := range []string{"missing", "0", "3", "null", "-1"} {
			t.Run(
				provider+"/"+value, func(t *testing.T) {
					field := ""
					if value != "missing" {
						field = `,"input_tokens_details":{"cached_tokens":` + value + `}`
					}
					var detail usage.Detail
					switch provider {
					case "openai":
						detail = parseOpenAIUsage([]byte(`{"usage":{"input_tokens":10,"output_tokens":2` + field + `}}`))
					case "codex":
						detail, _ = parseCodexUsage([]byte(`{"response":{"usage":{"input_tokens":10,"output_tokens":2` + field + `}}}`))
					case "claude":
						field = ""
						if value != "missing" {
							field = `,"cache_read_input_tokens":` + value
						}
						detail = parseClaudeUsage([]byte(`{"usage":{"input_tokens":10,"output_tokens":2,"cache_creation_input_tokens":5` + field + `}}`))
					case "gemini":
						field = ""
						if value != "missing" {
							field = `,"cachedContentTokenCount":` + value
						}
						detail = parseGeminiUsage([]byte(`{"usageMetadata":{"promptTokenCount":10,"candidatesTokenCount":2` + field + `}}`))
					case "interactions":
						field = ""
						if value != "missing" {
							field = `,"cache_read_tokens":` + value
						}
						detail = parseInteractionsUsage([]byte(`{"usage":{"input_tokens":10,"output_tokens":2,"cached_tokens":5` + field + `}}`))
					}
					reported := value == "0" || value == "3"
					if detail.CacheReadReported != reported {
						t.Fatalf("parsed presence=%v, detail=%+v", reported, detail)
					}
					ctx, records := requestObservationCapture(t)
					reporter := newUsageReporter(ctx, provider, "routed-model", nil)
					reporter.publish(ctx, detail)
					record := requestObservationRecord(t, records)
					if record.CacheReadReported != reported || record.Detail.CacheReadReported != reported {
						t.Fatalf("published presence = %+v", record)
					}
					if value == "0" && record.Detail.CacheReadTokens != 0 || value == "3" && record.Detail.CacheReadTokens != 3 {
						t.Fatalf("published read count changed: %+v", record.Detail)
					}
				},
			)
		}
	}
	for _, value := range []string{"0", "3"} {
		t.Run(
			"stream/"+value, func(t *testing.T) {
				var openai streamUsageBuffer
				openai.observeOpenAIStream([]byte(`data: {"usage":{"input_tokens":10,"output_tokens":1,"input_tokens_details":{"cached_tokens":` + value + `}}}`))
				openai.observeOpenAIStream([]byte(`data: {"usage":{"input_tokens":10,"output_tokens":2}}`))
				if !openai.detail.CacheReadReported || fmt.Sprint(openai.detail.CacheReadTokens) != value {
					t.Fatalf("OpenAI stream lost reported cache: %+v", openai.detail)
				}
				var claude claudeStreamUsageBuffer
				claude.observe([]byte(`data: {"type":"message_start","message":{"usage":{"input_tokens":10,"cache_read_input_tokens":` + value + `}}}`))
				claude.observe([]byte(`data: {"type":"message_delta","usage":{"output_tokens":2}}`))
				if !claude.detail.CacheReadReported || fmt.Sprint(claude.detail.CacheReadTokens) != value {
					t.Fatalf("Claude stream lost reported cache: %+v", claude.detail)
				}
				var gemini streamUsageBuffer
				gemini.observeGeminiStream([]byte(`{"usageMetadata":{"promptTokenCount":10,"cachedContentTokenCount":` + value + `}}`))
				gemini.observeGeminiStream([]byte(`{"usageMetadata":{"candidatesTokenCount":2}}`))
				if !gemini.detail.CacheReadReported || fmt.Sprint(gemini.detail.CacheReadTokens) != value {
					t.Fatalf("Gemini stream lost reported cache: %+v", gemini.detail)
				}
			},
		)
	}
}

func TestRequestObservationsBillingRawQuantities(t *testing.T) {
	claude := parseClaudeUsage([]byte(`{"usage":{"input_tokens":10,"cache_read_input_tokens":3,"cache_creation_input_tokens":2,"output_tokens":2,"inference_geo":"us"}}`))
	if claude.BillingMetadata == nil || claude.BillingMetadata.ContextInputTokens == nil || *claude.BillingMetadata.ContextInputTokens != 15 || claude.BillingMetadata.Region != "us" || claude.BillingMetadata.RegionSource != "usage.inference_geo" {
		t.Fatalf("Claude billing metadata=%+v", claude.BillingMetadata)
	}
	gemini := parseGeminiUsage([]byte(`{"usageMetadata":{"promptTokenCount":10,"toolUsePromptTokenCount":4,"cachedContentTokenCount":3,"candidatesTokenCount":2,"thoughtsTokenCount":1,"totalTokenCount":17,"promptTokensDetails":[{"modality":"TEXT","tokenCount":6},{"modality":"IMAGE","tokenCount":4}],"cacheTokensDetails":[{"modality":"TEXT","tokenCount":2},{"modality":"IMAGE","tokenCount":1}],"toolUsePromptTokensDetails":[{"modality":"TEXT","tokenCount":1},{"modality":"IMAGE","tokenCount":3}],"candidatesTokensDetails":[{"modality":"TEXT","tokenCount":2}]}}`))
	if gemini.BillingMetadata == nil || gemini.BillingMetadata.ContextInputTokens == nil || *gemini.BillingMetadata.ContextInputTokens != 10 || gemini.InputTokens != 14 {
		t.Fatalf("Gemini context includes tool use: %+v", gemini)
	}
	want := []usage.ModalityTokenUsage{
		{Category: "uncached_input", Modality: "text", Tokens: 5},
		{Category: "cache_read", Modality: "text", Tokens: 2}, {Category: "output", Modality: "text", Tokens: 2},
		{Category: "uncached_input", Modality: "image", Tokens: 6},
		{Category: "cache_read", Modality: "image", Tokens: 1},
	}
	if !reflect.DeepEqual(gemini.BillingMetadata.Modalities, want) {
		t.Fatalf("Gemini modality groups=%+v", gemini.BillingMetadata.Modalities)
	}
	for _, node := range []string{
		`{"input_tokens":100,"output_tokens":2,"input_tokens_details":{"cached_tokens":3,"cache_write_tokens":20,"cache_write_tokens_details":{"ttl_5m_tokens":3,"ttl_1h_tokens":4,"ttl_30m_tokens":5,"ttl_12h_tokens":8}}}`,
		`{"prompt_tokens":100,"completion_tokens":2,"prompt_tokens_details":{"cached_tokens":3,"cache_write_tokens":20,"cache_creation":{"ephemeral_5m_input_tokens":3,"ephemeral_1h_input_tokens":4}},"input_tokens_details":{"cache_write_tokens_details":{"ttl_30m_tokens":5,"ttl_12h_tokens":8}}}`,
	} {
		detail := parseOpenAIStyleUsageNode(gjson.Parse(node))
		if detail.CacheCreationTokens != 20 || detail.CacheCreation5mInputTokens != 3 || detail.CacheCreation1hInputTokens != 4 || detail.CacheCreation30mInputTokens != 5 || detail.CacheCreation12hInputTokens != 8 || detail.TokenBreakdown.Input.UncachedTokens != 77 || detail.BillingMetadata == nil || *detail.BillingMetadata.ContextInputTokens != 100 {
			t.Fatalf("TTL detail duplicated aggregate: %+v", detail)
		}
	}
	image, ok := parseCodexImageToolUsage([]byte(`{"response":{"tool_usage":{"image_gen":{"input_tokens":10,"output_tokens":4,"total_tokens":14,"cached_tokens":3,"input_tokens_details":{"text_tokens":6,"image_tokens":4},"output_tokens_details":{"text_tokens":1,"image_tokens":3},"cached_tokens_details":{"cached_text_tokens":2,"cached_image_tokens":1}}}}}`))
	if !ok || !image.CacheReadReported || image.CacheReadTokens != 3 || image.BillingMetadata == nil || !image.BillingMetadata.ChargesReported || len(image.BillingMetadata.Modalities) != 6 {
		t.Fatalf("image child usage=%+v", image)
	}
	ctx, records := requestObservationCapture(t)
	reporter := newUsageReporter(ctx, "codex", "main-model", nil)
	reporter.publishAdditionalModel(ctx, "gpt-image-2", image)
	child := requestObservationRecord(t, records)
	if child.Detail.InputTokens != 10 || child.Detail.CacheReadTokens != 3 || child.BillingMetadata == nil || len(child.BillingMetadata.Modalities) != 6 {
		t.Fatalf("image child metadata lost: %+v", child)
	}
}

func TestRequestObservationsKimiHeaderTTLRetryIsolation(t *testing.T) {
	ctx, records := requestObservationCapture(t)
	reporter := newUsageReporter(ctx, "kimi", "kimi-k3", nil)
	ctx = reporter.withContext(ctx)
	recordUpstreamRequest(
		ctx, nil, "https://synthetic.example.test/chat/completions", nil, []byte(`{"model":"kimi-k3"}`), "kimi", nil,
	)
	recordAPIResponseMetadata(
		ctx, nil, 200, http.Header{"Msh-Usage-Cache-Write-Tokens-5m": {"0"}, "Msh-Usage-Cache-Write-Tokens-1h": {"7"}},
	)
	if reporter.cacheWrite5mTokens == nil || *reporter.cacheWrite5mTokens != 0 || reporter.cacheWrite1hTokens == nil || *reporter.cacheWrite1hTokens != 7 {
		t.Fatal("response TTL headers lost their explicit zero")
	}
	recordUpstreamRequest(
		ctx, nil, "https://synthetic.example.test/chat/completions", nil, []byte(`{"model":"kimi-k3"}`), "kimi", nil,
	)
	reporter.publishFailure(ctx, errors.New("synthetic retry failure"))
	got := requestObservationRecord(t, records)
	if reporter.cacheWrite5mTokens != nil || reporter.cacheWrite1hTokens != nil || got.Detail.CacheCreation5mInputTokens != 0 || got.Detail.CacheCreation1hInputTokens != 0 {
		t.Fatalf("retry retained TTL headers: %+v", got.Detail)
	}
	ctx, records = requestObservationCapture(t)
	reporter = newUsageReporter(ctx, "kimi", "kimi-k3", nil)
	ctx = reporter.withContext(ctx)
	recordUpstreamRequest(
		ctx, nil, "https://synthetic.example.test/chat/completions", nil, []byte(`{"model":"kimi-k3"}`), "kimi", nil,
	)
	recordAPIResponseMetadata(
		ctx, nil, 200, http.Header{"Msh-Usage-Cache-Write-Tokens-5m": {"0"}, "Msh-Usage-Cache-Write-Tokens-1h": {"7"}},
	)
	reporter.publish(ctx, usage.Detail{CacheCreationTokens: 7})
	got = requestObservationRecord(t, records)
	if got.Detail.CacheCreationTokens != 7 || got.Detail.CacheCreation5mInputTokens != 0 || got.Detail.CacheCreation1hInputTokens != 7 {
		t.Fatalf("TTL headers not published independently: %+v", got.Detail)
	}
}

func TestRequestObservationsChargesNeedCompleteReportedSources(t *testing.T) {
	for _, test := range []struct {
		name, provider, endpoint, request, response string
		failed, reported                            bool
		quantity                                    float64
	}{
		{
			name: "ordinary", provider: "claude", request: `{"tools":[]}`,
			response: `{"type":"message","stop_reason":"end_turn"}`, reported: true,
		},
		{
			name: "function", provider: "openai", request: `{"tools":[{"type":"function"}]}`,
			response: `{"choices":[{"finish_reason":"stop"}]}`, reported: true,
		},
		{
			name: "reported-search", provider: "claude", request: `{"tools":[{"type":"web_search_20250305"}]}`,
			response: `{"type":"message","stop_reason":"end_turn","usage":{"server_tool_use":{"web_search_requests":2}}}`,
			reported: true, quantity: 2,
		},
		{
			name: "free-code-with-reported-search", provider: "claude",
			request:  `{"tools":[{"type":"web_search_20260209"},{"type":"code_execution_20250825"}]}`,
			response: `{"type":"message","stop_reason":"end_turn","usage":{"server_tool_use":{"web_search_requests":2,"code_execution_requests":1}}}`,
			reported: true, quantity: 2,
		},
		{
			name: "search-unknown", provider: "claude", request: `{"tools":[{"type":"web_search_20250305"}]}`,
			response: `{"type":"message","stop_reason":"end_turn"}`,
		},
		{
			name: "code-without-duration", provider: "claude",
			request:  `{"tools":[{"type":"code_execution_20250825"}]}`,
			response: `{"type":"message","stop_reason":"end_turn","usage":{"server_tool_use":{"code_execution_requests":1}}}`,
		},
		{
			name: "incomplete", provider: "claude", request: `{}`,
			response: `{"type":"message_start","message":{"usage":{"input_tokens":1}}}`,
		},
		{
			name: "failed", provider: "claude", request: `{}`, response: `{"type":"message","stop_reason":"end_turn"}`,
			failed: true,
		},
		{
			name: "gemini-search-not-billable-count", provider: "gemini", request: `{"tools":[{"googleSearch":{}}]}`,
			response: `{"candidates":[{"finishReason":"STOP","groundingMetadata":{"webSearchQueries":["one","two"]}}]}`,
		},
		{
			name: "undeclared-hosted-search", provider: "openai", request: `{}`,
			response: `{"type":"response.completed","response":{"output":[{"type":"web_search_call","status":"completed"}]}}`,
		},
		{
			name: "undeclared-code", provider: "claude", request: `{}`,
			response: `{"type":"message","stop_reason":"end_turn","usage":{"server_tool_use":{"code_execution_requests":1}}}`,
		},
		{
			name: "meta-produced-images", provider: "meta",
			endpoint: "https://synthetic.example.test/images/generations", request: `{"n":5}`,
			response: `{"data":[{"url":"https://synthetic.example.test/one"},{"b64_json":"synthetic-image"}]}`,
			reported: true, quantity: 2,
		},
	} {
		t.Run(
			test.name, func(t *testing.T) {
				ctx, records := requestObservationCapture(t)
				reporter := newUsageReporter(ctx, test.provider, "routed-model", nil)
				ctx = reporter.withContext(ctx)
				endpoint := test.endpoint
				if endpoint == "" {
					endpoint = "https://synthetic.example.test/responses"
				}
				recordUpstreamRequest(ctx, nil, endpoint, nil, []byte(test.request), test.provider, nil)
				appendAPIResponseChunk(ctx, nil, []byte(test.response))
				if test.failed {
					reporter.publishFailure(ctx, errors.New("synthetic failed response"))
				} else {
					reporter.publish(ctx, usage.Detail{})
				}
				got := requestObservationRecord(t, records)
				if got.BillingMetadata == nil || got.BillingMetadata.ChargesReported != test.reported {
					t.Fatalf("charge coverage=%+v", got.BillingMetadata)
				}
				if test.quantity > 0 && (len(got.BillingMetadata.Charges) != 1 || got.BillingMetadata.Charges[0].Quantity != test.quantity) {
					t.Fatalf("charge quantities=%+v", got.BillingMetadata.Charges)
				}
			},
		)
	}
}

func TestRequestObservationsWebsocketConnectionURLLifecycle(t *testing.T) {
	for _, provider := range []string{"codex", "xai"} {
		t.Run(
			provider, func(t *testing.T) {
				server := httptest.NewServer(
					http.HandlerFunc(
						func(w http.ResponseWriter, req *http.Request) {
							conn, err := (&websocket.Upgrader{}).Upgrade(w, req, nil)
							if err != nil {
								return
							}
							defer conn.Close()
							for {
								if _, _, err := conn.ReadMessage(); err != nil {
									return
								}
							}
						},
					),
				)
				t.Cleanup(server.Close)
				var executor interface {
					ensureUpstreamConn(
						context.Context,
						*cliproxyauth.Auth,
						*codexWebsocketSession,
						string,
						string,
						http.Header,
					) (*websocket.Conn, *websocketConnectionCloser, *http.Response, error)
					invalidateUpstreamConn(*codexWebsocketSession, *websocket.Conn, string, error)
				}
				if provider == "codex" {
					executor = newCodexWebsocketsExecutor(&config.Config{})
				} else {
					executor = newXAIWebsocketsExecutor(&config.Config{})
				}
				session := &codexWebsocketSession{sessionID: "synthetic-session"}
				auth := &cliproxyauth.Auth{ID: "synthetic-auth"}
				var previous *websocketConnectionCloser
				for index, path := range []string{"/first", "/first", "/second"} {
					ctx, records := requestObservationCapture(t)
					reporter := newUsageReporter(ctx, provider, "routed-model", nil)
					ctx = reporter.withContext(ctx)
					endpoint := "ws" + strings.TrimPrefix(server.URL, "http") + path
					recordAPIWebsocketRequest(ctx, nil, codexWebsocketRequestLog(endpoint, nil, nil, provider, auth))
					conn, closer, response, err := executor.ensureUpstreamConn(
						ctx, auth, session, auth.ID, endpoint, nil,
					)
					if err != nil {
						t.Fatal(err)
					}
					if provider == "codex" {
						recordCodexWebsocketHandshake(ctx, nil, response, closer)
					} else {
						recordXAIWebsocketHandshake(ctx, nil, response, closer)
					}
					reporter.publish(ctx, usage.Detail{})
					got := requestObservationRecord(t, records)
					wantSource := "ws_handshake_request_url"
					if index == 1 {
						wantSource = "ws_connection_handshake_url"
						if closer != previous || response != nil {
							t.Fatal("session did not reuse its connection")
						}
					}
					if closer.responseURL == "" || got.ResponseURL != closer.responseURL || got.ResponseURLSource != wantSource || got.ResponseURLStatus != "observed" {
						t.Fatalf("connection address observation=%+v, resource URL=%q", got, closer.responseURL)
					}
					if index == 2 && closer == previous {
						t.Fatal("replacement retained the old connection address")
					}
					previous = closer
					if index == 2 {
						executor.invalidateUpstreamConn(session, conn, "test-completed", nil)
						if session.conn != nil || session.connCloser != nil {
							t.Fatal("closed session retained connection metadata")
						}
					}
				}
			},
		)
	}
}

func TestRequestObservationsBillingDeliveryIsolated(t *testing.T) {
	ctx, records := requestObservationCapture(t)
	metadata := &usage.BillingMetadata{
		SchemaVersion: 1, ContextInputTokens: new(int64(10)),
		Modalities: []usage.ModalityTokenUsage{{Category: "output", Modality: "image", Tokens: 1}},
		Charges:    []usage.BillingQuantity{{Kind: "web_search", Unit: "request", Quantity: 2}}, ChargesReported: true,
	}
	if err := usage.PublishRecord(
		ctx, usage.Record{Detail: usage.Detail{CacheReadReported: true, BillingMetadata: metadata}},
	); err != nil {
		t.Fatal(err)
	}
	*metadata.ContextInputTokens = 999
	metadata.Modalities[0].Tokens = 999
	metadata.Charges[0].Quantity = 999
	got := requestObservationRecord(t, records)
	if !got.CacheReadReported || got.BillingMetadata == nil || *got.BillingMetadata.ContextInputTokens != 10 || got.BillingMetadata.Modalities[0].Tokens != 1 || got.BillingMetadata.Charges[0].Quantity != 2 {
		t.Fatalf("queued billing metadata retained caller-owned data: %+v", got.BillingMetadata)
	}
	if err := usage.PublishRecord(ctx, usage.Record{Failed: true, BillingMetadata: metadata}); err != nil {
		t.Fatal(err)
	}
	failed := requestObservationRecord(t, records)
	if failed.BillingMetadata.ChargesReported || failed.Detail.BillingMetadata.ChargesReported {
		t.Fatal("failed record retained complete fee coverage")
	}
}

func TestRequestObservationsGeminiModalityStreamMerge(t *testing.T) {
	var buffer streamUsageBuffer
	buffer.observeGeminiStream([]byte(`{"usageMetadata":{"promptTokenCount":10,"cachedContentTokenCount":3,"promptTokensDetails":[{"modality":"TEXT","tokenCount":10}],"cacheTokensDetails":[{"modality":"TEXT","tokenCount":3}]}}`))
	buffer.observeGeminiStream([]byte(`{"usageMetadata":{"toolUsePromptTokenCount":4,"toolUsePromptTokensDetails":[{"modality":"TEXT","tokenCount":4}],"candidatesTokenCount":2,"candidatesTokensDetails":[{"modality":"TEXT","tokenCount":2}]}}`))
	metadata := buffer.detail.BillingMetadata
	if metadata == nil || metadata.ContextInputTokens == nil || *metadata.ContextInputTokens != 10 {
		t.Fatalf("stream lost raw context: %+v", metadata)
	}
	want := []usage.ModalityTokenUsage{
		{Category: "uncached_input", Modality: "text", Tokens: 11},
		{Category: "cache_read", Modality: "text", Tokens: 3}, {Category: "output", Modality: "text", Tokens: 2},
	}
	if !reflect.DeepEqual(metadata.Modalities, want) {
		t.Fatalf("stream double-counted prompt/cache/tool groups: %+v", metadata.Modalities)
	}
	before := buffer.detail
	buffer.observeGeminiStream([]byte(`{"usageMetadata":{"promptTokenCount":0,"candidatesTokenCount":0,"totalTokenCount":0,"cachedContentTokenCount":0}}`))
	if buffer.detail.InputTokens != before.InputTokens || buffer.detail.OutputTokens != before.OutputTokens || buffer.detail.CacheReadTokens != before.CacheReadTokens {
		t.Fatal("zero placeholder changed the existing stream quantity contract")
	}
	buffer.observeGeminiStream([]byte(`{"usageMetadata":{"cachedContentTokenCount":0}}`))
	want = []usage.ModalityTokenUsage{
		{Category: "uncached_input", Modality: "text", Tokens: 14}, {Category: "output", Modality: "text", Tokens: 2},
	}
	if !reflect.DeepEqual(buffer.detail.BillingMetadata.Modalities, want) {
		t.Fatalf("corrected cache count retained stale modality usage: %+v", buffer.detail.BillingMetadata.Modalities)
	}
	buffer.observeGeminiStream([]byte(`{"usageMetadata":{"promptTokenCount":20}}`))
	want = []usage.ModalityTokenUsage{
		{Category: "uncached_input", Modality: "text", Tokens: 4}, {Category: "output", Modality: "text", Tokens: 2},
	}
	if !reflect.DeepEqual(buffer.detail.BillingMetadata.Modalities, want) {
		t.Fatalf("changed prompt count retained stale modality usage: %+v", buffer.detail.BillingMetadata.Modalities)
	}
}

func TestRequestObservationsBillingRegionsRequireOfficialEndpoint(t *testing.T) {
	for _, test := range []struct{ provider, endpoint, region string }{
		{"xai", "https://api.x.ai/v1/responses", "global"}, {"xai", "https://us.api.x.ai/v1/responses", "us"},
		{"xai", "https://proxy.example.test/us/responses", ""}, {
			"vertex",
			"https://us-central1-aiplatform.googleapis.com/v1/projects/example/locations/us-central1/publishers/google/models/example:generateContent",
			"us-central1",
		},
		{
			"vertex",
			"https://aiplatform.googleapis.com/v1/projects/example/locations/us-central1/publishers/google/models/example:generateContent",
			"us-central1",
		},
		{
			"vertex",
			"https://us-central1.aiplatform.googleapis.com/v1/projects/example/locations/us-central1/publishers/google/models/example:generateContent",
			"",
		},
		{"vertex", "https://proxy.example.test/v1/projects/example/locations/us-central1/models/example", ""},
	} {
		t.Run(
			test.endpoint, func(t *testing.T) {
				ctx, records := requestObservationCapture(t)
				reporter := newUsageReporter(ctx, test.provider, "routed-model", nil)
				ctx = reporter.withContext(ctx)
				recordUpstreamRequest(ctx, nil, test.endpoint, nil, []byte(`{}`), test.provider, nil)
				request, _ := http.NewRequest(http.MethodPost, test.endpoint, nil)
				recordAPIResponseMetadata(ctx, nil, 200, nil, &http.Response{Request: request})
				reporter.publish(ctx, usage.Detail{})
				got := requestObservationRecord(t, records)
				if got.BillingMetadata == nil || got.BillingMetadata.Region != test.region {
					t.Fatalf("endpoint inferred an unsupported region: %+v", got.BillingMetadata)
				}
			},
		)
	}
}
