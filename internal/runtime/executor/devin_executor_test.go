package executor

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	devinauth "github.com/Pyrokine/CLIProxyAPI/v6/internal/auth/devin"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/runtime/executor/helps"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/util"
	cliproxyauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/usage"
	sdktranslator "github.com/Pyrokine/CLIProxyAPI/v6/sdk/translator"
	"github.com/google/uuid"
	"github.com/tidwall/gjson"
	"google.golang.org/protobuf/encoding/protowire"
)

type devinTestRoundTripper func(*http.Request) (*http.Response, error)

func (roundTripper devinTestRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	return roundTripper(req)
}

func TestDevinExecutorIdentityAndWireHeaders(t *testing.T) {
	executor := NewDevinExecutor(&config.Config{})
	if executor.Identifier() != "devin" {
		t.Fatalf("Identifier() = %q, want devin", executor.Identifier())
	}
	if format := executor.RequestToFormat(cliproxyexecutor.Request{}, cliproxyexecutor.Options{}); format != sdktranslator.FormatInteractions {
		t.Fatalf("RequestToFormat() = %q, want interactions", format)
	}

	auth := &cliproxyauth.Auth{Attributes: map[string]string{"api_key": "session-token"}}
	request, err := http.NewRequest(http.MethodPost, "https://server.codeium.com/chat", nil)
	if err != nil {
		t.Fatalf("NewRequest() error = %v", err)
	}
	if err = executor.PrepareRequest(request, auth); err != nil {
		t.Fatalf("PrepareRequest() error = %v", err)
	}
	if got := request.Header.Get("Authorization"); got != "Basic session-token-session-token" {
		t.Fatalf("Authorization = %q", got)
	}
	if got := request.Header.Get("Content-Type"); got != "application/connect+proto" {
		t.Fatalf("Content-Type = %q", got)
	}
	if got := request.Header.Get("Connect-Protocol-Version"); got != "1" {
		t.Fatalf("Connect-Protocol-Version = %q", got)
	}
	if got := request.Header.Get("Sentry-Trace"); got == "" {
		t.Fatal("Sentry-Trace is empty")
	}

	var receivedUserAgent []string
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		receivedUserAgent = request.Header["User-Agent"]
		writer.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()
	wireRequest, err := http.NewRequest(http.MethodPost, server.URL, nil)
	if err != nil {
		t.Fatalf("NewRequest() error = %v", err)
	}
	if err = executor.PrepareRequest(wireRequest, auth); err != nil {
		t.Fatalf("PrepareRequest() error = %v", err)
	}
	response, err := server.Client().Do(wireRequest)
	if err != nil {
		t.Fatalf("Do() error = %v", err)
	}
	_ = response.Body.Close()
	if len(receivedUserAgent) != 0 {
		t.Fatalf("wire User-Agent = %#v, want omitted", receivedUserAgent)
	}
}

