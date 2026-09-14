package executor

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/util"
	cliproxyauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
	sdktranslator "github.com/Pyrokine/CLIProxyAPI/v6/sdk/translator"
	"github.com/tidwall/gjson"
)

var claudeSystemCloakingTestTime = time.Date(2026, time.August, 28, 12, 0, 0, 0, time.UTC)

func cloakClaudeSystemForTest(payload []byte, strictMode bool) []byte {
	return checkClaudeSystemInstructionsWithSigningModeAt(
		payload,
		strictMode,
		true,
		"2.1.220",
		"cli",
		"",
		claudeSystemCloakingTestTime,
	)
}

func assertClaudeSystemTestCurrentDate(t *testing.T, block gjson.Result) {
	t.Helper()
	text := block.Get("text").String()
	if !isClaudeCodeCurrentDateReminder(text) || !strings.Contains(text, "Today's date is 2026-08-28.") {
		t.Fatalf("current-date block = %q", text)
	}
	if block.Get("cache_control").Exists() {
		t.Fatalf("current-date block unexpectedly has cache_control: %s", block.Raw)
	}
}

func assertClaudeSystemTestUserText(t *testing.T, block gjson.Result, want string) {
	t.Helper()
	if got := block.Get("type").String(); got != "text" {
		t.Fatalf("user block type = %q, want text", got)
	}
	if got := block.Get("text").String(); got != want {
		t.Fatalf("user block text = %q, want %q", got, want)
	}
	if got := block.Get("cache_control.type").String(); got != "ephemeral" {
		t.Fatalf("user block cache_control.type = %q, want ephemeral", got)
	}
}

func assertClaudeSystemTestMessage(t *testing.T, payload []byte, index int, want string) {
	t.Helper()
	message := gjson.GetBytes(payload, "messages."+strconv.Itoa(index))
	if got := message.Get("role").String(); got != "system" {
		t.Fatalf("messages[%d].role = %q, want system: %s", index, got, payload)
	}
	content := message.Get("content").Array()
	if len(content) != 1 || content[0].Get("text").String() != want {
		t.Fatalf("messages[%d].content = %s, want %q", index, message.Get("content").Raw, want)
	}
	if got := content[0].Get("cache_control.type").String(); got != "ephemeral" {
		t.Fatalf("messages[%d] cache_control.type = %q, want ephemeral", index, got)
	}
}

func TestClaudeSystemCloakingPreservesCallerBlockBoundaries(t *testing.T) {
	tests := []struct {
		name   string
		model  string
		legacy bool
	}{
		{name: "mid-conversation system", model: "claude-opus-5"},
		{name: "legacy reminders", model: "claude-opus-4-6", legacy: true},
	}
	for _, test := range tests {
		t.Run(
			test.name, func(t *testing.T) {
				payload := []byte(`{"model":"` + test.model + `","system":[` +
					`{"type":"text","text":"first guidance","cache_control":{"type":"ephemeral","ttl":"1h"}},` +
					`{"type":"text","text":"second <guidance> & \"quotes\""}],` +
					`"messages":[{"role":"user","content":"hi"}]}`)

				out := cloakClaudeSystemForTest(payload, false)
				if got := gjson.GetBytes(out, "system.#").Int(); got != 2 {
					t.Fatalf("top-level system block count = %d, want 2: %s", got, out)
				}
				if got := gjson.GetBytes(out, "system.1.text").String(); got != claudeCodeCLIIdentity {
					t.Fatalf("system identity = %q, want %q", got, claudeCodeCLIIdentity)
				}
				if got := gjson.GetBytes(out, "system.1.cache_control.type").String(); got != "ephemeral" {
					t.Fatalf("identity cache_control.type = %q, want ephemeral", got)
				}
				userContent := gjson.GetBytes(out, "messages.0.content").Array()
				if test.legacy {
					if len(userContent) != 4 {
						t.Fatalf("legacy user content count = %d, want 4: %s", len(userContent), out)
					}
					assertClaudeSystemTestCurrentDate(t, userContent[0])
					for index, want := range []string{"first guidance", `second <guidance> & "quotes"`} {
						block := userContent[index+1]
						if got := block.Get("text").String(); got != claudeCallerSystemReminder(want) {
							t.Fatalf("caller reminder %d = %q, want %q", index, got, claudeCallerSystemReminder(want))
						}
						if block.Get("cache_control").Exists() {
							t.Fatalf("caller reminder %d unexpectedly has cache_control: %s", index, block.Raw)
						}
					}
					assertClaudeSystemTestUserText(t, userContent[3], "hi")
					return
				}
				if len(userContent) != 2 {
					t.Fatalf("user content count = %d, want 2: %s", len(userContent), out)
				}
				assertClaudeSystemTestCurrentDate(t, userContent[0])
				assertClaudeSystemTestUserText(t, userContent[1], "hi")
				if got := gjson.GetBytes(out, "messages.#").Int(); got != 3 {
					t.Fatalf("message count = %d, want user plus two system messages: %s", got, out)
				}
				assertClaudeSystemTestMessage(t, out, 1, "first guidance")
				assertClaudeSystemTestMessage(t, out, 2, `second <guidance> & "quotes"`)
			},
		)
	}
}

