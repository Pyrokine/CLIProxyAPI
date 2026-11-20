package executor

import (
	"io"
	"net/http"
	"regexp"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	claudeauth "github.com/Pyrokine/CLIProxyAPI/v6/internal/auth/claude"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/cache"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/runtime/executor/helps"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/util"
	cliproxyauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/usage"
	sdktranslator "github.com/Pyrokine/CLIProxyAPI/v6/sdk/translator"
	"github.com/tidwall/gjson"
)

var claudeDateReminderPattern = regexp.MustCompile(`Today's date is (\d{4}-\d{2}-\d{2})`)

func extractClaudeCloakDate(t *testing.T, payload []byte) string {
	t.Helper()
	content := gjson.GetBytes(payload, "messages.0.content")
	if content.IsArray() {
		for _, block := range content.Array() {
			if match := claudeDateReminderPattern.FindStringSubmatch(block.Get("text").String()); match != nil {
				return match[1]
			}
		}
	} else if match := claudeDateReminderPattern.FindStringSubmatch(content.String()); match != nil {
		return match[1]
	}
	t.Fatalf("no currentDate reminder found in first user message: %s", payload)
	return ""
}

func executeClaudeDatePinRequest(
	t *testing.T, executor *ClaudeExecutor, auth *cliproxyauth.Auth,
	payload []byte, session string, stream bool,
) []byte {
	t.Helper()
	var sent []byte
	calls := 0
	manager := usage.NewManager(1)
	defer manager.Stop()
	ctx := util.WithRoundTripper(
		usage.WithManager(t.Context(), manager),
		claudeContractTransport(
			func(request *http.Request) (*http.Response, error) {
				calls++
				body, err := io.ReadAll(request.Body)
				if err != nil {
					return nil, err
				}
				sent = body
				return claudeContractResponse(request, body), nil
			},
		),
	)
	operation := "messages"
	if stream {
		operation = "stream"
	}
	executeClaudeContractRequest(
		t, executor, ctx, auth,
		cliproxyexecutor.Request{Model: "claude-opus-5", Payload: payload},
		cliproxyexecutor.Options{
			SourceFormat: sdktranslator.FormatClaude,
			Headers:      http.Header{"X-Session-ID": {session}},
		}, operation,
	)
	if calls != 1 {
		t.Fatalf("date pin request made %d calls, want 1", calls)
	}
	return sent
}

func TestClaudeCloakedDateReminderPinnedToSessionAcrossMidnight(t *testing.T) {
	cache.StoreSignature(t.Name(), t.Name(), strings.Repeat("s", 64))
	t.Cleanup(func() { cache.ClearSignatureCache(t.Name()) })
	helps.ResetClaudeDiagnosticsForTest()
	t.Cleanup(helps.ResetClaudeDiagnosticsForTest)
	synctest.Test(
		t, func(t *testing.T) {
			before := time.Date(2026, 8, 1, 23, 59, 0, 0, time.FixedZone("Asia/Shanghai", 8*60*60))
			time.Sleep(time.Until(before))
			auth := claudeNativeHelperOAuthAuth("https://api.anthropic.com")
			auth.ID = "test-date-pin-cred"
			auth.Attributes["cloak_mode"] = "always"
			auth.Attributes["timezone"] = "Asia/Shanghai"
			executor := NewClaudeExecutor(&config.Config{})
			payload1 := []byte(`{"model":"claude-opus-5","messages":[{"role":"user","content":"turn one"}]}`)
			first := executeClaudeDatePinRequest(t, executor, auth, payload1, "sess-midnight", false)
			if got := extractClaudeCloakDate(t, first); got != "2026-08-01" {
				t.Fatalf("first request date = %q, want 2026-08-01", got)
			}
			time.Sleep(2 * time.Minute)
			payload2 := []byte(`{"model":"claude-opus-5","messages":[` +
				`{"role":"user","content":"turn one"},` +
				`{"role":"assistant","content":"ok"},` +
				`{"role":"user","content":"turn two"}]}`)
			second := executeClaudeDatePinRequest(t, executor, auth, payload2, "sess-midnight", false)
			if got := extractClaudeCloakDate(t, second); got != "2026-08-01" {
				t.Fatalf("date reminder changed across midnight: %q", got)
			}
		},
	)
}