func TestPrepareDevinRequestConsumesSensitiveWordsOnlyInSystemPrompt(t *testing.T) {
	executor := NewDevinExecutor(&config.Config{
		Devin: config.DevinConfig{SensitiveWords: []string{"SECRET"}},
	})
	auth := &cliproxyauth.Auth{
		ID: "devin-a.json",
		Attributes: map[string]string{
			"api_key":     "devin-session-token$test",
			"device_seed": "device-a",
		},
	}
	payload := []byte(`{
		"system_instruction":"keep one\nremove SECRET line\nkeep two",
		"generation_config":{"max_output_tokens":200000,"temperature":0},
		"input":[
			{"type":"user_input","content":[{"type":"text","text":"ordinary SECRET history"}]},
			{"type":"function_call","id":"call-1","name":"tool","arguments":{"value":"SECRET"}}
		],
		"tools":[{"name":"tool","description":"SECRET description","parameters":{"const":"SECRET"}}]
	}`)

	prepared, err := executor.prepareDevinRequest(
		context.Background(),
		auth,
		cliproxyexecutor.Request{Model: "devin/swe-2", Payload: payload},
		cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatInteractions},
		false,
	)
	if err != nil {
		t.Fatalf("prepareDevinRequest() error = %v", err)
	}
	framed, err := io.ReadAll(prepared.httpReq.Body)
	if err != nil {
		t.Fatalf("read request body: %v", err)
	}
	flag, protoBody, err := helps.ReadConnectFrame(bytes.NewReader(framed))
	if err != nil || flag != helps.ConnectFlagData {
		t.Fatalf("ReadConnectFrame() = flag %d, error %v", flag, err)
	}

	if got := string(firstDevinBytesField(t, protoBody, 2)); got != "keep one\nkeep two" {
		t.Fatalf("wire system prompt = %q", got)
	}
	promptFields := allDevinBytesFields(t, protoBody, 3)
	if len(promptFields) != 2 {
		t.Fatalf("prompt field count = %d, want 2", len(promptFields))
	}
	if got := string(firstDevinBytesField(t, promptFields[0], 3)); got != "ordinary SECRET history" {
		t.Fatalf("user history = %q", got)
	}
	toolCall := firstDevinBytesField(t, promptFields[1], 6)
	if got := string(firstDevinBytesField(t, toolCall, 3)); got != `{"value":"SECRET"}` {
		t.Fatalf("tool arguments = %q", got)
	}
	toolDefinition := firstDevinBytesField(t, protoBody, 10)
	if got := string(firstDevinBytesField(t, toolDefinition, 2)); got != "SECRET description" {
		t.Fatalf("tool description = %q", got)
	}
	if got := string(firstDevinBytesField(t, toolDefinition, 3)); got != `{"const":"SECRET"}` {
		t.Fatalf("tool schema = %q", got)
	}
	completion := firstDevinBytesField(t, protoBody, 8)
	if got := firstDevinVarintField(t, completion, 2); got != 64000 {
		t.Fatalf("max output tokens = %d, want 64000", got)
	}
	if got := string(firstDevinBytesField(t, protoBody, 21)); got != "swe-2-high" {
		t.Fatalf("chat model uid = %q, want swe-2-high", got)
	}
	if _, upstream := prepared.reporter.responseModelSnapshot(); upstream != "swe-2-high" {
		t.Fatalf("expected upstream model = %q, want swe-2-high", upstream)
	}
	logBody := devinRequestLogBody(prepared.body, "keep one\nkeep two")
	if strings.Contains(string(logBody), "remove SECRET line") {
		t.Fatal("request log body retained removed system line")
	}
	const token = "devin-session-token$test"
	if bytes.Contains(logBody, []byte(token)) {
		t.Fatal("request log body exposed the Devin session token")
	}
	var headerLog strings.Builder
	writeHeaders(&headerLog, prepared.httpReq.Header)
	if strings.Contains(headerLog.String(), token) {
		t.Fatal("request header log exposed the Devin session token")
	}
	authType, authValue := auth.AccountInfo()
	if formatted := formatAuthInfo(upstreamRequestLog{AuthType: authType, AuthValue: authValue}); strings.Contains(formatted, token) {
		t.Fatal("request auth log exposed the Devin session token")
	}
}

