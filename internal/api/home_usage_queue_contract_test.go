package api

import (
	"fmt"
	"sync/atomic"
	"testing"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/redisqueue"
	coreusage "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/usage"
)

func TestHomeUsageQueueTracksHomeAndDisplaySeparately(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "synthetic-home-count")
	queue := redisqueue.NewQueue()
	queue.SetEnabled(true)
	t.Cleanup(queue.Close)
	var display atomic.Bool
	server := newTestServerWithOptions(t, WithUsageQueue(queue), WithUsageStatisticsToggle(display.Store))
	t.Cleanup(server.quotaScheduler.Stop)
	plugin := redisqueue.NewUsagePluginForQueue(queue)
	for index, state := range []struct{ home, display bool }{
		{false, false}, {true, false}, {true, true}, {true, false}, {false, false}, {false, true},
	} {
		t.Run(
			fmt.Sprintf("%d/home=%t/display=%t", index, state.home, state.display), func(t *testing.T) {
				cfg := *server.configSnapshot()
				cfg.Home.Enabled = state.home
				cfg.UsageStatisticsEnabled = state.display
				server.UpdateClientsForConfigCommit(t.Context(), &cfg, uint64(index+1))
				plugin.HandleUsage(t.Context(), coreusage.Record{Provider: "codex", Model: "gpt-5.4"})
				items := queue.PopOldest(10)
				want := 0
				if state.home || state.display {
					want = 1
				}
				if len(items) != want || display.Load() != state.display {
					t.Errorf(
						"queue records=%d want=%d; display=%t want=%t", len(items), want, display.Load(), state.display,
					)
				}
			},
		)
	}
}
