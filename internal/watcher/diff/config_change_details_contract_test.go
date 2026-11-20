package diff

import (
	"encoding/json"
	"fmt"
	"maps"
	"reflect"
	"strings"
	"testing"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
)

func TestConfigChangeDetailsModelMetadata(t *testing.T) {
	for _, provider := range []struct {
		key, label string
		fields     []string
	}{
		{
			"gemini-api-key", "gemini", []string{
				`"display-name":"New display"`, `"max-context-length":262144`, `"is-compat":true`,
				`"thinking":{"levels":["high"]}`,
			},
		},
		{
			"interactions-api-key", "interactions", []string{
				`"display-name":"New display"`, `"max-context-length":262144`, `"is-compat":true`,
				`"thinking":{"levels":["high"]}`,
			},
		},
		{
			"claude-api-key", "claude", []string{
				`"display-name":"New display"`, `"max-context-length":262144`, `"is-compat":true`,
				`"thinking":{"levels":["high"]}`,
			},
		},
		{
			"vertex-api-key", "vertex",
			[]string{`"display-name":"New display"`, `"force-mapping":true`, `"thinking":{"levels":["high"]}`},
		},
		{
			"codex-api-key", "codex", []string{
				`"display-name":"New display"`, `"max-context-length":262144`, `"support-configuration-update":true`,
				`"force-mapping":true`, `"is-compat":true`, `"thinking":{"levels":["high"]}`,
			},
		},
		{
			"xai-api-key", "xai", []string{
				`"display-name":"New display"`, `"max-context-length":262144`, `"support-configuration-update":true`,
				`"force-mapping":true`, `"is-compat":true`, `"thinking":{"levels":["high"]}`,
			},
		},
	} {
		for _, field := range provider.fields {
			t.Run(
				provider.label+"/"+strings.SplitN(field, ":", 2)[0], func(t *testing.T) {
					before := fmt.Sprintf(`{%q:[{"models":[{"name":"model","alias":"alias"}]}]}`, provider.key)
					after := fmt.Sprintf(
						`{%q:[{"models":[{"name":"model","alias":"alias",%s}]}]}`,
						provider.key,
						field,
					)
					want := provider.label + "[0].models: updated (1 -> 1 entries)"
					assertConfigChangeDetails(t, before, after, want)
					assertConfigChangeDetails(t, after, before, want)
				},
			)
		}
	}
	t.Run(
		"vertex-upstream-name-with-stable-alias", func(t *testing.T) {
			assertConfigChangeDetails(
				t,
				`{"vertex-api-key":[{"models":[{"name":"before","alias":"alias"}]}]}`,
				`{"vertex-api-key":[{"models":[{"name":"after","alias":"alias"}]}]}`,
				"vertex[0].models: updated (1 -> 1 entries)",
			)
		},
	)
}