func TestPrepareDevinRequestFiltersResponsesToolsThroughWireAndLog(t *testing.T) {
	exec := NewDevinExecutor(&config.Config{})
	auth := &cliproxyauth.Auth{ID: "devin-offline-test", Attributes: map[string]string{
		"api_key": "test-session-token", "device_seed": "device-offline-test",
	}}
	payload := []byte(`{
		"model":"devin/swe-2","instructions":"system instruction","input":"hello",
		"tool_choice":{"type":"function","name":"automation_update","namespace":"mcp__codex_app"},
		"tools":[
			{"type":"namespace","name":"mcp__codex_app","children":[
				{"type":"function","name":"automation_update","description":"Recurring automations"},
				{"type":"function","name":"read_resource","description":"Read a resource"}
			]},
			{"type":"namespace","name":"mcp__terminal","children":[{"type":"function","name":"exec_command","description":"returning output or a session ID for ongoing interaction"}]},
			{"type":"function","name":"write_stdin","description":"Writes characters to an existing unified exec session and returns recent output."}
		]
	}`)
	prepared, err := exec.prepareDevinRequest(
		context.Background(), auth,
		cliproxyexecutor.Request{Model: "devin/swe-2", Payload: payload},
		cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatOpenAIResponse}, false,
	)
	if err != nil {
		t.Fatalf("prepareDevinRequest: %v", err)
	}
	framed, err := io.ReadAll(prepared.httpReq.Body)
	if err != nil {
		t.Fatalf("read wire request: %v", err)
	}
	_, wire, err := helps.ReadConnectFrame(bytes.NewReader(framed))
	if err != nil {
		t.Fatalf("read Connect frame: %v", err)
	}
	wireTools := allDevinBytesFields(t, wire, 10)
	wantNames := []string{"mcp__codex_app__read_resource", "mcp__terminal__exec_command", "write_stdin"}
	if len(wireTools) != len(wantNames) {
		t.Fatalf("wire tools = %d, want %d", len(wireTools), len(wantNames))
	}
	for i, tool := range wireTools {
		if name := string(firstDevinBytesField(t, tool, 1)); name != wantNames[i] {
			t.Fatalf("wire tool %d name = %q, want %q", i, name, wantNames[i])
		}
	}
	if got := string(firstDevinBytesField(t, wireTools[1], 2)); got != "returning output or an session ID for ongoing interaction" {
		t.Fatalf("wire exec_command description = %q", got)
	}
	if got := string(firstDevinBytesField(t, wireTools[2], 2)); got != "Writes characters to a existing unified exec session and returns recent output." {
		t.Fatalf("wire write_stdin description = %q", got)
	}
	if gjson.GetBytes(prepared.body, "generation_config.tool_choice").Exists() {
		t.Fatalf("filtered tool remained forced in translated request: %s", prepared.body)
	}
	logged := devinRequestLogBody(prepared.body, "system instruction")
	loggedTools := gjson.GetBytes(logged, "tools").Array()
	if len(loggedTools) != len(wireTools) {
		t.Fatalf("logged tools = %s, want %d", loggedTools, len(wireTools))
	}
	for i, tool := range loggedTools {
		if tool.Get("name").String() != wantNames[i] || string(firstDevinBytesField(t, wireTools[i], 2)) != tool.Get("description").String() {
			t.Fatalf("wire/log tool mismatch: wire=%v logged=%s", wireTools[i], tool.Raw)
		}
	}
}

func TestResolveDevinSessionScopesCanonicalIdentityByAuth(t *testing.T) {
	metadata := map[string]any{cliproxyexecutor.ExecutionSessionMetadataKey: "execution-42"}
	authA := &cliproxyauth.Auth{ID: "devin-a.json"}
	authB := &cliproxyauth.Auth{ID: "devin-b.json"}

	sessionA1, cascadeA1 := resolveDevinSessionAndCascadeIDs(
		context.Background(), authA, "request-session", "request-cascade", metadata,
	)
	sessionA2, cascadeA2 := resolveDevinSessionAndCascadeIDs(
		context.Background(), authA, "other-session", "other-cascade", metadata,
	)
	sessionB, cascadeB := resolveDevinSessionAndCascadeIDs(
		context.Background(), authB, "request-session", "request-cascade", metadata,
	)
	if sessionA1 != sessionA2 || cascadeA1 != cascadeA2 || sessionA1 != cascadeA1 {
		t.Fatalf("same canonical identity changed: %q/%q versus %q/%q", sessionA1, cascadeA1, sessionA2, cascadeA2)
	}
	if sessionA1 == sessionB || cascadeA1 == cascadeB {
		t.Fatalf("different auth identities shared Devin session %q", sessionA1)
	}
	if _, err := uuid.Parse(sessionA1); err != nil {
		t.Fatalf("session is not UUID: %q", sessionA1)
	}

	rawUUID := "8176cf8a-feff-44c1-8e3e-b10f6d737ae1"
	unscoped, cascade := resolveDevinSessionAndCascadeIDs(
		context.Background(), nil, rawUUID, rawUUID, nil,
	)
	if unscoped != rawUUID || cascade != rawUUID {
		t.Fatalf("unscoped UUID = %q/%q, want %q", unscoped, cascade, rawUUID)
	}
}

