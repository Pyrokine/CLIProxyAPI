package executor

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/util"
	cliproxyauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
	sdktranslator "github.com/Pyrokine/CLIProxyAPI/v6/sdk/translator"
	"github.com/tidwall/gjson"
)

func TestShouldEnsureCacheControl(t *testing.T) {
	markerless := []byte(`{"messages":[{"role":"user","content":"x"}]}`)
	withMarker := []byte(`{"messages":[{"role":"user","content":[{"type":"text","text":"x",` +
		`"cache_control":{"type":"ephemeral"}}]}]}`)
	for _, test := range []struct {
		name                string
		payload             []byte
		cloaked             bool
		confirmedClaudeCode bool
		want                bool
	}{
		{name: "confirmed native markerless", payload: markerless, confirmedClaudeCode: true},
		{name: "confirmed native with marker", payload: withMarker, confirmedClaudeCode: true},
		{name: "cloaked with marker", payload: withMarker, cloaked: true, want: true},
		{name: "unconfirmed markerless", payload: markerless, want: true},
		{name: "unconfirmed with marker", payload: withMarker},
		{
			name: "explicit mode markerless", payload: []byte(`{"prompt_cache_options":{"mode":"explicit"},` +
				`"messages":[{"role":"user","content":"x"}]}`),
		},
		{
			name: "explicit mode cloaked", cloaked: true,
			payload: []byte(`{"prompt_cache_options":{"mode":"explicit"},` +
				`"messages":[{"role":"user","content":"x"}]}`),
		},
		{
			name: "explicit mode with marker", cloaked: true,
			payload: []byte(`{"prompt_cache_options":{"mode":"explicit"},"messages":[{"role":"user",` +
				`"content":[{"type":"text","text":"x","cache_control":{"type":"ephemeral"}}]}]}`),
		},
	} {
		t.Run(
			test.name, func(t *testing.T) {
				if !isExplicitPromptCacheMode(test.payload) {
					if got := shouldEnsureCacheControl(
						test.payload, test.cloaked, test.confirmedClaudeCode,
					); got != test.want {
						t.Fatalf("shouldEnsureCacheControl() = %t, want %t", got, test.want)
					}
					return
				}
				var sent []byte
				calls := 0
				ctx := util.WithRoundTripper(
					t.Context(),
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
				auth := &cliproxyauth.Auth{
					Provider: "claude", Attributes: map[string]string{
						"api_key": "synthetic-cache-gate", "base_url": "https://api.anthropic.com",
					},
				}
				if test.cloaked {
					auth = claudeNativeHelperOAuthAuth("https://api.anthropic.com")
				}
				executeClaudeContractRequest(
					t, NewClaudeExecutor(&config.Config{DisableClaudeCloakMode: !test.cloaked}),
					ctx, auth, cliproxyexecutor.Request{Model: "claude-opus-5", Payload: test.payload},
					cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatClaude}, "messages",
				)
				if calls != 1 || countCacheControls(sent) != countCacheControls(test.payload) {
					t.Fatalf("explicit request changed caller cache markers: calls=%d body=%s", calls, sent)
				}
				if gjson.GetBytes(sent, "prompt_cache_options").Exists() {
					t.Fatalf("explicit cache option reached upstream: %s", sent)
				}
			},
		)
	}
}