func TestClaudeSystemCloakingStrictModeDropsCallerSystem(t *testing.T) {
	payload := []byte(`{"model":"claude-opus-5","system":[{"type":"text","text":"caller directive"}],"messages":[{"role":"user","content":"hi"}]}`)

	out := cloakClaudeSystemForTest(payload, true)
	if bytes.Contains(out, []byte("caller directive")) {
		t.Fatalf("strict mode forwarded caller system: %s", out)
	}
	if got := gjson.GetBytes(out, "system.#").Int(); got != 2 {
		t.Fatalf("top-level system block count = %d, want 2", got)
	}
	content := gjson.GetBytes(out, "messages.0.content").Array()
	if len(content) != 2 {
		t.Fatalf("user content count = %d, want 2", len(content))
	}
	assertClaudeSystemTestCurrentDate(t, content[0])
	assertClaudeSystemTestUserText(t, content[1], "hi")
}

func TestClaudeSystemCloakingIsByteIdempotent(t *testing.T) {
	callerSystem := "\nBEGIN\n# currentDate\nToday's date is caller-owned text.\n" +
		strings.Repeat("Preserve caller semantics exactly.\n", 64) + "END  \n"
	payload := []byte(`{"model":"claude-opus-5","system":` + marshalClaudeJSONString(callerSystem) +
		`,"messages":[{"role":"user","content":"hello"}]}`)

	first := cloakClaudeSystemForTest(payload, false)
	second := cloakClaudeSystemForTest(first, false)
	if !bytes.Equal(first, second) {
		t.Fatalf("cloak layout is not byte-idempotent:\nfirst:  %s\nsecond: %s", first, second)
	}
	assertClaudeSystemTestMessage(t, first, 1, callerSystem)
}

func TestRelocateClaudeSystemPromptForCountTokensPreservesBoundaries(t *testing.T) {
	tests := []struct {
		name       string
		model      string
		strictMode bool
		legacy     bool
	}{
		{name: "mid-conversation", model: "claude-opus-5"},
		{name: "legacy", model: "claude-opus-4-6", legacy: true},
		{name: "strict", model: "claude-opus-5", strictMode: true},
	}
	for _, test := range tests {
		t.Run(
			test.name, func(t *testing.T) {
				payload := []byte(`{"model":"` + test.model + `","system":[` +
					`{"type":"text","text":"first guidance"},{"type":"text","text":"second guidance"}],` +
					`"messages":[{"role":"user","content":"hi"}]}`)
				out := relocateClaudeSystemPromptForCountTokens(payload, test.strictMode)
				if gjson.GetBytes(out, "system").Exists() {
					t.Fatalf("count_tokens system must be absent: %s", out)
				}
				if test.strictMode {
					if got := gjson.GetBytes(out, "messages.#").Int(); got != 1 {
						t.Fatalf("strict message count = %d, want 1: %s", got, out)
					}
					if bytes.Contains(out, []byte("guidance")) {
						t.Fatalf("strict count_tokens forwarded caller system: %s", out)
					}
					return
				}
				if test.legacy {
					content := gjson.GetBytes(out, "messages.0.content").Array()
					if len(content) != 3 {
						t.Fatalf("legacy content count = %d, want 3: %s", len(content), out)
					}
					for index, want := range []string{"first guidance", "second guidance"} {
						if got := content[index].Get("text").String(); got != claudeCallerSystemReminder(want) {
							t.Fatalf("content[%d].text = %q", index, got)
						}
					}
					return
				}
				if got := gjson.GetBytes(out, "messages.#").Int(); got != 3 {
					t.Fatalf("message count = %d, want 3: %s", got, out)
				}
				assertClaudeSystemTestMessage(t, out, 1, "first guidance")
				assertClaudeSystemTestMessage(t, out, 2, "second guidance")
			},
		)
	}
}

