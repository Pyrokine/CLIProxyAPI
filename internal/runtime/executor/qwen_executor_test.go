package executor

import (
	"net/http"
	"testing"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/thinking"
	"github.com/tidwall/gjson"
)

func TestQwenExecutorParseSuffix(t *testing.T) {
	tests := []struct {
		name      string
		model     string
		wantBase  string
		wantLevel string
	}{
		{"no suffix", "qwen-max", "qwen-max", ""},
		{"with level suffix", "qwen-max(high)", "qwen-max", "high"},
		{"with budget suffix", "qwen-max(16384)", "qwen-max", "16384"},
		{"complex model name", "qwen-plus-latest(medium)", "qwen-plus-latest", "medium"},
	}

	for _, tt := range tests {
		t.Run(
			tt.name, func(t *testing.T) {
				result := thinking.ParseSuffix(tt.model)
				if result.ModelName != tt.wantBase {
					t.Errorf("ParseSuffix(%q).ModelName = %q, want %q", tt.model, result.ModelName, tt.wantBase)
				}
			},
		)
	}
}

func TestEnsureQwenSystemMessage_MergesStringSystem(t *testing.T) {
	payload := []byte(`{
		"model": "qwen3-coder-plus",
		"messages": [
			{"role": "system", "content": "project rule"},
			{"role": "user", "content": "hi"}
		]
	}`)

	out, err := ensureQwenSystemMessage(payload)
	if err != nil {
		t.Fatalf("ensureQwenSystemMessage() error = %v", err)
	}

	messages := gjson.GetBytes(out, "messages").Array()
	if len(messages) != 2 {
		t.Fatalf("messages length = %d, want 2, raw = %s", len(messages), gjson.GetBytes(out, "messages").Raw)
	}
	if got := messages[0].Get("role").String(); got != "system" {
		t.Fatalf("messages.0.role = %q, want system", got)
	}
	parts := messages[0].Get("content").Array()
	if len(parts) != 2 {
		t.Fatalf("messages.0.content length = %d, want 2, raw = %s", len(parts), messages[0].Get("content").Raw)
	}
	if got := parts[0].Get("cache_control.type").String(); got != "ephemeral" {
		t.Fatalf("messages.0.content.0.cache_control.type = %q, want ephemeral", got)
	}
	if got := parts[1].Get("text").String(); got != "project rule" {
		t.Fatalf("messages.0.content.1.text = %q, want project rule", got)
	}
	if got := messages[1].Get("role").String(); got != "user" {
		t.Fatalf("messages.1.role = %q, want user", got)
	}
}

func TestEnsureQwenSystemMessage_DedupesInjectedSystemPart(t *testing.T) {
	payload := []byte(`{
		"messages": [
			{"role": "system", "content": [{"type":"text","text":"","cache_control":{"type":"ephemeral"}}]},
			{"role": "user", "content": "hi"}
		]
	}`)

	out, err := ensureQwenSystemMessage(payload)
	if err != nil {
		t.Fatalf("ensureQwenSystemMessage() error = %v", err)
	}

	parts := gjson.GetBytes(out, "messages.0.content").Array()
	if len(parts) != 1 {
		t.Fatalf(
			"messages.0.content length = %d, want 1, raw = %s", len(parts),
			gjson.GetBytes(out, "messages.0.content").Raw,
		)
	}
	if got := gjson.GetBytes(out, "messages.1.role").String(); got != "user" {
		t.Fatalf("messages.1.role = %q, want user", got)
	}
}

func TestApplyQwenHeadersUsesCurrentCliIdentity(t *testing.T) {
	req, err := http.NewRequest(http.MethodPost, "https://portal.qwen.ai/v1/chat/completions", nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}

	applyQwenHeaders(req, "token", true)

	if got := req.Header.Get("User-Agent"); got != "QwenCode/0.14.2 (darwin; arm64)" {
		t.Fatalf("User-Agent = %q, want current Qwen Code identity", got)
	}
	if got := req.Header.Get("X-Dashscope-Useragent"); got != "QwenCode/0.14.2 (darwin; arm64)" {
		t.Fatalf("X-Dashscope-Useragent = %q, want current Qwen Code identity", got)
	}
	if got := req.Header.Get("Accept-Language"); got != "*" {
		t.Fatalf("Accept-Language = %q, want *", got)
	}
	if got := req.Header.Get("Accept-Encoding"); got != "gzip, deflate" {
		t.Fatalf("Accept-Encoding = %q, want gzip, deflate", got)
	}
	if got := req.Header.Get("Accept"); got != "text/event-stream" {
		t.Fatalf("Accept = %q, want text/event-stream", got)
	}
}
