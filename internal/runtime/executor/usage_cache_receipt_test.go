package executor

import (
	"context"
	"reflect"
	"testing"
	"time"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	persistusage "github.com/Pyrokine/CLIProxyAPI/v6/internal/usage"
	coreusage "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/usage"
)

func TestClaudeCacheTTLReachesPricingReceipt(t *testing.T) {
	const initialUsage = `{"speed":"standard","input_tokens":10,"cache_read_input_tokens":3,"cache_creation_input_tokens":15,"` +
		`cache_creation":{"ephemeral_5m_input_tokens":6,"ephemeral_1h_input_tokens":7}}`
	const outputUsage = `{"output_tokens":5,"output_tokens_details":{"thinking_tokens":2}}`
	for _, tc := range []struct {
		name   string
		stream bool
		usage  string
	}{
		{name: "nonstream-known-and-unknown-ttl"},
		{name: "stream-output-only-retains-unknown-ttl", stream: true, usage: outputUsage},
		{
			name: "stream-repeated-5m-retains-unknown-ttl", stream: true,
			usage: `{"output_tokens":5,"output_tokens_details":{"thinking_tokens":2},` +
				`"cache_creation":{"ephemeral_5m_input_tokens":` +
				`6}}`,
		},
		{
			name: "stream-repeated-1h-retains-unknown-ttl", stream: true,
			usage: `{"output_tokens":5,"output_tokens_details":{"thinking_tokens":2},` +
				`"cache_creation":{"ephemeral_1h_input_tokens":` +
				`7}}`,
		},
	} {
		t.Run(
			tc.name, func(t *testing.T) {
				ctx := t.Context()
				persister := persistusage.NewPersister(t.TempDir(), config.UsageRetention{Days: -1})
				persister.SetPriceFunc(
					func(string) (float64, float64, float64, bool) {
						return 1000, 2000, 500, true
					},
				)
				persister.Start(ctx)
				if !persister.IsReady() {
					t.Fatal("temporary usage persister did not start")
				}
				t.Cleanup(
					func() {
						stopCtx, stopCancel := context.WithTimeout(context.Background(), 3*time.Second)
						defer stopCancel()
						if err := persister.StopContext(stopCtx); err != nil {
							t.Errorf("stop temporary persister: %v", err)
						}
					},
				)
				manager := coreusage.NewManager(4)
				manager.Register(persistusage.NewLoggerPlugin(persister, nil, func() bool { return true }))
				manager.Start(ctx)
				t.Cleanup(manager.Stop)
				publishCtx := coreusage.WithManager(ctx, manager)
				requestedAt := time.Now().UTC()
				reporter := &usageReporter{
					provider: "claude", executorType: "claude", model: "synthetic-cache-model",
					alias: "synthetic-cache-model", requestedAt: requestedAt, generate: true,
				}
				var parsed coreusage.Detail
				if tc.stream {
					lines := []string{
						`data: {"type":"message_start","message":{"id":"msg-synthetic-cache","t` +
							`ype":"message","role":"assistant","model":"synthetic-cache-model","con` +
							`tent":[],"stop_reason":null,"stop_sequence":null,"usage":` + initialUsage + `}}`,
						`data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
						`data: {"type":"content_block_delta","index":0,"delta":{"type":` +
							`"text_delta","text":` +
							`"synthetic"}}`,
						`data: {"type":"content_block_stop","index":0}`,
						`data: {"type":"message_delta","delta":{"stop_reason":"end_turn",` +
							`"stop_sequence":null},"usage":` +
							tc.usage +
							`}`,
						`data: {"type":"message_stop"}`,
					}
					var buffer claudeStreamUsageBuffer
					validator := &claudeStreamingValidator{}
					for _, line := range lines {
						if err := validator.observe([]byte(line)); err != nil {
							t.Fatalf("stream validator: %v", err)
						}
						reporter.observeResponseChunk([]byte(line))
						buffer.observe([]byte(line))
					}
					if err := validator.finish(); err != nil {
						t.Fatalf("finish stream: %v", err)
					}
					var ok bool
					parsed, ok = buffer.detailValue()
					if !ok || !buffer.publish(publishCtx, reporter) {
						t.Fatal("stream usage buffer did not publish")
					}
				} else {
					payload := []byte(`{"usage":{"speed":"standard","input_tokens":10,"output_tokens":5,` +
						`"output_tokens_details":{"thinking_tokens":2},"cache_read_input_tokens":3,` +
						`"cache_creation_input_tokens":15,"cache_creation":{"ephemeral_5m_input_tokens":6,` +
						`"ephemeral_1h_input_tokens":7}}}`)
					reporter.observeResponseChunk(payload)
					parsed = parseClaudeUsage(payload)
					reporter.publish(publishCtx, parsed)
				}
				drainCtx, cancelDrain := context.WithTimeout(ctx, 8*time.Second)
				defer cancelDrain()
				if err := manager.StopContext(drainCtx); err != nil {
					t.Fatalf("drain usage manager: %v", err)
				}
				queryCtx, cancelQuery := context.WithTimeout(ctx, 8*time.Second)
				defer cancelQuery()
				var records []persistusage.FlatDetail
				ticker := time.NewTicker(10 * time.Millisecond)
				defer ticker.Stop()
				for {
					var total int
					var err error
					records, total, err = persister.QueryEventsContext(
						queryCtx,
						requestedAt.Add(-time.Minute),
						requestedAt.Add(time.Minute),
						persistusage.EventFilters{},
						1,
						10,
						"timestamp",
						false,
					)
					if err != nil {
						t.Fatalf("query SQLite event: %v", err)
					}
					if total == 1 && len(records) == 1 {
						break
					}
					select {
					case <-queryCtx.Done():
						t.Fatalf(
							"usage event not committed: total=%d records=%d error=%v",
							total,
							len(records),
							queryCtx.Err(),
						)
					case <-ticker.C:
					}
				}
				if parsed.CacheCreationTokens != 15 || parsed.CacheCreation5mInputTokens != 6 ||
					parsed.CacheCreation1hInputTokens != 7 ||
					parsed.TotalTokens != 33 {
					t.Fatalf("partial cumulative usage changed aggregate or TTL values: %+v", parsed)
				}
				receipt := records[0].PricingReceipt
				if receipt == nil {
					t.Fatal("SQLite event did not preserve pricing receipt")
				}
				if receipt.Speed != "standard" || receipt.SpeedSource != "response_speed" {
					t.Fatalf("response speed evidence missing from receipt: %+v", receipt)
				}
				if records[0].Tokens.TotalTokens != 33 || records[0].Cost != 0.0365 ||
					receipt.Coverage.CacheWrite5mTokens != 6 || receipt.Coverage.CacheWrite1hTokens != 7 ||
					receipt.Coverage.CacheWriteUnknownTokens !=
						2 ||
					receipt.Coverage.TotalTokens !=
						33 ||
					receipt.CostMicro !=
						36500 ||
					receipt.Coverage.UncachedInputTokens != 10 || receipt.Coverage.CacheReadTokens != 3 ||
					receipt.Coverage.OutputTokens != 3 || receipt.Coverage.ReasoningTokens != 2 ||
					receipt.Coverage.PricedTokens !=
						33 ||
					receipt.Coverage.UnpricedTokens !=
						0 ||
					receipt.CostStatus !=
						"complete" {
					t.Fatalf(
						"persisted receipt lost known/unknown TTL usage: event=%+v receipt=%+v",
						records[0],
						receipt,
					)
				}
				persister.SetPriceFunc(
					func(string) (float64, float64, float64, bool) {
						return 2000, 4000, 1000, true
					},
				)
				recalculateCtx, cancelRecalculate := context.WithTimeout(ctx, 8*time.Second)
				defer cancelRecalculate()
				if _, err := persister.RecalculateCostsContext(recalculateCtx); err != nil {
					t.Fatalf("recalculate current-catalog cost: %v", err)
				}
				resultCtx, cancelResult := context.WithTimeout(ctx, 8*time.Second)
				defer cancelResult()
				repriced, total, err := persister.QueryEventsContext(
					resultCtx,
					requestedAt.Add(-time.Minute),
					requestedAt.Add(time.Minute),
					persistusage.EventFilters{},
					1,
					10,
					"timestamp",
					false,
				)
				if err != nil || total != 1 || len(repriced) != 1 {
					t.Fatalf("query repriced event: count=%d error=%v", total, err)
				}
				if repriced[0].Cost != 0.073 || !reflect.DeepEqual(repriced[0].PricingReceipt, receipt) {
					t.Fatalf("recalculation changed receipt or lost TTL coverage: %+v", repriced[0])
				}
				t.Logf(
					"persisted tokens=33 cache_write=6/7/2 receipt_micro=36500 " +
						"derived_cost=0.073 receipt_unchanged=true",
				)
			},
		)
	}
}