func TestValidateClaudeCallerSystemBlocks(t *testing.T) {
	for _, system := range []string{
		`"S1"`,
		`[{"type":"text","text":"S1"},{"type":"text","text":"S2"}]`,
	} {
		if err := validateClaudeCallerSystemBlocks(gjson.Parse(system)); err != nil {
			t.Fatalf("validateClaudeCallerSystemBlocks(%s) error = %v", system, err)
		}
	}

	tests := []struct {
		name      string
		system    string
		wantIndex string
		wantType  string
	}{
		{
			name:      "image",
			system:    `[{"type":"text","text":"S1"},{"type":"image"}]`,
			wantIndex: "system.1.type",
			wantType:  `"image"`,
		},
		{
			name:      "responses marker",
			system:    `[{"type":"input_file"}]`,
			wantIndex: "system.0.type",
			wantType:  `"input_file"`,
		},
		{
			name:      "missing type",
			system:    `[{"text":"S1"}]`,
			wantIndex: "system.0.type",
			wantType:  `"unknown"`,
		},
	}
	for _, test := range tests {
		t.Run(
			test.name, func(t *testing.T) {
				errValidate := validateClaudeCallerSystemBlocks(gjson.Parse(test.system))
				if errValidate == nil {
					t.Fatal("validation error = nil, want rejection")
				}
				var statusCoder interface{ StatusCode() int }
				if !errors.As(errValidate, &statusCoder) || statusCoder.StatusCode() != http.StatusBadRequest {
					t.Fatalf("validation error = %v, want HTTP 400", errValidate)
				}
				var requestScoped interface{ IsRequestScoped() bool }
				if !errors.As(errValidate, &requestScoped) || !requestScoped.IsRequestScoped() {
					t.Fatalf("validation error = %v, want request-scoped error", errValidate)
				}
				if !strings.Contains(errValidate.Error(), test.wantIndex) || !strings.Contains(
					errValidate.Error(), test.wantType,
				) {
					t.Fatalf("validation error = %q, want %s and %s", errValidate, test.wantIndex, test.wantType)
				}
			},
		)
	}
}

func TestApplyCloakingHandlesNonTextCallerSystemByMode(t *testing.T) {
	payload := []byte(`{"model":"claude-opus-5","system":[{"type":"input_image"}],"messages":[{"role":"user","content":"U1"}]}`)
	auth := &cliproxyauth.Auth{Attributes: map[string]string{"api_key": "key-123"}}

	out, cloaked, errCloaking := applyCloaking(
		context.Background(),
		&config.Config{},
		auth,
		payload,
		"claude-opus-5",
		"key-123",
		false,
	)
	if errCloaking == nil || out != nil || cloaked {
		t.Fatalf("non-strict applyCloaking = (%s, %v, %v), want local rejection", out, cloaked, errCloaking)
	}

	auth.Attributes["cloak_strict_mode"] = "true"
	out, cloaked, errCloaking = applyCloaking(
		context.Background(),
		&config.Config{},
		auth,
		payload,
		"claude-opus-5",
		"key-123",
		false,
	)
	if errCloaking != nil || !cloaked {
		t.Fatalf("strict applyCloaking = (%s, %v, %v), want success", out, cloaked, errCloaking)
	}
	if got := gjson.GetBytes(out, "system.#").Int(); got != 2 {
		t.Fatalf("strict system block count = %d, want 2: %s", got, out)
	}
}