func TestConfigChangeDetailsSupportedConfiguration(t *testing.T) {
	//goland:noinspection HttpUrlsUsage
	for _, testCase := range []struct {
		name, before, after, want string
	}{
		{
			"redis-retention", `{}`, `{"redis-usage-queue-retention-seconds":180}`,
			"redis-usage-queue-retention-seconds: 0 -> 180",
		},
		{
			"transient-error-cooldown", `{}`, `{"transient-error-cooldown-seconds":-1}`,
			"transient-error-cooldown-seconds: 0 -> -1",
		},
		{"claude-global-cloak", `{}`, `{"disable-claude-cloak-mode":true}`, "disable-claude-cloak-mode: false -> true"},
		{"image-boolean-input", `{}`, `{"disable-image-generation":true}`, "disable-image-generation: off -> all"},
		{
			"image-chat", `{"disable-image-generation":"all"}`, `{"disable-image-generation":"chat"}`,
			"disable-image-generation: all -> chat",
		},
		{
			"image-passthrough", `{"disable-image-generation":"chat"}`, `{"disable-image-generation":"passthrough"}`,
			"disable-image-generation: chat -> passthrough",
		},
		{
			"interactions-count", `{}`, `{"interactions-api-key":[{"api-key":"synthetic-new-secret"}]}`,
			"interactions-api-key count: 0 -> 1",
		},
		{
			"interactions-removal", `{"interactions-api-key":[{"api-key":"synthetic-old-secret"}]}`, `{}`,
			"interactions-api-key count: 1 -> 0",
		},
		{
			"interactions-key", `{"interactions-api-key":[{"api-key":"synthetic-old-secret"}]}`,
			`{"interactions-api-key":[{"api-key":"synthetic-new-secret"}]}`, "interactions[0].api-key: updated",
		},
		{
			"interactions-base-url",
			`{"interactions-api-key":[{"base-url":"https://old-user:synthetic-old-s` +
				`ecret@before.invalid/private?token=synthetic-old-secret"}]}`,
			`{"interactions-api-key":[{"base-url":"https://new-user:synthetic-new-s` +
				`ecret@after.invalid/private?token=synthetic-new-secret"}]}`,
			"interactions[0].base-url: https://before.invalid -> https://after.invalid",
		},
		{
			"interactions-proxy", `{"interactions-api-key":[{}]}`,
			`{"interactions-api-key":[{"proxy-url":"http://new-user:synthetic-new-s` +
				`ecret@proxy.invalid:8080/private?token=synthetic-new-secret"}]}`,
			"interactions[0].proxy-url: <none> -> http://proxy.invalid:8080",
		},
		{
			"interactions-prefix", `{"interactions-api-key":[{}]}`, `{"interactions-api-key":[{"prefix":"team"}]}`,
			"interactions[0].prefix:  -> team",
		},
		{
			"interactions-headers", `{"interactions-api-key":[{"headers":{"Authorization":"synthetic-old-secret"}}]}`,
			`{"interactions-api-key":[{"headers":{"Authorization":"synthetic-new-secret"}}]}`,
			"interactions[0].headers: updated",
		},
		{
			"interactions-models", `{"interactions-api-key":[{"models":[{"name":"before"}]}]}`,
			`{"interactions-api-key":[{"models":[{"name":"after"}]}]}`,
			"interactions[0].models: updated (1 -> 1 entries)",
		},
		{
			"interactions-exclusions", `{"interactions-api-key":[{}]}`,
			`{"interactions-api-key":[{"excluded-models":["hidden"]}]}`,
			"interactions[0].excluded-models: updated (0 -> 1 entries)",
		},
		{
			"interactions-same-count-exclusions", `{"interactions-api-key":[{"excluded-models":["old"]}]}`,
			`{"interactions-api-key":[{"excluded-models":["new"]}]}`,
			"interactions[0].excluded-models: updated (1 -> 1 entries)",
		},
		{
			"interactions-cooling", `{"interactions-api-key":[{}]}`,
			`{"interactions-api-key":[{"disable-cooling":false}]}`, "interactions[0].disable-cooling: inherit -> false",
		},
		{
			"interactions-retry", `{"interactions-api-key":[{}]}`, `{"interactions-api-key":[{"request-retry":0}]}`,
			"interactions[0].request-retry: inherit -> 0",
		},
		{
			"interactions-error-rule", `{"interactions-api-key":[{}]}`,
			`{"interactions-api-key":[{"request-scoped-errors":[{"status":422,"match":["synthetic-new-secret"` +
				`],"action":"continue"}]}]}`,
			"interactions[0].request-scoped-errors: updated (0 -> 1 entries)",
		},
		{
			"vertex-exclusions", `{"vertex-api-key":[{}]}`, `{"vertex-api-key":[{"excluded-models":["hidden"]}]}`,
			"vertex[0].excluded-models: updated (0 -> 1 entries)",
		},
		{
			"vertex-same-count-exclusions", `{"vertex-api-key":[{"excluded-models":["old"]}]}`,
			`{"vertex-api-key":[{"excluded-models":["new"]}]}`, "vertex[0].excluded-models: updated (1 -> 1 entries)",
		},
		{
			"claude-cloak-created", `{"claude-api-key":[{}]}`, `{"claude-api-key":[{"cloak":{"mode":"always"}}]}`,
			"claude[0].cloak: created",
		},
		{
			"claude-cloak-removed", `{"claude-api-key":[{"cloak":{"mode":"always"}}]}`, `{"claude-api-key":[{}]}`,
			"claude[0].cloak: deleted",
		},
		{
			"claude-cloak-same-count-words",
			`{"claude-api-key":[{"cloak":{"sensitive-words":["synthetic-old-secret"]}}]}`,
			`{"claude-api-key":[{"cloak":{"sensitive-words":["synthetic-new-secret"]}}]}`,
			"claude[0].cloak.sensitive-words: updated (1 -> 1 entries)",
		},
		{"debug-control", `{}`, `{"debug":true}`, "debug: false -> true"},
	} {
		t.Run(
			testCase.name, func(t *testing.T) {
				assertConfigChangeDetails(t, testCase.before, testCase.after, testCase.want)
			},
		)
	}
}