func TestDevinExecuteUsesConnectResponseAndPublishesUsage(t *testing.T) {
	plugin := &captureUsagePlugin{records: make(chan usage.Record, 2)}
	registerTestUsagePlugin(t, plugin)
	var requestPath string
	var authorization string
	var requestFlag byte
	ctx := util.WithRoundTripper(
		context.Background(),
		devinTestRoundTripper(func(request *http.Request) (*http.Response, error) {
			requestPath = request.URL.Path
			authorization = request.Header.Get("Authorization")
			body, err := io.ReadAll(request.Body)
			if err != nil {
				t.Fatalf("read request body: %v", err)
			}
			requestFlag, _, err = helps.ReadConnectFrame(bytes.NewReader(body))
			if err != nil {
				t.Fatalf("read request frame: %v", err)
			}

			frame := testDevinBytesField(nil, 3, []byte("answer"))
			usageBody := testDevinVarintField(nil, 2, 2)
			usageBody = testDevinVarintField(usageBody, 3, 3)
			usageBody = testDevinVarintField(usageBody, 5, 1)
			usageBody = testDevinBytesField(usageBody, 9, []byte("devin/swe-3"))
			frame = testDevinBytesField(frame, 7, usageBody)
			stream := append(helps.WrapConnectEnvelope(frame), helps.WrapConnectEnvelopeWithFlag(
				helps.ConnectFlagEndStream,
				[]byte(`{}`),
			)...)
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     http.Header{"Content-Type": []string{"application/connect+proto"}},
				Body:       io.NopCloser(bytes.NewReader(stream)),
				Request:    request,
			}, nil
		}),
	)

	executor := NewDevinExecutor(&config.Config{})
	response, err := executor.Execute(
		ctx,
		&cliproxyauth.Auth{
			ID: "devin.json",
			Attributes: map[string]string{
				"api_key":  "devin-session-token$test",
				"base_url": "https://devin.test",
			},
		},
		cliproxyexecutor.Request{
			Model:   "devin/swe-2",
			Payload: []byte(`{"input":[{"type":"user_input","content":"hi"}]}`),
		},
		cliproxyexecutor.Options{
			SourceFormat:   sdktranslator.FormatInteractions,
			ResponseFormat: sdktranslator.FormatInteractions,
		},
	)
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if requestPath != helps.DevinChatPath || requestFlag != helps.ConnectFlagData {
		t.Fatalf("request path/flag = %q/%d", requestPath, requestFlag)
	}
	if authorization != "Basic devin-session-token$test-devin-session-token$test" {
		t.Fatalf("Authorization = %q", authorization)
	}
	if got := gjson.GetBytes(response.Payload, "steps.0.content.0.text").String(); got != "answer" {
		t.Fatalf("response text = %q, payload = %s", got, response.Payload)
	}
	if got := gjson.GetBytes(response.Payload, "usage.total_input_tokens").Int(); got != 3 {
		t.Fatalf("total input tokens = %d", got)
	}
	if got := gjson.GetBytes(response.Payload, "usage.total_output_tokens").Int(); got != 3 {
		t.Fatalf("total output tokens = %d", got)
	}
	record := testWaitForDevinUsageRecord(t, plugin.records)
	if record.Failed || record.Model != "devin/swe-2" || record.ResponseModel != "devin/swe-3" ||
		record.Detail.InputTokens != 3 || record.Detail.OutputTokens != 3 {
		t.Fatalf("Devin non-stream served model record = %#v", record)
	}
}