func TestClaudeCountTokensRejectsNonTextCallerSystemBeforeUpstream(t *testing.T) {
	upstreamCalled := false
	roundTripper := protectedUtlsRoundTripFunc(
		func(req *http.Request) (*http.Response, error) {
			upstreamCalled = true
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     http.Header{"Content-Type": {"application/json"}},
				Body:       io.NopCloser(strings.NewReader(`{"input_tokens":1}`)),
				Request:    req,
			}, nil
		},
	)
	ctx := util.WithRoundTripper(context.Background(), roundTripper)
	auth := &cliproxyauth.Auth{Attributes: map[string]string{"api_key": "sk-ant-api03-test"}}
	payload := []byte(`{"model":"claude-opus-5","system":[{"type":"text","text":"S1"},{"type":"input_image"}],"messages":[{"role":"user","content":"x"}]}`)

	_, errCount := NewClaudeExecutor(&config.Config{}).countTokensUpstream(
		ctx,
		auth,
		cliproxyexecutor.Request{Model: "claude-opus-5", Payload: payload},
		cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatClaude},
	)
	var statusCoder interface{ StatusCode() int }
	if !errors.As(errCount, &statusCoder) || statusCoder.StatusCode() != http.StatusBadRequest {
		t.Fatalf("countTokensUpstream() error = %v, want HTTP 400", errCount)
	}
	if upstreamCalled {
		t.Fatal("countTokensUpstream() called upstream before rejecting the caller system block")
	}
}

func TestClaudeSystemInsertionFollowsToolResultsAndIsIdempotent(t *testing.T) {
	payload := []byte(`{"messages":[` +
		`{"role":"assistant","content":[{"type":"tool_use","id":"toolu_1","name":"Read","input":{}}]},` +
		`{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_1","content":"ok"},{"type":"text","text":"continue"}]}` +
		`]}`)
	texts := []string{"first guidance", "second guidance"}

	first := prependClaudeSystemRemindersToFirstUserMessage(payload, texts)
	second := prependClaudeSystemRemindersToFirstUserMessage(first, texts)
	if !bytes.Equal(first, second) {
		t.Fatalf("caller reminder insertion is not idempotent:\nfirst:  %s\nsecond: %s", first, second)
	}
	content := gjson.GetBytes(first, "messages.1.content").Array()
	if len(content) != 4 || content[0].Get("type").String() != "tool_result" {
		t.Fatalf(
			"legacy content = %s, want tool result, two reminders, and user text",
			gjson.GetBytes(first, "messages.1.content").Raw,
		)
	}
	for index, text := range texts {
		if got := content[index+1].Get("text").String(); got != claudeCallerSystemReminder(text) {
			t.Fatalf("content[%d].text = %q", index+1, got)
		}
	}

	first = insertClaudeMidConversationSystemMessages(payload, texts)
	second = insertClaudeMidConversationSystemMessages(first, texts)
	if !bytes.Equal(first, second) {
		t.Fatalf("mid-system insertion is not idempotent:\nfirst:  %s\nsecond: %s", first, second)
	}
	if got := gjson.GetBytes(first, "messages.#").Int(); got != 4 {
		t.Fatalf("message count = %d, want assistant, user, and two system messages: %s", got, first)
	}
	assertClaudeSystemTestMessage(t, first, 2, texts[0])
	assertClaudeSystemTestMessage(t, first, 3, texts[1])
}

func TestInjectClaudeCodeContextManagementRespectsThinkingAndCallerOwnership(t *testing.T) {
	for _, test := range []struct {
		name    string
		payload string
		want    bool
	}{
		{name: "omitted thinking", payload: `{"model":"claude-opus-5"}`, want: true},
		{name: "enabled thinking", payload: `{"thinking":{"type":"enabled"}}`, want: true},
		{name: "adaptive thinking", payload: `{"thinking":{"type":"adaptive"}}`, want: true},
		{name: "disabled thinking", payload: `{"thinking":{"type":"disabled"}}`},
		{name: "caller owned", payload: `{"context_management":{"edits":[]}}`},
	} {
		t.Run(
			test.name, func(t *testing.T) {
				input := []byte(test.payload)
				got, injected := injectClaudeCodeContextManagement(input)
				if injected != test.want {
					t.Fatalf("automatically injected = %t, want %t", injected, test.want)
				}
				if test.want {
					if raw := gjson.GetBytes(got, "context_management").Raw; raw != claudeCodeContextManagement {
						t.Fatalf("context_management = %s, want %s", raw, claudeCodeContextManagement)
					}
					return
				}
				if !bytes.Equal(got, input) {
					t.Fatalf("payload changed without automatic injection: %s", got)
				}
			},
		)
	}
}