func TestUpgradeClaudeCacheControlTTL(t *testing.T) {
	t.Run(
		"Upgrades Only Existing Markers", func(t *testing.T) {
			input := []byte(`{` +
				`"tools":[{"name":"t","cache_control":{"type":"ephemeral"}},{"name":"u"}],` +
				`"system":[{"type":"text","text":"s0"},{"type":"text","text":"s1",` +
				`"cache_control":{"type":"ephemeral"}}],` +
				`"messages":[{"role":"user","content":[{"type":"text","text":"a"},{"type":"text","text":"b",` +
				`"cache_control":{"type":"ephemeral"}}]}]}`)
			output := upgradeClaudeCacheControlTTL(input, claudeCacheControlTTL1h)
			for _, path := range []string{"tools.0", "system.1", "messages.0.content.1"} {
				if got := gjson.GetBytes(output, path+".cache_control.ttl").String(); got != "1h" {
					t.Errorf("%s.cache_control.ttl = %q, want 1h. Output: %s", path, got, output)
				}
			}
			for _, path := range []string{"tools.1", "system.0", "messages.0.content.0"} {
				if gjson.GetBytes(output, path+".cache_control").Exists() {
					t.Errorf("%s must not gain a cache_control: %s", path, output)
				}
			}
			if got := countCacheControls(output); got != 3 {
				t.Errorf("breakpoint count = %d, want the original 3", got)
			}
		},
	)
	t.Run(
		"Preserves Caller TTL And Is Idempotent", func(t *testing.T) {
			input := []byte(`{"system":[{"type":"text","text":"s",` +
				`"cache_control":{"type":"ephemeral","ttl":"5m"}}]}`)
			output := upgradeClaudeCacheControlTTL(input, claudeCacheControlTTL1h)
			if got := gjson.GetBytes(output, "system.0.cache_control.ttl").String(); got != "5m" {
				t.Errorf("existing ttl = %q, want the caller's 5m to survive", got)
			}
			once := upgradeClaudeCacheControlTTL(
				[]byte(`{"system":[{"type":"text","text":"s",`+
					`"cache_control":{"type":"ephemeral"}}]}`), claudeCacheControlTTL1h,
			)
			twice := upgradeClaudeCacheControlTTL(once, claudeCacheControlTTL1h)
			if !bytes.Equal(once, twice) {
				t.Errorf("upgrade is not idempotent: %s vs %s", once, twice)
			}
		},
	)
	t.Run(
		"Keeps Native Key Order With Scope", func(t *testing.T) {
			input := []byte(`{"system":[{"type":"text","text":"s",` +
				`"cache_control":{"type":"ephemeral","scope":"global"}}]}`)
			output := upgradeClaudeCacheControlTTL(input, claudeCacheControlTTL1h)
			want := []byte(`"cache_control":{"type":"ephemeral","ttl":"1h","scope":"global"}`)
			if !bytes.Contains(output, want) {
				t.Errorf("scope-bearing marker lost native type, ttl, scope order: %s", output)
			}
		},
	)
	t.Run(
		"No TTL Is A No-op", func(t *testing.T) {
			input := []byte(`{"system":[{"type":"text","text":"s","cache_control":{"type":"ephemeral"}}]}`)
			if output := upgradeClaudeCacheControlTTL(input, ""); !bytes.Equal(output, input) {
				t.Errorf("empty ttl must be a no-op: %s", output)
			}
			if output := upgradeClaudeCacheControlTTL(
				[]byte(`not json`), claudeCacheControlTTL1h,
			); string(output) != "not json" {
				t.Errorf("invalid payload must be returned untouched: %s", output)
			}
		},
	)
}

func TestClaudeExecutorCloakedRollingCacheBreakpointAdvances(t *testing.T) {
	buildConversation := func(exchanges int) []byte {
		messages := make([]string, 0, exchanges*2)
		for i := range exchanges {
			messages = append(
				messages,
				fmt.Sprintf(`{"role":"user","content":"question number %d"}`, i),
				fmt.Sprintf(`{"role":"assistant","content":"answer number %d"}`, i),
			)
		}
		return []byte(`{"model":"claude-opus-5","max_tokens":100,` +
			`"system":"You are a helpful assistant.",` +
			`"messages":[` + strings.Join(messages, ",") + `]}`)
	}
	lastMarkedMessage := func(body []byte) int {
		last := -1
		gjson.GetBytes(body, "messages").ForEach(
			func(msgIdx, message gjson.Result) bool {
				message.Get("content").ForEach(
					func(_, block gjson.Result) bool {
						if block.Get("cache_control").Exists() {
							last = int(msgIdx.Int())
						}
						return true
					},
				)
				return true
			},
		)
		return last
	}
	executeConversation := func(payload []byte) []byte {
		var sent []byte
		calls := 0
		ctx := util.WithRoundTripper(
			t.Context(),
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
		executeClaudeContractRequest(
			t, NewClaudeExecutor(&config.Config{}), ctx,
			claudeNativeHelperOAuthAuth("https://api.anthropic.com"),
			cliproxyexecutor.Request{Model: "claude-opus-5", Payload: payload},
			cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatClaude}, "messages",
		)
		if calls != 1 {
			t.Fatalf("conversation made %d requests, want 1", calls)
		}
		return sent
	}
	shortBody := executeConversation(buildConversation(2))
	longBody := executeConversation(buildConversation(6))
	shortMarked, longMarked := lastMarkedMessage(shortBody), lastMarkedMessage(longBody)
	if shortMarked <= 0 {
		t.Fatalf("short conversation kept its only breakpoint at index %d: %s", shortMarked, shortBody)
	}
	if longMarked <= shortMarked {
		t.Fatalf(
			"rolling breakpoint did not advance with history: short=%d long=%d\n%s", shortMarked, longMarked, longBody,
		)
	}
	if want := int(gjson.GetBytes(longBody, "messages.#").Int()) - 1; longMarked != want {
		t.Fatalf("rolling breakpoint at message %d, want final message %d: %s", longMarked, want, longBody)
	}
	if !gjson.GetBytes(longBody, "messages.0.content.1.cache_control").Exists() {
		t.Fatalf("cloak first-user breakpoint lost: %s", longBody)
	}
	if total := countCacheControls(longBody); total > 4 {
		t.Fatalf("cache_control count = %d, want at most 4: %s", total, longBody)
	}
}
