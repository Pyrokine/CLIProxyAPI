package auth

import (
	"context"
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executionregistry"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
)

func TestHomeDispatchCountAcrossEntrypoints(t *testing.T) {
	for _, entry := range []string{"execute", "count", "stream", "stream-bootstrap"} {
		for _, display := range []bool{false, true} {
			t.Run(
				fmt.Sprintf("%s/display=%t", entry, display), func(t *testing.T) {
					dispatcher := &retryContractHomeDispatcher{authIDs: []string{"home-retry-a", "home-retry-b"}}
					provider := &retryContractHomeExecutor{streamBootstrap: entry == "stream-bootstrap"}
					manager := NewManager(nil, nil, nil)
					manager.SetConfig(
						&config.Config{
							Home: config.HomeConfig{Enabled: true}, UsageStatisticsEnabled: display,
						},
					)
					manager.SetRetryConfig(0, time.Second, 2)
					registry := executionregistry.New()
					manager.PublishHomeDispatch(dispatcher, registry, 1)
					manager.RegisterExecutor(provider)
					t.Cleanup(
						func() {
							if err := registry.Drain(context.Background()); err != nil {
								t.Error(err)
							}
						},
					)
					req := executor.Request{Model: "gpt"}
					var payload []byte
					var err error
					switch entry {
					case "execute", "count":
						var response executor.Response
						if entry == "count" {
							response, err = manager.ExecuteCount(
								t.Context(), []string{provider.Identifier()}, req, executor.Options{},
							)
						} else {
							response, err = manager.Execute(
								t.Context(), []string{provider.Identifier()}, req, executor.Options{},
							)
						}
						payload = response.Payload
					case "stream", "stream-bootstrap":
						var response *executor.StreamResult
						response, err = manager.ExecuteStream(
							t.Context(), []string{provider.Identifier()}, req, executor.Options{},
						)
						if err == nil {
							for chunk := range response.Chunks {
								if chunk.Err != nil {
									err = chunk.Err
								}
								payload = append(payload, chunk.Payload...)
							}
						}
					}
					if err != nil || string(payload) != "home-retry-b" {
						t.Fatalf("execution result=%q error=%v", payload, err)
					}
					dispatcher.mu.Lock()
					counts := append([]int(nil), dispatcher.counts...)
					dispatcher.mu.Unlock()
					if !slices.Equal(counts, []int{1, 2}) || !slices.Equal(
						provider.Calls(), []string{"home-retry-a", "home-retry-b"},
					) {
						t.Fatalf("Home dispatch counts=%v executor calls=%v", counts, provider.Calls())
					}
				},
			)
		}
	}
}
