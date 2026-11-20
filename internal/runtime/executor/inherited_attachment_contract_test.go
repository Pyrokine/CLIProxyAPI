package executor

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	translatorcommon "github.com/Pyrokine/CLIProxyAPI/v6/internal/translator/common"
	coreauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
	coreexecutor "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
	sdktranslator "github.com/Pyrokine/CLIProxyAPI/v6/sdk/translator"
)

func TestInheritedAttachmentErrorsStopEveryNativeExecutionEntry(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(
		http.HandlerFunc(
			func(w http.ResponseWriter, _ *http.Request) {
				calls.Add(1)
				w.WriteHeader(http.StatusInternalServerError)
			},
		),
	)
	defer server.Close()
	cfg := &config.Config{}
	const claudeFile = `{"model":"m","max_tokens":8,"messages":[{"role":"user","content":[{"ty` +
		`pe":"container_upload","file_id":"file_not_stored"}]}]}`
	const chatFile = `{"model":"m","messages":[{"role":"user","content":[{"type":"file",` +
		`"file":{"file_id":"file-not-stored"}}]}]}`
	for _, sample := range []struct {
		provider string
		executor coreauth.ProviderExecutor
		format   sdktranslator.Format
		payload  string
		part     string
	}{
		{
			"openai-compatibility", NewOpenAICompatExecutor("openai-compatibility", cfg), sdktranslator.FormatClaude,
			claudeFile, "container_upload",
		},
		{"codex", NewCodexAutoExecutor(cfg), sdktranslator.FormatClaude, claudeFile, "container_upload"},
		{"claude", NewClaudeExecutor(cfg), sdktranslator.FormatOpenAI, chatFile, "file"},
		{"gemini", NewGeminiExecutor(cfg), sdktranslator.FormatOpenAI, chatFile, "file"},
		{"antigravity", NewAntigravityExecutor(cfg), sdktranslator.FormatClaude, claudeFile, "container_upload"},
	} {
		for _, entry := range []string{"Execute", "ExecuteStream", "CountTokens"} {
			t.Run(
				sample.provider+"/"+entry, func(t *testing.T) {
					auth := &coreauth.Auth{
						ID: t.Name(), Provider: sample.provider, Status: coreauth.StatusActive, ProxyURL: "direct",
						Attributes: map[string]string{
							"api_key": "synthetic-key", "base_url": server.URL,
							"compat_name": "synthetic-channel", "provider_key": "synthetic-channel", "plan_type": "pro",
						},
						Metadata: map[string]any{
							"access_token": "synthetic-token", "project_id": "synthetic-project",
							"expired": time.Now().Add(time.Hour).Format(time.RFC3339),
						},
					}
					request := coreexecutor.Request{Model: "m", Payload: []byte(sample.payload)}
					opts := coreexecutor.Options{SourceFormat: sample.format, Stream: entry == "ExecuteStream"}
					var err error
					switch entry {
					case "Execute":
						_, err = sample.executor.Execute(t.Context(), auth, request, opts)
					case "ExecuteStream":
						_, err = sample.executor.ExecuteStream(t.Context(), auth, request, opts)
					case "CountTokens":
						_, err = sample.executor.CountTokens(t.Context(), auth, request, opts)
					}
					var part *translatorcommon.UnsupportedPartError
					if !errors.As(
						err, &part,
					) || part.Type != sample.part || part.StatusCode() != 400 || !part.IsRequestScoped() {
						t.Fatalf("attachment error=%v, want request-scoped unsupported %s", err, sample.part)
					}
					if got := calls.Load(); got != 0 {
						t.Fatalf("rejected attachment reached upstream %d times", got)
					}
				},
			)
		}
	}
}
