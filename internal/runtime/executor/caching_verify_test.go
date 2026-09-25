package executor

import (
	"bytes"
	"fmt"
	"strings"
	"testing"

	"github.com/tidwall/gjson"
)

func TestEnsureCacheControl(t *testing.T) {
	// Test case 1: System prompt as string
	t.Run(
		"String System Prompt", func(t *testing.T) {
			input := []byte(`{"model": "claude-3-5-sonnet", "system": "This is a long system prompt", "messages": []}`)
			output := ensureCacheControl(input)

			res := gjson.GetBytes(output, "system.0.cache_control.type")
			if res.String() != "ephemeral" {
				t.Errorf("cache_control not found in system string. Output: %s", string(output))
			}
		},
	)

	// Test case 2: System prompt as array
	t.Run(
		"Array System Prompt", func(t *testing.T) {
			input := []byte(
				`{"model": "claude-3-5-sonnet", "system": [{"type": "text", "text": "Part 1"}, {"type": "text", "text": "Part 2"}], "messages": []}`,
			)
			output := ensureCacheControl(input)

			// cache_control should only be on the LAST element
			res0 := gjson.GetBytes(output, "system.0.cache_control")
			res1 := gjson.GetBytes(output, "system.1.cache_control.type")

			if res0.Exists() {
				t.Errorf("cache_control should NOT be on the first element")
			}
			if res1.String() != "ephemeral" {
				t.Errorf("cache_control not found on last system element. Output: %s", string(output))
			}
		},
	)

	// Test case 3: A usable system prompt covers the tools prefix, so native
	// Claude Code does not add a redundant tools breakpoint.
	t.Run(
		"Tools Not Auto Cached", func(t *testing.T) {
			input := []byte(`{
			"model": "claude-3-5-sonnet",
			"tools": [
				{"name": "tool1", "description": "First tool", "input_schema": {"type": "object"}},
				{"name": "tool2", "description": "Second tool", "input_schema": {"type": "object"}}
			],
			"system": "System prompt",
			"messages": []
		}`)
			output := ensureCacheControl(input)

			if gjson.GetBytes(output, "tools.0.cache_control").Exists() ||
				gjson.GetBytes(output, "tools.1.cache_control").Exists() {
				t.Errorf("default ensureCacheControl must not stamp tools: %s", string(output))
			}
			systemCache := gjson.GetBytes(output, "system.0.cache_control")
			if systemCache.Get("type").String() != "ephemeral" {
				t.Errorf("cache_control not found in system. Output: %s", string(output))
			}
			if systemCache.Get("ttl").Exists() {
				t.Errorf("default system cache_control must not carry ttl. Output: %s", string(output))
			}
		},
	)

	// Test case 4: Tools and system are INDEPENDENT breakpoints
	// Per Anthropic docs: Up to 4 breakpoints allowed, tools and system are cached separately
	t.Run(
		"Independent Cache Breakpoints", func(t *testing.T) {
			input := []byte(`{
			"model": "claude-3-5-sonnet",
			"tools": [
				{"name": "tool1", "description": "First tool", "input_schema": {"type": "object"}, "cache_control": {"type": "ephemeral"}}
			],
			"system": [{"type": "text", "text": "System"}],
			"messages": []
		}`)
			output := ensureCacheControl(input)

			// Tool already has cache_control - should not be changed
			tool0Cache := gjson.GetBytes(output, "tools.0.cache_control.type")
			if tool0Cache.String() != "ephemeral" {
				t.Errorf("existing cache_control was incorrectly removed")
			}

			// System SHOULD get cache_control because it is an INDEPENDENT breakpoint
			// Tools and system are separate cache levels in the hierarchy
			systemCache := gjson.GetBytes(output, "system.0.cache_control.type")
			if systemCache.String() != "ephemeral" {
				t.Errorf("system should have its own cache_control breakpoint (independent of tools)")
			}
		},
	)

	// Test case 5: Without a usable system prompt, the last non-deferred tool
	// hosts the stable-prefix fallback while the rolling message marker remains.
	t.Run(
		"Only Tools No System Falls Back To Tools Breakpoint", func(t *testing.T) {
			input := []byte(`{
			"model": "claude-3-5-sonnet",
			"tools": [
				{"name": "tool1", "description": "Tool", "input_schema": {"type": "object"}},
				{"name": "tool2", "description": "Tool", "input_schema": {"type": "object"}}
			],
			"messages": [{"role": "user", "content": "Hi"}]
		}`)
			output := ensureCacheControl(input)

			if gjson.GetBytes(output, "tools.0.cache_control").Exists() {
				t.Errorf("only the last tool may host the fallback breakpoint: %s", string(output))
			}
			if got := gjson.GetBytes(output, "tools.1.cache_control.type").String(); got != "ephemeral" {
				t.Errorf("missing tools fallback breakpoint when system is absent: %s", string(output))
			}
			if gjson.GetBytes(output, "tools.1.cache_control.ttl").Exists() {
				t.Errorf("tools fallback breakpoint must not carry a default ttl: %s", string(output))
			}
			if got := gjson.GetBytes(output, "messages.0.content.0.cache_control.type").String(); got != "ephemeral" {
				t.Errorf("rolling message breakpoint should still be present: %s", string(output))
			}
		},
	)

	t.Run(
		"Empty System Still Falls Back To Tools", func(t *testing.T) {
			for name, system := range map[string]string{
				"empty array":  `"system": [],`,
				"empty string": `"system": "",`,
				"blank string": `"system": "   ",`,
			} {
				t.Run(name, func(t *testing.T) {
					input := []byte(`{
					"model": "claude-3-5-sonnet",
					` + system + `
					"tools": [{"name": "tool1", "description": "Tool", "input_schema": {"type": "object"}}],
					"messages": [{"role": "user", "content": "Hi"}]
				}`)
					output := ensureCacheControl(input)

					if got := gjson.GetBytes(output, "tools.0.cache_control.type").String(); got != "ephemeral" {
						t.Errorf("an unusable system prompt must still yield a tools breakpoint: %s", string(output))
					}
					if gjson.GetBytes(output, "system.0.cache_control").Exists() {
						t.Errorf("unusable system must not receive its own breakpoint: %s", string(output))
					}
					if countCacheControls(output) != 2 {
						t.Errorf("want tools and message breakpoints only, got %d in %s", countCacheControls(output), string(output))
					}
				})
			}
		},
	)

	// Test case 6: Many tools (Claude Code scenario)
	t.Run(
		"Many Tools (Claude Code Scenario)", func(t *testing.T) {
			// Simulate Claude Code with many tools
			var toolsJSON strings.Builder
			toolsJSON.WriteString(`[`)
			for i := range 50 {
				if i > 0 {
					toolsJSON.WriteString(",")
				}
				toolsJSON.WriteString(
					fmt.Sprintf(
						`{"name": "tool%d", "description": "Tool %d", "input_schema": {"type": "object"}}`, i, i,
					),
				)
			}
			toolsJSON.WriteString(`]`)

			input := fmt.Appendf(
				nil,
				`{
			"model": "claude-3-5-sonnet",
			"tools": %s,
			"system": [{"type": "text", "text": "You are Claude Code"}],
			"messages": [{"role": "user", "content": "Hello"}]
		}`, toolsJSON.String(),
			)

			output := ensureCacheControl(input)

			for i := range 50 {
				path := fmt.Sprintf("tools.%d.cache_control", i)
				if gjson.GetBytes(output, path).Exists() {
					t.Errorf("tool %d should NOT have cache_control under default ensure", i)
				}
			}

			helperOut := injectToolsCacheControl(input)
			if got := gjson.GetBytes(helperOut, "tools.49.cache_control.type").String(); got != "ephemeral" {
				t.Errorf("injectToolsCacheControl should still mark last tool")
			}
			if got := gjson.GetBytes(output, "system.0.cache_control.type").String(); got != "ephemeral" {
				t.Errorf("system should have cache_control, got %q", got)
			}
			if got := gjson.GetBytes(output, "messages.0.content.0.cache_control.type").String(); got != "ephemeral" {
				t.Errorf("latest user should have cache_control, got %q", got)
			}
		},
	)

	// Test case 7: Empty tools array
	t.Run(
		"Empty Tools Array", func(t *testing.T) {
			input := []byte(`{"model": "claude-3-5-sonnet", "tools": [], "system": "Test", "messages": []}`)
			output := ensureCacheControl(input)

			// System should still get cache_control
			systemCache := gjson.GetBytes(output, "system.0.cache_control.type")
			if systemCache.String() != "ephemeral" {
				t.Errorf("system should have cache_control even with empty tools array")
			}
		},
	)

	// Test case 8: Messages caching follows native Claude Code and rolls onto
	// the latest eligible turn.
	t.Run(
		"Messages Caching Latest User", func(t *testing.T) {
			input := []byte(`{
			"model": "claude-3-5-sonnet",
			"messages": [
				{"role": "user", "content": "First user"},
				{"role": "assistant", "content": "Assistant reply"},
				{"role": "user", "content": "Second user"},
				{"role": "assistant", "content": "Assistant reply 2"},
				{"role": "user", "content": "Third user"}
			]
		}`)
			output := ensureCacheControl(input)

			if got := gjson.GetBytes(output, "messages.4.content.0.cache_control.type").String(); got != "ephemeral" {
				t.Errorf("cache_control.type on latest user = %q, want ephemeral. Output: %s", got, string(output))
			}
			if gjson.GetBytes(output, "messages.4.content.0.cache_control.ttl").Exists() {
				t.Errorf("default rolling marker must not carry ttl. Output: %s", string(output))
			}
			if gjson.GetBytes(output, "messages.2.content.0.cache_control").Exists() {
				t.Errorf("second-to-last user turn should NOT have cache_control")
			}
		},
	)

	// Test case 9: An earlier marker does not freeze the rolling breakpoint.
	t.Run(
		"Messages Inject Despite Earlier Cache Control", func(t *testing.T) {
			input := []byte(`{
			"model": "claude-3-5-sonnet",
			"messages": [
				{"role": "user", "content": [{"type": "text", "text": "First user"}]},
				{"role": "assistant", "content": [{"type": "text", "text": "Assistant reply", "cache_control": {"type": "ephemeral"}}]},
				{"role": "user", "content": [{"type": "text", "text": "Second user"}]}
			]
		}`)
			output := ensureCacheControl(input)

			if got := gjson.GetBytes(output, "messages.1.content.0.cache_control.type").String(); got != "ephemeral" {
				t.Errorf("existing cache_control should be preserved. Output: %s", string(output))
			}
			if got := gjson.GetBytes(output, "messages.2.content.0.cache_control.type").String(); got != "ephemeral" {
				t.Errorf("latest eligible message should receive the rolling marker. Output: %s", string(output))
			}
		},
	)

	t.Run(
		"Messages Caching Trailing System String", func(t *testing.T) {
			input := []byte(`{
			"messages": [
				{"role": "user", "content": "User"},
				{"role": "assistant", "content": "Assistant"},
				{"role": "system", "content": "Internal system"}
			]
		}`)
			output := ensureCacheControl(input)
			systemContent := gjson.GetBytes(output, "messages.2.content")
			if !systemContent.IsArray() || len(systemContent.Array()) != 1 {
				t.Fatalf("trailing string system was not replaced by one text block: %s", output)
			}
			if got := systemContent.Get("0.text").String(); got != "Internal system" {
				t.Fatalf("trailing system text = %q, want original text: %s", got, output)
			}
			if got := systemContent.Get("0.cache_control.type").String(); got != "ephemeral" {
				t.Fatalf("trailing string system did not take the native special case: %s", output)
			}
			if gjson.GetBytes(output, "messages.1.content.0.cache_control").Exists() {
				t.Fatalf("preceding assistant must not also receive the rolling marker: %s", output)
			}
		},
	)

	t.Run(
		"Messages Caching Trailing System Array Falls Back", func(t *testing.T) {
			input := []byte(`{
			"messages": [
				{"role": "user", "content": "User"},
				{"role": "assistant", "content": "Assistant"},
				{"role": "system", "content": [{"type": "text", "text": "Internal 1"}, {"type": "text", "text": "Internal 2"}]}
			]
		}`)
			output := ensureCacheControl(input)
			if gjson.GetBytes(output, "messages.2.content.1.cache_control").Exists() {
				t.Fatalf("array-content trailing system must not be marked: %s", output)
			}
			if got := gjson.GetBytes(output, "messages.1.content.0.cache_control.type").String(); got != "ephemeral" {
				t.Fatalf("marker should fall back to the last eligible assistant turn: %s", output)
			}
		},
	)

	t.Run(
		"Messages Caching Trailing Assistant Text", func(t *testing.T) {
			input := []byte(`{
			"messages": [
				{"role": "user", "content": "User"},
				{"role": "assistant", "content": "Assistant prefill"}
			]
		}`)
			output := ensureCacheControl(input)
			if got := gjson.GetBytes(output, "messages.1.content.0.cache_control.type").String(); got != "ephemeral" {
				t.Fatalf("trailing assistant cache_control.type = %q, want ephemeral. Output: %s", got, output)
			}
			if gjson.GetBytes(output, "messages.0.content.0.cache_control").Exists() {
				t.Fatalf("preceding user must not receive the assistant rolling marker: %s", output)
			}
		},
	)

	for _, blockType := range []string{"thinking", "redacted_thinking"} {
		t.Run(
			"Messages Skip Trailing Assistant "+blockType, func(t *testing.T) {
				input := []byte(`{
				"messages": [
					{"role": "user", "content": "User"},
					{"role": "system", "content": "Internal system"},
					{"role": "assistant", "content": [
						{"type": "text", "text": "Assistant"},
						{"type": "` + blockType + `", "thinking": "Internal", "data": "encrypted"}
					]}
				]
			}`)
				output := ensureCacheControl(input)
				if got := gjson.GetBytes(output, "messages.0.content.0.cache_control.type").String(); got != "ephemeral" {
					t.Fatalf("preceding user cache_control.type = %q, want fallback ephemeral. Output: %s", got, output)
				}
				if got := gjson.GetBytes(output, "messages.1.content"); got.Type != gjson.String {
					t.Fatalf("internal system message was rewritten instead of skipped: %s", output)
				}
				if gjson.GetBytes(output, "messages.2.content.1.cache_control").Exists() {
					t.Fatalf("assistant %s block must not receive cache_control: %s", blockType, output)
				}
			},
		)
	}

	t.Run(
		"Native Cache Control Wire Order", func(t *testing.T) {
			input := []byte(`{
			"system": [{"type": "text", "text": "System"}],
			"messages": [{"role": "user", "content": [{"type": "text", "text": "User"}]}]
		}`)
			output := ensureCacheControl(input)
			want := []byte(`"cache_control":{"type":"ephemeral"}`)
			if got := bytes.Count(output, want); got != 2 {
				t.Fatalf("native cache_control wire shape count = %d, want 2. Output: %s", got, output)
			}
			upgraded := upgradeClaudeCacheControlTTL(output, claudeCacheControlTTL1h)
			wantUpgraded := []byte(`"cache_control":{"type":"ephemeral","ttl":"1h"}`)
			if got := bytes.Count(upgraded, wantUpgraded); got != 2 {
				t.Fatalf("upgraded cache_control wire shape count = %d, want 2. Output: %s", got, upgraded)
			}
			if bytes.Contains(upgraded, []byte(`"cache_control":{"ttl":"1h","type":"ephemeral"}`)) {
				t.Fatalf("cache_control keys emitted in non-native order: %s", upgraded)
			}
		},
	)
}

