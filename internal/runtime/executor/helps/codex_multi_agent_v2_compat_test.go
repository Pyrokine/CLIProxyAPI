package helps

import (
	"context"
	"net/http"
	"testing"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	"github.com/tidwall/gjson"
)

func TestOptimizeCodexMultiAgentV2RequestRewritesAgentMessageForCompatModel(t *testing.T) {
	payload := []byte(`{"input":[{"type":"agent_message","content":[{"type":"encrypted_content","encrypted_content":"task"}]}]}`)
	headers := http.Header{"User-Agent": []string{"codex-tui/0.145.0"}}
	cfg := &config.Config{Codex: config.CodexConfig{OptimizeMultiAgentV2: true}}

	withoutCompat, _ := OptimizeCodexMultiAgentV2Request(
		context.Background(),
		headers,
		payload,
		cfg,
		false,
	)
	if got := gjson.GetBytes(withoutCompat, "input.0.type").String(); got != "agent_message" {
		t.Fatalf("non-compat input type = %q, want agent_message; output=%s", got, withoutCompat)
	}

	withCompat, _ := OptimizeCodexMultiAgentV2Request(
		context.Background(),
		headers,
		payload,
		cfg,
		true,
	)
	if got := gjson.GetBytes(withCompat, "input.0.type").String(); got != "message" {
		t.Fatalf("compat input type = %q, want message; output=%s", got, withCompat)
	}
	if got := gjson.GetBytes(withCompat, "input.0.content.0.text").String(); got != "task" {
		t.Fatalf("compat input text = %q, want task; output=%s", got, withCompat)
	}
}
