package executor

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/cache"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/logging"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/signature"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/util"
	core "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/translator"
	log "github.com/sirupsen/logrus"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
	"google.golang.org/protobuf/encoding/protowire"
)

func TestAntigravityOriginalCachedThinkingReachesEveryEntry(t *testing.T) {
	const model = "claude-sonnet-4-5-thinking"
	channel := protowire.AppendTag(nil, 1, protowire.VarintType)
	channel = protowire.AppendVarint(channel, 12)
	channel = protowire.AppendTag(channel, 6, protowire.BytesType)
	channel = protowire.AppendBytes(channel, []byte("claude-sonnet-4-5-thinking-test-signature-payload"))
	container := protowire.AppendBytes(protowire.AppendTag(nil, 1, protowire.BytesType), channel)
	sealed := protowire.AppendBytes(protowire.AppendTag(nil, 2, protowire.BytesType), container)
	cached := base64.StdEncoding.EncodeToString(sealed)
	want, compatible := signature.CompatibleAntigravityClaudeThinkingSignature(cached)
	if !compatible {
		t.Fatal("original cached signature fixture is incompatible")
	}
	for _, mode := range []string{"execute", "stream", "count"} {
		t.Run(
			mode, func(t *testing.T) {
				thinkingText := "original cached thinking for " + t.Name()
				if !cache.CacheSignatureBestEffort(t.Context(), model, thinkingText, cached) {
					t.Fatal("original fixed cache entry was not stored")
				}
				payload := []byte(
					`{"model":"` + model + `","max_tokens":4096,"messages":[{"role":"assistant",` +
						`"content":[{"type":"thinking","thinking":"` +
						thinkingText +
						`"},{"type":"text","text":"answer"}]},{"role":"user",` +
						`"content":"next"}]}`,
				)
				wire := antigravityCachedThinkingWire(t, model, payload, mode)
				found := false
				for _, content := range gjson.GetBytes(wire, "request.contents").Array() {
					for _, part := range content.Get("parts").Array() {
						if part.Get("thought").Bool() && part.Get("text").String() == thinkingText &&
							part.Get("thoughtSignature").String() == want {
							found = true
						}
					}
				}
				if !found {
					t.Fatalf("cached thinking did not reach the final wire: %s", wire)
				}
			},
		)
	}
}

func antigravityCachedThinkingWire(t *testing.T, model string, payload []byte, mode string) []byte {
	t.Helper()
	before := bytes.Clone(payload)
	calls := 0
	var wire []byte
	ctx := util.WithRoundTripper(
		t.Context(), claudeContractTransport(
			func(request *http.Request) (*http.Response, error) {
				calls++
				var err error
				wire, err = io.ReadAll(request.Body)
				if err != nil {
					return nil, err
				}
				body := `{"response":{"candidates":[{"content":{"role":"model","parts":[{"text":"ok"}]},` +
					`"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":1,` +
					`"candidatesTokenCount":1,"totalTokenCount":2}}}`
				contentType := "application/json"
				if mode == "stream" {
					body, contentType = "data: "+body+"\n\n", "text/event-stream"
				} else if mode == "count" {
					body = `{"totalTokens":7}`
				}
				return &http.Response{
					StatusCode: http.StatusOK,
					Header:     http.Header{"Content-Type": {contentType}},
					Body:       io.NopCloser(strings.NewReader(body)),
					Request:    request,
				}, nil
			},
		),
	)
	request := core.Request{Model: model, Payload: payload}
	opts := core.Options{
		SourceFormat:   translator.FormatClaude,
		ResponseFormat: translator.FormatClaude,
		Stream:         mode == "stream",
	}
	execute := NewAntigravityExecutor(&config.Config{})
	credential := antigravityCountTokensAuth("http://127.0.0.1:1", t.Name())
	var err error
	switch mode {
	case "execute":
		_, err = execute.Execute(ctx, credential, request, opts)
	case "count":
		_, err = execute.CountTokens(ctx, credential, request, opts)
	case "stream":
		var result *core.StreamResult
		result, err = execute.ExecuteStream(ctx, credential, request, opts)
		if err == nil {
			for chunk := range result.Chunks {
				if chunk.Acknowledge != nil {
					chunk.Acknowledge(nil)
				}
				if chunk.Err != nil {
					err = chunk.Err
				}
			}
		}
	}
	if err != nil || calls != 1 {
		t.Fatalf("cached-thinking entry error=%v calls=%d", err, calls)
	}
	if !bytes.Equal(request.Payload, before) {
		t.Fatal("source request was mutated")
	}
	return wire
}