func TestInjectToolsCacheControlSkipsDeferredTools(t *testing.T) {
	tests := []struct {
		name           string
		input          string
		wantCacheIndex int
		wantCacheTTL   string
	}{
		{
			name: "trailing deferred tool",
			input: `{"tools":[
				{"name":"resident","defer_loading":false},
				{"name":"deferred","defer_loading":true}
			]}`,
			wantCacheIndex: 0,
		},
		{
			name: "multiple trailing deferred tools",
			input: `{"tools":[
				{"name":"resident"},
				{"name":"deferred_1","defer_loading":true},
				{"name":"deferred_2","defer_loading":true}
			]}`,
			wantCacheIndex: 0,
		},
		{
			name: "middle deferred tool",
			input: `{"tools":[
				{"name":"resident_1"},
				{"name":"deferred","defer_loading":true},
				{"name":"resident_2"}
			]}`,
			wantCacheIndex: 2,
		},
		{
			name: "all tools deferred",
			input: `{"tools":[
				{"name":"deferred_1","defer_loading":true},
				{"name":"deferred_2","defer_loading":true}
			]}`,
			wantCacheIndex: -1,
		},
		{
			name: "existing cache control",
			input: `{"tools":[
				{"name":"resident_1","cache_control":{"type":"ephemeral","ttl":"1h"}},
				{"name":"resident_2"}
			]}`,
			wantCacheIndex: 0,
			wantCacheTTL:   "1h",
		},
	}

	for _, test := range tests {
		t.Run(
			test.name, func(t *testing.T) {
				output := injectToolsCacheControl([]byte(test.input))
				tools := gjson.GetBytes(output, "tools").Array()
				cacheCount := 0
				for index, tool := range tools {
					cacheControl := tool.Get("cache_control")
					if cacheControl.Exists() {
						cacheCount++
						if index != test.wantCacheIndex {
							t.Errorf(
								"cache_control added to tool %d, want tool %d: %s", index, test.wantCacheIndex,
								string(output),
							)
						}
					}
					if tool.Get("defer_loading").Bool() && cacheControl.Exists() {
						t.Errorf("deferred tool %d must not have cache_control: %s", index, string(output))
					}
				}

				wantCacheCount := 1
				if test.wantCacheIndex < 0 {
					wantCacheCount = 0
				}
				if cacheCount != wantCacheCount {
					t.Errorf("cache_control count = %d, want %d: %s", cacheCount, wantCacheCount, string(output))
				}
				if test.wantCacheTTL != "" {
					path := fmt.Sprintf("tools.%d.cache_control.ttl", test.wantCacheIndex)
					if got := gjson.GetBytes(output, path).String(); got != test.wantCacheTTL {
						t.Errorf("cache_control TTL = %q, want %q: %s", got, test.wantCacheTTL, string(output))
					}
				}
			},
		)
	}
}