func TestConfigChangeDetailsOpenAICompatibilitySameCountUpdates(t *testing.T) {
	for _, field := range []string{
		`"alias":"new-alias"`, `"display-name":"New display"`, `"max-context-length":262144`,
		`"image":true`, `"input-modalities":["text","image"]`, `"output-modalities":["image"]`,
		`"use-max-completion-tokens":true`, `"is-compat":true`, `"thinking":{"levels":["high"]}`,
	} {
		t.Run(
			strings.SplitN(field, ":", 2)[0], func(t *testing.T) {
				before := `{"openai-compatibility":[{"name":"compat","models":[{"name":"model"}]}]}`
				after := fmt.Sprintf(
					`{"openai-compatibility":[{"name":"compat","models":[{"name":"model",%s}]}]}`, field,
				)
				assertConfigChangeDetails(t, before, after, "  provider updated: compat (models updated)")
			},
		)
	}
	for _, testCase := range []struct{ name, field, want string }{
		{"credential", `"api-key-entries":[{"api-key":"synthetic-new-secret"}]`, "api-keys updated (redacted)"},
		{
			"proxy",
			`"api-key-entries":[{"api-key":"synthetic-old-secret","proxy-url":"http` +
				`://new-user:synthetic-new-secret@proxy.invalid:8080"}]`,
			"api-keys updated (redacted)",
		},
		{
			"disabled-key", `"api-key-entries":[{"api-key":"synthetic-old-secret","disabled":true}]`,
			"api-keys updated (redacted)",
		},
		{"weight", `"api-key-entries":[{"api-key":"synthetic-old-secret","weight":0}]`, "api-keys updated (redacted)"},
		{
			"base-url",
			`"base-url":"https://new-user:synthetic-new-secret@after.invalid/private?token=synthetic-new-secret"`,
			"base-url <none> -> https://after.invalid",
		},
		{"prefix", `"prefix":"team"`, "prefix  -> team"},
		{"priority", `"priority":2`, "priority 0 -> 2"},
	} {
		t.Run(
			testCase.name, func(t *testing.T) {
				before := `{"openai-compatibility":[{"name":"compat","api-key-entries":` +
					`[{"api-key":"synthetic-old-secret"}]}]}`
				var after map[string]any
				if err := json.Unmarshal([]byte(before), &after); err != nil {
					t.Fatal(err)
				}
				var update map[string]any
				if err := json.Unmarshal([]byte("{"+testCase.field+"}"), &update); err != nil {
					t.Fatal(err)
				}
				entry := after["openai-compatibility"].([]any)[0].(map[string]any)
				maps.Copy(entry, update)
				encoded, err := json.Marshal(after)
				if err != nil {
					t.Fatal(err)
				}
				assertConfigChangeDetails(
					t,
					before,
					string(encoded),
					"  provider updated: compat ("+testCase.want+")",
				)
			},
		)
	}
}

func TestConfigChangeDetailsCredentialSelection(t *testing.T) {
	for _, provider := range []struct{ key, label string }{
		{"gemini-api-key", "gemini"}, {"interactions-api-key", "interactions"},
		{"claude-api-key", "claude"}, {"codex-api-key", "codex"}, {"xai-api-key", "xai"}, {"vertex-api-key", "vertex"},
	} {
		for _, field := range []struct{ value, detail string }{
			{`"priority":2`, "priority: 0 -> 2"}, {`"weight":0`, "weight: inherit -> 0"},
		} {
			t.Run(
				provider.label+"/"+field.value, func(t *testing.T) {
					want := provider.label + "[0]." + field.detail
					if provider.label == "xai" && strings.HasPrefix(field.detail, "weight:") {
						want = "xai[0].weight: updated"
					}
					assertConfigChangeDetails(
						t, fmt.Sprintf(`{%q:[{}]}`, provider.key),
						fmt.Sprintf(`{%q:[{%s}]}`, provider.key, field.value), want,
					)
				},
			)
		}
	}
}

func TestConfigChangeDetailsInteractionsCountRetainsExistingOverrides(t *testing.T) {
	assertConfigChangeDetails(
		t,
		`{"interactions-api-key":[{}]}`,
		`{"interactions-api-key":[{"disable-cooling":false},{}]}`,
		"interactions[0].disable-cooling: inherit -> false",
	)
}

func TestConfigChangeDetailsEmptyHeaderValues(t *testing.T) {
	assertConfigChangeDetails(
		t,
		`{"interactions-api-key":[{"headers":{"X-Before":""}}]}`,
		`{"interactions-api-key":[{"headers":{"X-After":""}}]}`,
		"interactions[0].headers: updated",
	)
	assertConfigChangeDetails(
		t,
		`{"openai-compatibility":[{"name":"compat","headers":{"X-Before":""}}]}`,
		`{"openai-compatibility":[{"name":"compat","headers":{"X-After":""}}]}`,
		"  provider updated: compat (headers updated)",
	)
}

func assertConfigChangeDetails(t *testing.T, beforeJSON, afterJSON, want string) {
	t.Helper()
	var before, after config.Config
	if err := json.Unmarshal([]byte(beforeJSON), &before); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal([]byte(afterJSON), &after); err != nil {
		t.Fatal(err)
	}
	if reflect.DeepEqual(before, after) {
		t.Fatal("test input did not change the configuration")
	}
	changes := BuildConfigChangeDetails(&before, &after)
	expectContains(t, changes, want)
	for _, secret := range []string{
		"synthetic-old-secret", "synthetic-new-secret", "old-user", "new-user", "?token=", "/private",
	} {
		if strings.Contains(strings.Join(changes, "\n"), secret) {
			t.Fatalf("change details disclosed %q: %v", secret, changes)
		}
	}
	if unchanged := BuildConfigChangeDetails(&after, &after); len(unchanged) != 0 {
		t.Fatalf("unchanged configuration reported updates: %v", unchanged)
	}
}