func TestAntigravityCAQSCacheFinalConsumer(t *testing.T) {
	const model = "claude-opus-5-5-high"
	sig := syntheticAntigravityCAQS()
	for _, prefix := range []string{"", "claude#"} {
		for _, stream := range []bool{false, true} {
			t.Run(
				fmt.Sprintf("prefix=%q/stream=%t", prefix, stream), func(t *testing.T) {
					cache.ClearSignatureCache(model)
					t.Cleanup(func() { cache.ClearSignatureCache(model) })
					text := "continuation164-cache-" + t.Name()
					cache.StoreSignature(model, text, prefix+sig)
					body := []byte(`{"max_tokens":4096,"thinking":{"type":"enabled","budget_tokens":1024},` +
						`"messages":[{"role":"assistant","content":[{"type":"thinking","thinking":""},` +
						`{"type":"text","text":"previous answer"}]},{"role":"user",` +
						`"content":"continue"}]}`)
					body, err := sjson.SetBytes(body, "messages.0.content.0.thinking", text)
					if err != nil {
						t.Fatal(err)
					}
					mode := "execute"
					if stream {
						mode = "stream"
					}
					upstream := antigravityCachedThinkingWire(t, model, body, mode)
					found := false
					for _, content := range gjson.GetBytes(upstream, "request.contents").Array() {
						for _, part := range content.Get("parts").Array() {
							if part.Get("thought").Bool() && part.Get("text").String() == text &&
								part.Get("thoughtSignature").String() == sig {
								found = true
							}
						}
					}
					if !found {
						t.Fatalf("cached CAQS was not recovered unchanged in actual request: %s", upstream)
					}
				},
			)
		}
	}
}

func TestAntigravityCAQSCachePrevalidation(t *testing.T) {
	const model = "claude-opus-5-5-high"
	const text = "continuation164-stage-cache-text"
	sig := syntheticAntigravityCAQS()
	cache.ClearSignatureCache(model)
	t.Cleanup(func() { cache.ClearSignatureCache(model) })
	cache.StoreSignature(model, text, "claude#"+sig)
	if got := cache.GetCachedSignature(model, text); got != "claude#"+sig {
		t.Fatalf("stored signature=%q", got)
	}
	body := []byte(`{"messages":[{"role":"assistant","content":[{"type":"thinking",` +
		`"thinking":"continuation164-stage-cache-text"},{"type":"text",` +
		`"text":"visible"}]},{"role":"user","content":"continue"}]}`)
	before, err := translator.TranslateRequestChecked(
		t.Context(),
		translator.FormatClaude,
		translator.FormatAntigravity,
		model,
		body,
		false,
	)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(before, []byte(sig)) {
		t.Fatalf("direct registered converter did not recover cached signature: %s", before)
	}
	validated, err := validateAntigravityRequestSignatures(t.Context(), model, translator.FormatClaude, body)
	if err != nil {
		t.Fatal(err)
	}
	after, err := translator.TranslateRequestChecked(
		t.Context(),
		translator.FormatClaude,
		translator.FormatAntigravity,
		model,
		validated,
		false,
	)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(after, []byte(sig)) {
		t.Fatalf("executor prevalidation removed supported cache recovery: %s", validated)
	}
}