func TestClaudeCloakedDateReminderSessionByteStability(t *testing.T) {
	cache.StoreSignature(t.Name(), t.Name(), strings.Repeat("s", 64))
	t.Cleanup(func() { cache.ClearSignatureCache(t.Name()) })
	helps.ResetClaudeDiagnosticsForTest()
	t.Cleanup(helps.ResetClaudeDiagnosticsForTest)
	synctest.Test(
		t, func(t *testing.T) {
			before := time.Date(2026, 8, 1, 23, 59, 0, 0, time.FixedZone("Asia/Shanghai", 8*60*60))
			time.Sleep(time.Until(before))
			auth := claudeNativeHelperOAuthAuth("https://api.anthropic.com")
			auth.ID = "test-date-pin-exec-oauth"
			auth.Attributes["api_key"] = "sk-ant-oat-test-oauth-key-date-pin"
			auth.Metadata[claudeauth.DeviceIDsMetadataKey] = []string{
				"00000000000000000000000000000000000000000000000000000000000000d1",
			}
			auth.Attributes["cloak_mode"] = "always"
			auth.Attributes["timezone"] = "Asia/Shanghai"
			executor := NewClaudeExecutor(&config.Config{})
			payload := []byte(`{
			"model":"claude-opus-5",
			"system":[{"type":"text","text":"You are an expert software engineer.",
				"cache_control":{"type":"ephemeral"}}],
			"messages":[{"role":"user","content":"Say OK."}],
			"max_tokens":100
		}`)
			first := executeClaudeDatePinRequest(t, executor, auth, payload, "date-pin-session-1", false)
			time.Sleep(2 * time.Minute)
			second := executeClaudeDatePinRequest(t, executor, auth, payload, "date-pin-session-1", false)
			stream := executeClaudeDatePinRequest(t, executor, auth, payload, "date-pin-session-1", true)
			firstMessage := gjson.GetBytes(first, "messages.0").Raw
			if firstMessage != gjson.GetBytes(second, "messages.0").Raw ||
				firstMessage != gjson.GetBytes(stream, "messages.0").Raw {
				t.Fatalf(
					"messages[0] changed within one session across midnight: first=%s second=%s stream=%s",
					first, second, stream,
				)
			}
			for _, body := range [][]byte{first, second, stream} {
				if got := extractClaudeCloakDate(t, body); got != "2026-08-01" {
					t.Fatalf("same-session date = %q, want 2026-08-01", got)
				}
			}
			fresh := executeClaudeDatePinRequest(t, executor, auth, payload, "date-pin-session-2", false)
			if got := extractClaudeCloakDate(t, fresh); got != "2026-08-02" {
				t.Fatalf("fresh session date = %q, want 2026-08-02", got)
			}
		},
	)
}

func TestClaudeCloakedDateReminderProbeOverrideDoesNotReinitializeSessionDate(t *testing.T) {
	cache.StoreSignature(t.Name(), t.Name(), strings.Repeat("s", 64))
	t.Cleanup(func() { cache.ClearSignatureCache(t.Name()) })
	helps.ResetClaudeDiagnosticsForTest()
	t.Cleanup(helps.ResetClaudeDiagnosticsForTest)
	synctest.Test(
		t, func(t *testing.T) {
			before := time.Date(2026, 8, 1, 23, 59, 0, 0, time.FixedZone("Asia/Shanghai", 8*60*60))
			time.Sleep(time.Until(before))
			cfg := &config.Config{
				Payload: config.PayloadConfig{
					Override: []config.PayloadRule{
						{
							Models: []config.PayloadModelRule{{Name: "claude-opus-5", Protocol: "claude"}},
							Params: map[string]any{"max_tokens": 100},
						},
					},
				},
			}
			auth := claudeNativeHelperOAuthAuth("https://api.anthropic.com")
			auth.ID = "test-date-pin-exec-oauth-declass"
			auth.Attributes["api_key"] = "sk-ant-oat-test-oauth-key-date-pin"
			auth.Metadata[claudeauth.DeviceIDsMetadataKey] = []string{
				"00000000000000000000000000000000000000000000000000000000000000d1",
			}
			auth.Attributes["cloak_mode"] = "always"
			auth.Attributes["timezone"] = "Asia/Shanghai"
			executor := NewClaudeExecutor(cfg)
			normal := []byte(`{"model":"claude-opus-5",` +
				`"messages":[{"role":"user","content":"normal request"}],"max_tokens":100}`)
			first := executeClaudeDatePinRequest(t, executor, auth, normal, "declass-session-1", false)
			if got := extractClaudeCloakDate(t, first); got != "2026-08-01" {
				t.Fatalf("initial date = %q, want 2026-08-01", got)
			}
			time.Sleep(2 * time.Minute)
			probe := []byte(`{"model":"claude-opus-5",` +
				`"messages":[{"role":"user","content":"probe"}],"max_tokens":1}`)
			for _, stream := range []bool{false, true} {
				body := executeClaudeDatePinRequest(t, executor, auth, probe, "declass-session-1", stream)
				if got := gjson.GetBytes(body, "max_tokens").Int(); got != 100 {
					t.Errorf("configured request stream=%t max_tokens = %d, want 100", stream, got)
				}
				if got := extractClaudeCloakDate(t, body); got != "2026-08-01" {
					t.Errorf("configured request stream=%t date = %q, want pinned session date 2026-08-01", stream, got)
				}
			}
		},
	)
}
