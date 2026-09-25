package multiagentv2

import (
	"context"
	"fmt"
	"net/http"
	"strings"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/util"
	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

const (
	codexAppNamespace            = "codex_app"
	codexCreateThreadName        = "create_thread"
	codexSendMessageToThreadName = "send_message_to_thread"
	codexAppCreateThreadTool     = "codex_app__create_thread"
	codexAppSendMessageTool      = "codex_app__send_message_to_thread"
	codexOpenAISubagentHeader    = "X-Openai-Subagent"
	codexCollabSpawnSubagent     = "collab_spawn"
)

// RewriteCodexOrphanDelegationInput converts unpaired Codex delegation outputs
// into user messages for collab_spawn requests.
func RewriteCodexOrphanDelegationInput(
	ctx context.Context,
	headers http.Header,
	payload []byte,
	enabled bool,
) []byte {
	if !enabled || len(payload) == 0 || !isCodexCollabSpawnSubagent(ctx, headers) {
		return payload
	}
	input := gjson.GetBytes(payload, "input")
	if !input.IsArray() {
		return payload
	}

	items := input.Array()
	availableCalls := make(map[string]int)
	for _, item := range items {
		if item.Get("type").String() != "function_call" {
			continue
		}
		callID := item.Get("call_id").String()
		if strings.TrimSpace(callID) != "" {
			availableCalls[callID]++
		}
	}

	updated := payload
	for index, item := range items {
		if item.Get("type").String() != "function_call_output" {
			continue
		}
		callID := item.Get("call_id").String()
		if strings.TrimSpace(callID) != "" && availableCalls[callID] > 0 {
			availableCalls[callID]--
			continue
		}
		toolLabel, target := codexDelegationToolLabel(item)
		if !target {
			continue
		}
		var errSet error
		updated, errSet = sjson.SetRawBytes(
			updated,
			fmt.Sprintf("input.%d", index),
			buildCodexOrphanUserMessage(toolLabel, item.Get("output")),
		)
		if errSet != nil {
			return payload
		}
	}
	return updated
}

// RewriteCodexOrphanDelegationInputForConfig applies the independent orphan
// delegation compatibility flag.
func RewriteCodexOrphanDelegationInputForConfig(
	ctx context.Context,
	headers http.Header,
	payload []byte,
	cfg *config.Config,
) []byte {
	return RewriteCodexOrphanDelegationInput(
		ctx,
		headers,
		payload,
		cfg != nil && cfg.Codex.OrphanDelegationCompatibility,
	)
}

func isCodexCollabSpawnSubagent(ctx context.Context, headers http.Header) bool {
	if ctx != nil {
		if ginCtx, ok := util.GinContextValue(ctx).(*gin.Context); ok && ginCtx != nil && ginCtx.Request != nil {
			if value := headerValueCaseInsensitive(ginCtx.Request.Header, codexOpenAISubagentHeader); value != "" {
				return strings.EqualFold(value, codexCollabSpawnSubagent)
			}
		}
	}
	return strings.EqualFold(headerValueCaseInsensitive(headers, codexOpenAISubagentHeader), codexCollabSpawnSubagent)
}

func codexDelegationToolLabel(item gjson.Result) (string, bool) {
	if item.Get("namespace").String() != codexAppNamespace {
		return "", false
	}
	switch item.Get("name").String() {
	case codexCreateThreadName:
		return codexAppCreateThreadTool, true
	case codexSendMessageToThreadName:
		return codexAppSendMessageTool, true
	default:
		return "", false
	}
}

func buildCodexOrphanUserMessage(toolLabel string, output gjson.Result) []byte {
	outputText := ""
	if output.Exists() {
		if output.Type == gjson.String {
			outputText = output.String()
		} else {
			outputText = output.Raw
		}
	}
	message := []byte(`{"type":"message","role":"user","content":[{"type":"input_text","text":""}]}`)
	message, _ = sjson.SetBytes(message, "content.0.text", fmt.Sprintf("Tool output from %s:\n%s", toolLabel, outputText))
	return message
}