func TestReconcileClaudeCodeContextManagement(t *testing.T) {
	withAutomatic := func(thinkingType string) string {
		return `{"thinking":{"type":"` + thinkingType + `"},"context_management":` + claudeCodeContextManagement + `}`
	}

	for _, test := range []struct {
		name    string
		payload string
		state   claudeCodeContextManagementState
		wantRaw string
	}{
		{
			name:    "removes unchanged automatic object when disabled",
			payload: withAutomatic("disabled"),
			state:   claudeCodeContextManagementState{eligible: true, automaticallyInjected: true},
		},
		{
			name:    "preserves caller owned object when disabled",
			payload: withAutomatic("disabled"),
			state:   claudeCodeContextManagementState{eligible: true, callerOwned: true, automaticallyInjected: true},
			wantRaw: claudeCodeContextManagement,
		},
		{
			name:    "preserves payload rule object when disabled",
			payload: withAutomatic("disabled"),
			state: claudeCodeContextManagementState{
				eligible: true, automaticallyInjected: true, payloadRuleTouched: true,
			},
			wantRaw: claudeCodeContextManagement,
		},
		{
			name:    "preserves changed automatic object when disabled",
			payload: `{"thinking":{"type":"disabled"},"context_management":{"edits":[{"type":"custom"}]}}`,
			state:   claudeCodeContextManagementState{eligible: true, automaticallyInjected: true},
			wantRaw: `{"edits":[{"type":"custom"}]}`,
		},
		{
			name:    "adds automatic object when enabled",
			payload: `{"thinking":{"type":"enabled"}}`,
			state:   claudeCodeContextManagementState{eligible: true},
			wantRaw: claudeCodeContextManagement,
		},
		{
			name:    "adds automatic object when adaptive",
			payload: `{"thinking":{"type":"adaptive"}}`,
			state:   claudeCodeContextManagementState{eligible: true},
			wantRaw: claudeCodeContextManagement,
		},
		{
			name:    "caller ownership prevents addition",
			payload: `{"thinking":{"type":"enabled"}}`,
			state:   claudeCodeContextManagementState{eligible: true, callerOwned: true},
		},
		{
			name:    "payload rule ownership prevents addition",
			payload: `{"thinking":{"type":"enabled"}}`,
			state:   claudeCodeContextManagementState{eligible: true, payloadRuleTouched: true},
		},
		{
			name:    "ineligible request prevents addition",
			payload: `{"thinking":{"type":"enabled"}}`,
		},
		{
			name:    "omitted thinking prevents addition",
			payload: `{}`,
			state:   claudeCodeContextManagementState{eligible: true},
		},
		{
			name:    "unknown thinking prevents addition",
			payload: `{"thinking":{"type":"unexpected"}}`,
			state:   claudeCodeContextManagementState{eligible: true},
		},
	} {
		t.Run(
			test.name, func(t *testing.T) {
				got := reconcileClaudeCodeContextManagement([]byte(test.payload), test.state)
				if raw := gjson.GetBytes(got, "context_management").Raw; raw != test.wantRaw {
					t.Fatalf("context_management = %s, want %s; body=%s", raw, test.wantRaw, got)
				}
			},
		)
	}
}