func TestDevinRefreshPreservesPresenceAndRemovesStaleQuotaValues(t *testing.T) {
	statusResponse := testDevinStatusResponse(t, true, 0, false, 0)
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path != devinauth.DevinGetUserStatusPath {
			http.NotFound(writer, request)
			return
		}
		writer.Header().Set("Content-Type", "application/proto")
		_, _ = writer.Write(statusResponse)
	}))
	defer server.Close()

	auth := &cliproxyauth.Auth{
		ID:         "devin-refresh.json",
		Index:      "devin-stable-index",
		Provider:   "devin",
		Generation: 42,
		Attributes: map[string]string{
			"api_key":                           "devin-session-token$test",
			"base_url":                          server.URL,
			cliproxyauth.AttributeAuthIndexSeed: "devin-stable-user",
		},
		Metadata: map[string]any{
			"weekly_quota_remaining_percent": int64(90),
			"weekly_quota_reset_at":          "2026-09-20T00:00:00Z",
		},
		Quota: cliproxyauth.QuotaState{
			Generation: 9,
			Signals: map[string]string{
				"weekly_quota_present":           "true",
				"weekly_quota_remaining_percent": "90%",
				"weekly_quota_reset_at":          "2026-09-20T00:00:00Z",
			},
		},
	}
	executor := NewDevinExecutor(&config.Config{})
	updated, err := executor.Refresh(context.Background(), auth)
	if err != nil {
		t.Fatalf("Refresh() error = %v", err)
	}
	if updated == auth {
		t.Fatal("Refresh() returned original auth pointer")
	}
	if updated.Index != auth.Index ||
		updated.Attributes[cliproxyauth.AttributeAuthIndexSeed] != auth.Attributes[cliproxyauth.AttributeAuthIndexSeed] {
		t.Fatalf("Refresh() changed Devin identity: index=%q attributes=%#v", updated.Index, updated.Attributes)
	}
	for _, key := range []string{
		"daily_quota_present",
		"daily_quota_remaining_percent",
		"weekly_quota_present",
		"weekly_quota_remaining_percent",
		"weekly_quota_reset_at",
		"quota_observed_at",
	} {
		if _, exists := updated.Metadata[key]; exists {
			t.Fatalf("transient quota key %q remained in metadata", key)
		}
	}
	if updated.Quota.Signals["daily_quota_present"] != "true" ||
		updated.Quota.Signals["daily_quota_remaining_percent"] != "0%" {
		t.Fatalf("daily quota signals = %#v", updated.Quota.Signals)
	}
	if updated.Quota.Signals["weekly_quota_present"] != "false" {
		t.Fatalf("weekly quota presence = %#v", updated.Quota.Signals)
	}
	if _, exists := updated.Quota.Signals["weekly_quota_remaining_percent"]; exists {
		t.Fatal("stale weekly quota signal was retained")
	}
	if _, exists := updated.Quota.Signals["weekly_quota_reset_at"]; exists {
		t.Fatal("stale weekly reset signal was retained")
	}
	if updated.Metadata["plan"] != "Pro" || updated.Metadata["email"] != "devin@example.com" {
		t.Fatalf("identity metadata = %#v", updated.Metadata)
	}
	if updated.Generation != 42 || updated.Quota.Generation != 9 {
		t.Fatalf("refresh changed selected auth generation: auth=%d quota=%d", updated.Generation, updated.Quota.Generation)
	}
	if updated.Quota.ObservedAt.IsZero() || updated.Quota.ObservedAt.Location() != time.UTC ||
		updated.LastRefreshedAt.IsZero() || updated.LastRefreshedAt.Location() != time.UTC {
		t.Fatalf("refresh timestamps = quota:%v auth:%v", updated.Quota.ObservedAt, updated.LastRefreshedAt)
	}
	if _, exists := auth.Metadata["daily_quota_present"]; exists ||
		auth.Quota.Signals["weekly_quota_remaining_percent"] != "90%" {
		t.Fatal("Refresh() mutated original auth")
	}
}