func TestAntigravitySignatureSanitizeKeepsSafeRequestDiagnostics(t *testing.T) {
	for _, model := range []string{"claude-sonnet-4-5-thinking", "gemini-2.5-pro"} {
		for _, mode := range []string{"invalid", "missing", "unchanged", "info-level", "other-source"} {
			t.Run(
				model+"/"+mode, func(t *testing.T) {
					logger := log.StandardLogger()
					oldOutput, oldFormatter, oldLevel := logger.Out, logger.Formatter, logger.GetLevel()
					var output bytes.Buffer
					logger.SetOutput(&output)
					logger.SetFormatter(&log.JSONFormatter{DisableTimestamp: true})
					logger.SetLevel(log.DebugLevel)
					if mode == "info-level" {
						logger.SetLevel(log.InfoLevel)
					}
					t.Cleanup(
						func() {
							logger.SetOutput(oldOutput)
							logger.SetFormatter(oldFormatter)
							logger.SetLevel(oldLevel)
						},
					)
					signatureValue := "synthetic-private-signature"
					if mode == "missing" {
						signatureValue = ""
					}
					content := `{"type":"thinking","thinking":"private reasoning text",` +
						`"signature":"` + signatureValue + `"},`
					if mode == "unchanged" {
						content = ""
					}
					payload := []byte(
						`{"api_key":"synthetic-private-token",` +
							(`"messages":[{"role":"assistant","content":` +
								`[`) +
							content +
							(`{"type":"text","text":` +
								`"retained content"}]}]` +
								`}`),
					)
					before := bytes.Clone(payload)
					from := translator.FormatClaude
					if mode == "other-source" {
						from = translator.FormatOpenAI
					}
					ctx := logging.WithRequestID(t.Context(), "synthetic-antigravity-request-id")
					got, err := validateAntigravityRequestSignatures(ctx, model, from, payload)
					if err != nil {
						t.Fatal(err)
					}
					if !bytes.Equal(payload, before) || !bytes.Contains(got, []byte("retained content")) {
						t.Fatal("signature cleanup changed the caller payload or removed text")
					}
					cachedBypass := mode == "missing" && model == "gemini-2.5-pro"
					if cachedBypass &&
						gjson.GetBytes(
							got, "messages.0.content.0.signature",
						).String() != signature.GeminiSkipThoughtSignatureValidator {
						t.Fatalf("Gemini cache-miss signature changed: %s", got)
					}
					if mode != "other-source" && !cachedBypass &&
						bytes.Contains(got, []byte(`"type":"thinking"`)) {
						t.Fatalf("incompatible thinking remains: %s", got)
					}
					var entries []map[string]any
					for line := range bytes.SplitSeq(bytes.TrimSpace(output.Bytes()), []byte("\n")) {
						if gjson.GetBytes(line, "component").String() != "signature_sanitizer" {
							continue
						}
						var entry map[string]any
						if err := json.Unmarshal(line, &entry); err != nil {
							t.Fatal(err)
						}
						entries = append(entries, entry)
					}
					if mode == "unchanged" || mode == "info-level" || mode == "other-source" || cachedBypass {
						if len(entries) != 0 {
							t.Fatalf("unexpected diagnostics: %s", output.Bytes())
						}
						return
					}
					if len(entries) != 1 {
						t.Fatalf("want one cleanup summary, got %d: %s", len(entries), output.Bytes())
					}
					target, stage, reason := "claude", "prefix_cleanup", "empty_or_non_claude_signature"
					if model == "gemini-2.5-pro" {
						target, stage, reason = "gemini", "provider_cleanup", "empty_or_non_gemini_signature"
					}
					for key, want := range map[string]any{
						"request_id": "synthetic-antigravity-request-id", "executor": "antigravity",
						"target_provider": target, "target_model": model, "stage": stage, "reason": reason,
						"action": "drop_thinking_blocks", "count": float64(1),
						"before_thinking_blocks": float64(1), "after_thinking_blocks": float64(0),
					} {
						if entries[0][key] != want {
							t.Fatalf("%s=%v, want %v: %s", key, entries[0][key], want, output.Bytes())
						}
					}
					for _, private := range []string{
						"synthetic-private-signature", "private reasoning text", "retained content",
						"synthetic-private-token",
					} {
						if strings.Contains(output.String(), private) {
							t.Fatalf("private request content appeared in diagnostics: %q", private)
						}
					}
				},
			)
		}
	}
}