func TestApplyPayloadConfigWithRequestTrackedReportsContextManagementTouches(t *testing.T) {
	modelRules := []config.PayloadModelRule{{Name: "claude-opus-5", Protocol: "claude"}}
	originalWithoutContextManagement := []byte(`{"model":"claude-opus-5"}`)

	for _, test := range []struct {
		name          string
		payload       string
		original      []byte
		payloadConfig config.PayloadConfig
		wantTouched   bool
	}{
		{
			name:     "default",
			payload:  `{"model":"claude-opus-5"}`,
			original: originalWithoutContextManagement,
			payloadConfig: config.PayloadConfig{
				Default: []config.PayloadRule{
					{
						Models: modelRules,
						Params: map[string]any{"context_management": map[string]any{"edits": []any{map[string]any{"type": "default"}}}},
					},
				},
			},
			wantTouched: true,
		},
		{
			name:     "raw default",
			payload:  `{"model":"claude-opus-5"}`,
			original: originalWithoutContextManagement,
			payloadConfig: config.PayloadConfig{
				DefaultRaw: []config.PayloadRule{
					{
						Models: modelRules,
						Params: map[string]any{"context_management": `{"edits":[{"type":"raw_default"}]}`},
					},
				},
			},
			wantTouched: true,
		},
		{
			name:    "canonical descendant override",
			payload: `{"model":"claude-opus-5","context_management":` + claudeCodeContextManagement + `}`,
			payloadConfig: config.PayloadConfig{
				Override: []config.PayloadRule{
					{
						Models: modelRules,
						Params: map[string]any{"context_management.edits.0.keep": "all"},
					},
				},
			},
			wantTouched: true,
		},
		{
			name:    "identical raw override",
			payload: `{"model":"claude-opus-5","context_management":` + claudeCodeContextManagement + `}`,
			payloadConfig: config.PayloadConfig{
				OverrideRaw: []config.PayloadRule{
					{
						Models: modelRules,
						Params: map[string]any{"context_management": claudeCodeContextManagement},
					},
				},
			},
			wantTouched: true,
		},
		{
			name:    "filter already absent",
			payload: `{"model":"claude-opus-5"}`,
			payloadConfig: config.PayloadConfig{
				Filter: []config.PayloadFilterRule{
					{
						Models: modelRules,
						Params: []string{"context_management"},
					},
				},
			},
			wantTouched: true,
		},
		{
			name:    "unrelated override",
			payload: `{"model":"claude-opus-5"}`,
			payloadConfig: config.PayloadConfig{
				Override: []config.PayloadRule{
					{
						Models: modelRules,
						Params: map[string]any{"thinking.type": "enabled"},
					},
				},
			},
		},
		{
			name:    "nonmatching override",
			payload: `{"model":"claude-opus-5"}`,
			payloadConfig: config.PayloadConfig{
				Override: []config.PayloadRule{
					{
						Models: []config.PayloadModelRule{{Name: "other-model", Protocol: "claude"}},
						Params: map[string]any{"context_management": map[string]any{"edits": []any{}}},
					},
				},
			},
		},
		{
			name:     "default skipped for caller owned field",
			payload:  `{"model":"claude-opus-5","context_management":{"edits":[{"type":"caller"}]}}`,
			original: []byte(`{"model":"claude-opus-5","context_management":{"edits":[{"type":"caller"}]}}`),
			payloadConfig: config.PayloadConfig{
				Default: []config.PayloadRule{
					{
						Models: modelRules,
						Params: map[string]any{"context_management": map[string]any{"edits": []any{map[string]any{"type": "default"}}}},
					},
				},
			},
		},
	} {
		t.Run(
			test.name, func(t *testing.T) {
				cfg := &config.Config{Payload: test.payloadConfig}
				_, touched := applyPayloadConfigWithRequestTracked(
					cfg,
					"claude-opus-5",
					"claude",
					"claude",
					"",
					[]byte(test.payload),
					test.original,
					"claude-opus-5",
					"",
					nil,
					"context_management",
				)
				if touched != test.wantTouched {
					t.Fatalf("context_management touched = %t, want %t", touched, test.wantTouched)
				}
			},
		)
	}
}