func TestDevinCountTokensMarksEstimate(t *testing.T) {
	executor := NewDevinExecutor(&config.Config{})
	response, err := executor.CountTokens(
		context.Background(),
		nil,
		cliproxyexecutor.Request{Payload: []byte("12345678")},
		cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatInteractions},
	)
	if err != nil {
		t.Fatalf("CountTokens() error = %v", err)
	}
	if got := gjson.GetBytes(response.Payload, "input_tokens").Int(); got != 2 {
		t.Fatalf("input_tokens = %d, want 2", got)
	}
	if !gjson.GetBytes(response.Payload, "estimated").Bool() {
		t.Fatalf("payload does not mark estimate: %s", response.Payload)
	}
	if estimated, _ := response.Metadata["estimated"].(bool); !estimated {
		t.Fatalf("metadata = %#v", response.Metadata)
	}

	translated, err := executor.CountTokens(
		context.Background(),
		nil,
		cliproxyexecutor.Request{Payload: []byte("12345678")},
		cliproxyexecutor.Options{
			SourceFormat:   sdktranslator.FormatInteractions,
			ResponseFormat: sdktranslator.FormatClaude,
		},
	)
	if err != nil {
		t.Fatalf("CountTokens(translated) error = %v", err)
	}
	if got := gjson.GetBytes(translated.Payload, "input_tokens").Int(); got != 2 ||
		!gjson.GetBytes(translated.Payload, "estimated").Bool() {
		t.Fatalf("translated estimate = %s", translated.Payload)
	}
}

func firstDevinBytesField(t *testing.T, data []byte, wanted protowire.Number) []byte {
	t.Helper()
	values := allDevinBytesFields(t, data, wanted)
	if len(values) == 0 {
		t.Fatalf("protobuf field %d not found", wanted)
	}
	return values[0]
}

func allDevinBytesFields(t *testing.T, data []byte, wanted protowire.Number) [][]byte {
	t.Helper()
	var values [][]byte
	for len(data) > 0 {
		number, wireType, consumed := protowire.ConsumeTag(data)
		if consumed <= 0 {
			t.Fatalf("consume tag: %v", protowire.ParseError(consumed))
		}
		data = data[consumed:]
		if wireType == protowire.BytesType {
			value, size := protowire.ConsumeBytes(data)
			if size <= 0 {
				t.Fatalf("consume field %d: %v", number, protowire.ParseError(size))
			}
			if number == wanted {
				values = append(values, bytes.Clone(value))
			}
			data = data[size:]
			continue
		}
		size := protowire.ConsumeFieldValue(number, wireType, data)
		if size <= 0 {
			t.Fatalf("skip field %d: %v", number, protowire.ParseError(size))
		}
		data = data[size:]
	}
	return values
}

func firstDevinVarintField(t *testing.T, data []byte, wanted protowire.Number) uint64 {
	t.Helper()
	for len(data) > 0 {
		number, wireType, consumed := protowire.ConsumeTag(data)
		if consumed <= 0 {
			t.Fatalf("consume tag: %v", protowire.ParseError(consumed))
		}
		data = data[consumed:]
		if wireType == protowire.VarintType {
			value, size := protowire.ConsumeVarint(data)
			if size <= 0 {
				t.Fatalf("consume field %d: %v", number, protowire.ParseError(size))
			}
			if number == wanted {
				return value
			}
			data = data[size:]
			continue
		}
		size := protowire.ConsumeFieldValue(number, wireType, data)
		if size <= 0 {
			t.Fatalf("skip field %d: %v", number, protowire.ParseError(size))
		}
		data = data[size:]
	}
	t.Fatalf("protobuf varint field %d not found", wanted)
	return 0
}

func testDevinBytesField(data []byte, number protowire.Number, value []byte) []byte {
	data = protowire.AppendTag(data, number, protowire.BytesType)
	return protowire.AppendBytes(data, value)
}

func testDevinVarintField(data []byte, number protowire.Number, value uint64) []byte {
	data = protowire.AppendTag(data, number, protowire.VarintType)
	return protowire.AppendVarint(data, value)
}

