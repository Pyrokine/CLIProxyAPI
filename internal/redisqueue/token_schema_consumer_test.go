package redisqueue_test

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/redisqueue"
	runtimeexecutor "github.com/Pyrokine/CLIProxyAPI/v6/internal/runtime/executor"
	coreauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
	coreusage "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/usage"
)

func TestUsageQueueNormalizedCacheReadSchemaConsumer(t *testing.T) {
	for _, fixture := range []struct {
		name     string
		protocol string
		payload  string
		wantRead int64
	}{
		{
			name: "OpenAI absent cache", protocol: "openai",
			payload: `{"usage":{"prompt_tokens":100,"completion_tokens":20,"total_tokens":120}}`,
		},
		{
			name: "OpenAI explicit zero", protocol: "openai",
			payload: `{"usage":{"prompt_tokens":100,"completion_tokens":20,"total_tokens":` +
				`120,"prompt_tokens_details":{"cached_tokens":` +
				`0}}}`,
		},
		{
			name: "OpenAI cached", protocol: "openai",
			payload: `{"usage":{"prompt_tokens":100,"completion_tokens":20,"total_tokens":` +
				`120,"prompt_tokens_details":{"cached_tokens":` +
				`7}}}`,
			wantRead: 7,
		},
		{
			name: "Codex cached", protocol: "codex",
			payload: `{"type":"response.completed","response":{"usage":{"input_tokens":100,"output_tokens":` +
				`20,"total_tokens":120,"input_tokens_details":{"cached_tokens":7}}}}`,
			wantRead: 7,
		},
		{
			name: "Claude cache creation only", protocol: "claude",
			payload: `{"type":"message_start","message":{"usage":{"input_tokens":100,` +
				`"output_tokens":20,"cache_creation_input_tokens":` +
				`5}}}`,
		},
		{
			name: "Claude explicit zero", protocol: "claude",
			payload: `{"type":"message_start","message":{"usage":{"input_tokens":100,"output` +
				`_tokens":20,"cache_read_input_tokens":0,"cache_creation_input_tokens":` +
				`5}}}`,
		},
		{
			name: "Gemini cached", protocol: "gemini",
			payload: `{"usageMetadata":{"promptTokenCount":100,"candidatesTokenCount":20,` +
				`"totalTokenCount":120,"cachedContentTokenCount":` +
				`7}}`,
			wantRead: 7,
		},
		{
			name: "Antigravity cached", protocol: "antigravity",
			payload: `{"response":{"usageMetadata":{"promptTokenCount":100,"candidatesTokenCount":20,"` +
				`totalTokenCount":120,"cachedContentTokenCount":7}}}`,
			wantRead: 7,
		},
		{
			name: "Interactions cached", protocol: "interactions",
			payload:  `{"usage":{"input_tokens":100,"output_tokens":20,"total_tokens":120,"cache_read_tokens":7}}`,
			wantRead: 7,
		},
	} {
		for _, streaming := range []bool{false, true} {
			t.Run(
				fmt.Sprintf("%s/stream=%t", fixture.name, streaming), func(t *testing.T) {
					var detail coreusage.Detail
					if streaming {
						buffer := runtimeexecutor.NewPluginExecutorStreamUsage(fixture.protocol)
						buffer.ObserveLine([]byte("data: " + fixture.payload))
						var ok bool
						detail, ok = buffer.Detail()
						if !ok {
							t.Fatal("stream usage was not collected")
						}
					} else {
						detail = runtimeexecutor.ParsePluginExecutorResponseUsage(
							fixture.protocol, []byte(fixture.payload),
						)
					}
					if detail.CacheReadTokens != fixture.wantRead {
						t.Fatalf("parsed cache read=%d, want %d", detail.CacheReadTokens, fixture.wantRead)
					}
					queue := redisqueue.NewQueue()
					queue.SetEnabled(true)
					queue.SetUsageStatisticsEnabled(true)
					manager := coreusage.NewManager(1)
					manager.Register(redisqueue.NewUsagePluginForQueue(queue))
					manager.Start(context.Background())
					t.Cleanup(manager.Stop)
					ctx := coreusage.WithManager(t.Context(), manager)
					reporter := runtimeexecutor.NewPluginExecutorUsageReporter(
						ctx,
						fixture.protocol,
						"schema-model",
						&coreauth.Auth{ID: t.Name(), Provider: fixture.protocol},
					)
					if streaming {
						reporter.MarkStream()
					}
					reporter.Publish(ctx, detail)
					manager.Stop()
					items := queue.PopOldest(2)
					if len(items) != 1 {
						t.Fatalf("queued records=%d, want one", len(items))
					}
					var payload struct {
						Tokens            map[string]json.RawMessage `json:"tokens"`
						AccountingVersion *int64                     `json:"accounting_version"`
						Stream            *bool                      `json:"stream"`
					}
					if err := json.Unmarshal(items[0], &payload); err != nil {
						t.Fatal(err)
					}
					var present bool
					var readTokens int64
					for field, raw := range payload.Tokens {
						if field == "cache_read_tokens_present" {
							if err := json.Unmarshal(raw, &present); err != nil {
								t.Fatalf("unmarshal token presence: %v", err)
							}
							continue
						}
						var count int64
						if err := json.Unmarshal(raw, &count); err != nil {
							t.Fatalf("unmarshal tokens[%q]: %v", field, err)
						}
						if field == "cache_read_tokens" {
							readTokens = count
						}
					}
					if !present || readTokens != fixture.wantRead {
						t.Fatalf(
							"cache read = %d, present = %t, want %d and true",
							readTokens,
							present,
							fixture.wantRead,
						)
					}
					if payload.AccountingVersion == nil ||
						*payload.AccountingVersion != coreusage.TokenAccountingSchemaVersion {
						t.Fatalf("unexpected accounting schema version: %s", items[0])
					}
					if payload.Stream == nil || *payload.Stream != streaming {
						t.Fatalf("unexpected stream marker: %s", items[0])
					}
				},
			)
		}
	}
}