// TestCacheControlOrder verifies the correct order: tools -> system -> messages
func TestCacheControlOrder(t *testing.T) {
	input := []byte(`{
		"model": "claude-sonnet-4",
		"tools": [
			{"name": "Read", "description": "Read file", "input_schema": {"type": "object", "properties": {"path": {"type": "string"}}}},
			{"name": "Write", "description": "Write file", "input_schema": {"type": "object", "properties": {"path": {"type": "string"}, "content": {"type": "string"}}}}
		],
		"system": [
			{"type": "text", "text": "You are Claude Code, Anthropic's official CLI for Claude."},
			{"type": "text", "text": "Additional instructions here..."}
		],
		"messages": [
			{"role": "user", "content": "Hello"}
		]
	}`)

	output := ensureCacheControl(input)

	// A usable system prompt covers the tools prefix, so default native
	// placement does not add a redundant tools marker.
	if gjson.GetBytes(output, "tools.0.cache_control").Exists() ||
		gjson.GetBytes(output, "tools.1.cache_control").Exists() {
		t.Error("default ensureCacheControl must not stamp tools")
	}

	// The last system element receives the default marker without a ttl.
	if gjson.GetBytes(output, "system.1.cache_control.type").String() != "ephemeral" {
		t.Error("last system element should have cache_control")
	}
	if gjson.GetBytes(output, "system.1.cache_control.ttl").Exists() {
		t.Error("default last system element must not carry a ttl")
	}
	if got := gjson.GetBytes(upgradeClaudeCacheControlTTL(output, claudeCacheControlTTL1h), "system.1.cache_control.ttl").String(); got != "1h" {
		t.Errorf("upgraded last system element ttl = %q, want 1h", got)
	}

	if gjson.GetBytes(output, "system.0.cache_control").Exists() {
		t.Error("first system element should NOT have cache_control")
	}
}