func testDevinStatusResponse(
	t *testing.T,
	dailyPresent bool,
	daily uint64,
	weeklyPresent bool,
	weekly uint64,
) []byte {
	t.Helper()
	planInfo := testDevinBytesField(nil, 2, []byte("Pro"))
	planStatus := testDevinBytesField(nil, 1, planInfo)
	if dailyPresent {
		planStatus = testDevinVarintField(planStatus, 14, daily)
	}
	if weeklyPresent {
		planStatus = testDevinVarintField(planStatus, 15, weekly)
	}
	userStatus := testDevinBytesField(nil, 3, []byte("devin-user"))
	userStatus = testDevinBytesField(userStatus, 7, []byte("devin@example.com"))
	userStatus = testDevinBytesField(userStatus, 13, planStatus)
	return testDevinBytesField(nil, 1, userStatus)
}

func TestPrepareDevinNativeRulesPreserveCallerAliasesAndWire(t *testing.T) {
	models := []config.PayloadModelRule{{Name: "devin/swe-2", Protocol: "devin"}}
	for _, test := range []struct {
		name    string
		payload string
		rules   config.PayloadConfig
		max     uint64
		removed bool
		invalid bool
		model   string
		cascade bool
	}{
		{name: "override", payload: `{"input":[{"type":"user_input","content":"hello"}]}`, rules: config.PayloadConfig{Override: []config.PayloadRule{{Models: models, Params: map[string]any{"completion_config.max_tokens": 1234}}}}, max: 1234},
		{name: "default", payload: `{"input":[{"type":"user_input","content":"hello"}]}`, rules: config.PayloadConfig{Default: []config.PayloadRule{{Models: models, Params: map[string]any{"completion_config.max_tokens": 1234}}}}, max: 1234},
		{name: "default_raw", payload: `{"input":[{"type":"user_input","content":"hello"}]}`, rules: config.PayloadConfig{DefaultRaw: []config.PayloadRule{{Models: models, Params: map[string]any{"completion_config.max_tokens": "1234"}}}}, max: 1234},
		{name: "snake", payload: `{"generation_config":{"max_output_tokens":17},"input":[{"type":"user_input","content":"hello"}]}`, max: 17},
		{name: "camel", payload: `{"generationConfig":{"maxOutputTokens":17},"input":[{"type":"user_input","content":"hello"}]}`, max: 17},
		{name: "snake_parent_camel_field", payload: `{"generation_config":{"maxOutputTokens":17},"input":[{"type":"user_input","content":"hello"}]}`, max: 17},
		{name: "camel_parent_snake_field", payload: `{"generationConfig":{"max_output_tokens":17},"input":[{"type":"user_input","content":"hello"}]}`, max: 17},
		{name: "remove", payload: `{"system_instruction":"caller","input":[{"type":"user_input","content":"hello"}]}`, rules: config.PayloadConfig{Filter: []config.PayloadFilterRule{{Models: models, Params: []string{"system_prompt", "completion_config"}}}}, removed: true},
		{name: "invalid_raw", payload: `{"input":[{"type":"user_input","content":"hello"}]}`, rules: config.PayloadConfig{OverrideRaw: []config.PayloadRule{{Models: models, Params: map[string]any{"system_prompt": "{"}}}}, invalid: true},
		{name: "model_observation", payload: `{"input":[{"type":"user_input","content":"hello"}]}`, rules: config.PayloadConfig{Override: []config.PayloadRule{{Models: models, Params: map[string]any{"model": "synthetic-native-model", "completion_config.max_tokens": 1234}}}}, max: 1234, model: "synthetic-native-model"},
		{name: "cascade_alias", payload: `{"cascadeId":"caller-cascade","input":[{"type":"user_input","content":"hello"}]}`, rules: config.PayloadConfig{Default: []config.PayloadRule{{Models: models, Params: map[string]any{"cascade_id": "rule-cascade", "completion_config.max_tokens": 1234}}}}, max: 1234, cascade: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			if test.max == 17 {
				test.rules = config.PayloadConfig{Default: []config.PayloadRule{{Models: models, Params: map[string]any{"completion_config.max_tokens": 1234}}}}
			}
			executor := NewDevinExecutor(&config.Config{Payload: test.rules})
			auth := &cliproxyauth.Auth{ID: t.Name(), Attributes: map[string]string{"api_key": "synthetic-session", "device_seed": "synthetic-device"}}
			prepared, err := executor.prepareDevinRequest(context.Background(), auth, cliproxyexecutor.Request{Model: "devin/swe-2", Payload: []byte(test.payload)}, cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatInteractions}, false)
			if test.invalid {
				if err == nil {
					t.Fatal("invalid native JSON returned a prepared request")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			framed, err := io.ReadAll(prepared.httpReq.Body)
			if err != nil {
				t.Fatal(err)
			}
			_, wire, err := helps.ReadConnectFrame(bytes.NewReader(framed))
			if err != nil {
				t.Fatal(err)
			}
			completion := allDevinBytesFields(t, wire, 8)
			if test.removed {
				if len(completion) != 0 || len(allDevinBytesFields(t, wire, 2)) != 0 {
					t.Fatal("removed native fields were reinserted")
				}
			} else if len(completion) != 1 || firstDevinVarintField(t, completion[0], 2) != test.max {
				t.Fatalf("native max_tokens=%x, want %d", completion, test.max)
			}
			if test.model != "" {
				_, observed := prepared.reporter.responseModelSnapshot()
				if string(firstDevinBytesField(t, wire, 21)) != test.model || observed != test.model {
					t.Fatalf("wire/observed model differs: wire=%q observed=%q", firstDevinBytesField(t, wire, 21), observed)
				}
			}
			if test.cascade && string(firstDevinBytesField(t, wire, 16)) == "rule-cascade" {
				t.Fatal("native default replaced an explicit caller cascade")
			}
		})
	}
}

