package executor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/redisqueue"
	coreauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
	coreusage "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/usage"
)

func TestNativeExecutorTypesReachUsageQueue(t *testing.T) {
	cfg := &config.Config{}
	for _, test := range []struct {
		provider string
		typeName string
		executor usageExecutor
	}{
		{"aistudio", "AIStudioExecutor", NewAIStudioExecutor(cfg, "aistudio", nil)},
		{"antigravity", "AntigravityExecutor", NewAntigravityExecutor(cfg)},
		{"claude", "ClaudeExecutor", NewClaudeExecutor(cfg)},
		{"codex", "CodexExecutor", newCodexExecutor(cfg)},
		{"codex", "CodexWebsocketsExecutor", newCodexWebsocketsExecutor(cfg)},
		{"gemini", "GeminiExecutor", NewGeminiExecutor(cfg)},
		{"gemini-cli", "GeminiCLIExecutor", NewGeminiCLIExecutor(cfg)},
		{"vertex", "GeminiVertexExecutor", NewGeminiVertexExecutor(cfg)},
		{"kimi", "KimiExecutor", NewKimiExecutor(cfg)},
		{"compatible-channel", "OpenAICompatExecutor", NewOpenAICompatExecutor("compatible-channel", cfg)},
		{"xai", "XAIExecutor", newXAIExecutor(cfg)},
		{"xai", "XAIWebsocketsExecutor", newXAIWebsocketsExecutor(cfg)},
	} {
		for _, streaming := range []bool{false, true} {
			for _, failed := range []bool{false, true} {
				t.Run(
					fmt.Sprintf("%s/stream=%t/failed=%t", test.typeName, streaming, failed), func(t *testing.T) {
						queue := redisqueue.NewQueue()
						queue.SetEnabled(true)
						queue.SetUsageStatisticsEnabled(true)
						t.Cleanup(queue.Close)
						manager := coreusage.NewManager(1)
						manager.Register(redisqueue.NewUsagePluginForQueue(queue))
						manager.Start(context.Background())
						t.Cleanup(manager.Stop)
						ctx := coreusage.WithManager(t.Context(), manager)
						reporter := NewPluginExecutorUsageReporter(
							ctx, test.executor, "wire-model", &coreauth.Auth{
								ID: t.Name(), Provider: test.provider,
							},
						)
						if streaming {
							reporter.MarkStream()
						}
						detail := coreusage.Detail{InputTokens: 1, TotalTokens: 1}
						if failed {
							reporter.PublishFailureWithDetail(ctx, detail, errors.New("synthetic upstream failure"))
						} else {
							reporter.Publish(ctx, detail)
						}
						manager.Stop()
						items := queue.PopOldest(2)
						if len(items) != 1 {
							t.Fatalf("queued records = %d, want one", len(items))
						}
						var payload struct {
							Provider     string `json:"provider"`
							ExecutorType string `json:"executor_type"`
							Stream       bool   `json:"stream"`
							Failed       bool   `json:"failed"`
						}
						if err := json.Unmarshal(items[0], &payload); err != nil {
							t.Fatal(err)
						}
						if payload.Provider != test.provider || payload.ExecutorType != test.typeName {
							t.Fatalf(
								"usage identity = %s/%s, want %s/%s",
								payload.Provider, payload.ExecutorType, test.provider, test.typeName,
							)
						}
						if payload.Stream != streaming || payload.Failed != failed {
							t.Fatalf(
								"usage outcome = stream:%t failed:%t, want stream:%t failed:%t",
								payload.Stream, payload.Failed, streaming, failed,
							)
						}
					},
				)
			}
		}
	}
}