func TestPrepareClaudeRequestReconcilesContextManagement(t *testing.T) {
	const basePayload = `{"model":"claude-opus-5","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`
	modelRules := []config.PayloadModelRule{{Name: "claude-opus-5", Protocol: "claude"}}

	for _, test := range []struct {
		name       string
		cfg        *config.Config
		payload    string
		wantRaw    string
		wantType   string
		wantAbsent bool
	}{
		{
			name:       "caller disables thinking",
			cfg:        &config.Config{},
			payload:    `{"model":"claude-opus-5","max_tokens":16,"messages":[{"role":"user","content":"hi"}],"thinking":{"type":"disabled"}}`,
			wantType:   "disabled",
			wantAbsent: true,
		},
		{
			name: "payload override disables thinking",
			cfg: &config.Config{
				Payload: config.PayloadConfig{
					Override: []config.PayloadRule{
						{
							Models: modelRules,
							Params: map[string]any{"thinking.type": "disabled"},
						},
					},
				},
			},
			payload:    basePayload,
			wantType:   "disabled",
			wantAbsent: true,
		},
		{
			name: "payload override enables thinking",
			cfg: &config.Config{
				Payload: config.PayloadConfig{
					Override: []config.PayloadRule{
						{
							Models: modelRules,
							Params: map[string]any{"thinking.type": "enabled"},
						},
					},
				},
			},
			payload:  `{"model":"claude-opus-5","max_tokens":16,"messages":[{"role":"user","content":"hi"}],"thinking":{"type":"disabled"}}`,
			wantRaw:  claudeCodeContextManagement,
			wantType: "enabled",
		},
		{
			name:     "caller context management survives disabled thinking",
			cfg:      &config.Config{},
			payload:  `{"model":"claude-opus-5","max_tokens":16,"messages":[{"role":"user","content":"hi"}],"thinking":{"type":"disabled"},"context_management":{"edits":[{"type":"caller"}]}}`,
			wantRaw:  `{"edits":[{"type":"caller"}]}`,
			wantType: "disabled",
		},
		{
			name: "payload rule context management survives disabled thinking",
			cfg: &config.Config{
				Payload: config.PayloadConfig{
					Override: []config.PayloadRule{
						{
							Models: modelRules,
							Params: map[string]any{
								"thinking.type":      "disabled",
								"context_management": map[string]any{"edits": []any{map[string]any{"type": "payload_rule"}}},
							},
						},
					},
				},
			},
			payload:  basePayload,
			wantRaw:  `{"edits":[{"type":"payload_rule"}]}`,
			wantType: "disabled",
		},
		{
			name: "exact automatic value remains payload rule owned",
			cfg: &config.Config{
				Payload: config.PayloadConfig{
					Override: []config.PayloadRule{
						{
							Models: modelRules,
							Params: map[string]any{"thinking.type": "disabled"},
						},
					},
					OverrideRaw: []config.PayloadRule{
						{
							Models: modelRules,
							Params: map[string]any{"context_management": claudeCodeContextManagement},
						},
					},
				},
			},
			payload:  basePayload,
			wantRaw:  claudeCodeContextManagement,
			wantType: "disabled",
		},
		{
			name: "context management filter remains authoritative",
			cfg: &config.Config{
				Payload: config.PayloadConfig{
					Override: []config.PayloadRule{
						{
							Models: modelRules,
							Params: map[string]any{"thinking.type": "enabled"},
						},
					},
					Filter: []config.PayloadFilterRule{
						{
							Models: modelRules,
							Params: []string{"context_management"},
						},
					},
				},
			},
			payload:    `{"model":"claude-opus-5","max_tokens":16,"messages":[{"role":"user","content":"hi"}],"thinking":{"type":"disabled"}}`,
			wantType:   "enabled",
			wantAbsent: true,
		},
		{
			name:     "forced tool choice retains automatic context management",
			cfg:      &config.Config{},
			payload:  `{"model":"claude-opus-5","max_tokens":16,"messages":[{"role":"user","content":"hi"}],"thinking":{"type":"adaptive"},"tool_choice":{"type":"any"}}`,
			wantRaw:  claudeCodeContextManagement,
			wantType: "",
		},
	} {
		for _, forceStream := range []bool{false, true} {
			name := test.name + "/non-stream"
			if forceStream {
				name = test.name + "/stream"
			}
			t.Run(
				name, func(t *testing.T) {
					body := prepareClaudeContextManagementBody(t, test.cfg, []byte(test.payload), forceStream)
					contextManagement := gjson.GetBytes(body, "context_management")
					if test.wantAbsent {
						if contextManagement.Exists() {
							t.Fatalf("context_management = %s, want absent; body=%s", contextManagement.Raw, body)
						}
					} else if contextManagement.Raw != test.wantRaw {
						t.Fatalf("context_management = %s, want %s; body=%s", contextManagement.Raw, test.wantRaw, body)
					}
					if got := gjson.GetBytes(body, "thinking.type").String(); got != test.wantType {
						t.Fatalf("thinking.type = %q, want %q; body=%s", got, test.wantType, body)
					}
				},
			)
		}
	}
}

func prepareClaudeContextManagementBody(
	t *testing.T,
	cfg *config.Config,
	payload []byte,
	forceStream bool,
) []byte {
	t.Helper()
	request, errPrepare := NewClaudeExecutor(cfg).prepareClaudeRequest(
		context.Background(),
		&cliproxyauth.Auth{
			Attributes: map[string]string{
				"api_key":  "context-management-test-key",
				"base_url": "https://api.anthropic.com",
			},
		},
		cliproxyexecutor.Request{Model: "claude-opus-5", Payload: payload},
		cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatClaude},
		forceStream,
	)
	if errPrepare != nil {
		t.Fatalf("prepareClaudeRequest() error = %v", errPrepare)
	}
	body, errRead := io.ReadAll(request.httpReq.Body)
	if errRead != nil {
		t.Fatalf("read prepared request body: %v", errRead)
	}
	return body
}