func TestDevinMillionContextModelUIDAtRequestBuilder(t *testing.T) {
	for _, stream := range []bool{false, true} {
		for _, level := range []string{"none", "max"} {
			t.Run(fmt.Sprintf("%s/stream=%v", level, stream), func(t *testing.T) {
				executor := NewDevinExecutor(&config.Config{})
				auth := &cliproxyauth.Auth{ID: "synthetic-devin", Provider: "devin", Attributes: map[string]string{
					"api_key": "synthetic-session-token", "base_url": "http://example.invalid", "device_seed": "synthetic-device"}}
				payload := []byte(fmt.Sprintf(`{"generation_config":{"thinking_level":%q,"max_output_tokens":200000},"input":[{"type":"user_input","content":[{"type":"text","text":"hello"}]}]}`, level))
				prepared, err := executor.prepareDevinRequest(context.Background(), auth,
					cliproxyexecutor.Request{Model: "devin/glm-5-2-1m", Payload: payload},
					cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatInteractions}, stream)
				if err != nil {
					t.Fatalf("prepareDevinRequest: %v", err)
				}
				body, err := io.ReadAll(prepared.httpReq.Body)
				_ = prepared.httpReq.Body.Close()
				if err != nil {
					t.Fatalf("read prepared body: %v", err)
				}
				flag, protoBody, err := helps.ReadConnectFrame(bytes.NewReader(body))
				if err != nil || flag != helps.ConnectFlagData {
					t.Fatalf("ReadConnectFrame = %d, %v", flag, err)
				}
				want := "glm-5-2-" + level + "-1m"
				if got := string(firstDevinBytesField(t, protoBody, 21)); got != want {
					t.Fatalf("wire chat_model_uid = %q, want %q", got, want)
				}
				completion := firstDevinBytesField(t, protoBody, 8)
				if limit := firstDevinVarintField(t, completion, 2); limit != 64000 {
					t.Fatalf("wire max tokens = %d, want catalog limit 64000", limit)
				}
			})
		}
	}
}
