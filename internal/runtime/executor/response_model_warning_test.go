package executor

import (
	"fmt"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	cliproxyauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/usage"
	log "github.com/sirupsen/logrus"
	logtest "github.com/sirupsen/logrus/hooks/test"
)

// captureResponseModelWarnings 隔离全局日志与告警缓存，这些测试按顺序执行
func captureResponseModelWarnings(t *testing.T) *logtest.Hook {
	t.Helper()
	logger := log.StandardLogger()
	oldHooks, oldLevel, oldOutput := logger.ReplaceHooks(log.LevelHooks{}), logger.GetLevel(), logger.Out
	hook := &logtest.Hook{}
	logger.AddHook(hook)
	logger.SetLevel(log.WarnLevel)
	logger.SetOutput(io.Discard)
	responseModelWarnings.Lock()
	oldCache := responseModelWarnings.last
	responseModelWarnings.last = make(map[modelWarningKey]time.Time)
	responseModelWarnings.Unlock()
	t.Cleanup(
		func() {
			responseModelWarnings.Lock()
			responseModelWarnings.last = oldCache
			responseModelWarnings.Unlock()
			logger.ReplaceHooks(oldHooks)
			logger.SetLevel(oldLevel)
			logger.SetOutput(oldOutput)
		},
	)
	return hook
}

func responseModelWarningMessages(hook *logtest.Hook) []string {
	var messages []string
	for _, entry := range hook.AllEntries() {
		if entry.Level == log.WarnLevel && strings.Contains(entry.Message, "upstream served model") {
			messages = append(messages, entry.Message)
		}
	}
	return messages
}

func TestUsageReporterModelWarningsUseFourDimensionsAndNormalizedModels(t *testing.T) {
	base := modelWarningKey{"codex", "codex-auth-1", "gpt-6-astra", "gpt-5.6-luna"}
	for _, test := range []struct {
		name string
		key  modelWarningKey
		want int
	}{
		{"same dimensions", base, 1},
		{"served case", modelWarningKey{"codex", "codex-auth-1", "gpt-6-astra", "GPT-5.6-LUNA"}, 1},
		{"requested case and suffix", modelWarningKey{"codex", "codex-auth-1", "GPT-6-ASTRA(high)", "gpt-5.6-luna"}, 1},
		{"provider", modelWarningKey{"xai", "codex-auth-1", "gpt-6-astra", "gpt-5.6-luna"}, 2},
		{"credential", modelWarningKey{"codex", "codex-auth-2", "gpt-6-astra", "gpt-5.6-luna"}, 2},
		{"requested model", modelWarningKey{"codex", "codex-auth-1", "gpt-6-sol", "gpt-5.6-luna"}, 2},
		{"served model", modelWarningKey{"codex", "codex-auth-1", "gpt-6-astra", "gpt-6-sol"}, 2},
	} {
		t.Run(
			test.name, func(t *testing.T) {
				hook := captureResponseModelWarnings(t)
				ctx, finishUsage := xaiOutcomeContext(t)
				for _, key := range []modelWarningKey{base, test.key} {
					reporter := &usageReporter{
						provider: key.provider, authID: key.authID, authIndex: "auth-index-7",
						model: key.expected, responseModel: key.served,
					}
					reporter.publish(ctx, usage.Detail{TotalTokens: 3})
					reporter.ensurePublished(ctx)
				}
				if messages := responseModelWarningMessages(hook); len(messages) != test.want {
					t.Fatalf("warnings = %#v, want %d", messages, test.want)
				}
				if records := finishUsage(); len(records) != 2 {
					t.Fatalf("usage records = %d, want one for each request", len(records))
				}
			},
		)
	}
}

func TestUsageReporterModelWarningStartsAtPublication(t *testing.T) {
	hook := captureResponseModelWarnings(t)
	ctx, finishUsage := xaiOutcomeContext(t)
	auth := &cliproxyauth.Auth{
		ID: "codex-auth-1", Index: "auth-index-7", Provider: "codex",
		FileName: "/auths/codex-user@example.com.json",
	}
	reporter := newUsageReporter(ctx, "codex", "gpt-6-astra", auth)
	for _, payload := range []string{
		`data: {"type":"response.created","response":{"id":"resp_1","model":"gpt-5.6-luna"}}`,
		`data: {"type":"response.in_progress","response":{"id":"resp_1","model":"gpt-5.6-luna"}}`,
		`data: {"type":"response.output_text.delta","delta":"hello"}`,
		`data: {"type":"response.completed","response":{"id":"resp_1","model":"gpt-5.6-luna"}}`,
		`data: [DONE]`,
	} {
		reporter.observeResponseModel([]byte(payload))
	}
	if messages := responseModelWarningMessages(hook); len(messages) != 0 {
		t.Fatalf("observations emitted a warning before publication: %#v", messages)
	}
	reporter.publish(ctx, usage.Detail{InputTokens: 5, OutputTokens: 7, TotalTokens: 12})
	reporter.ensurePublished(ctx)
	messages := responseModelWarningMessages(hook)
	want := `codex executor: upstream served model "gpt-5.6-luna" for requested model "gpt-6-astra" ` +
		`(auth_index=auth-index-7)`
	if len(messages) != 1 || messages[0] != want {
		t.Fatalf("warnings = %#v, want %q", messages, want)
	}
	if strings.Contains(messages[0], "example.com") || strings.Contains(messages[0], "auth_file") {
		t.Fatalf("warning exposed credential filename: %q", messages[0])
	}
	records := finishUsage()
	if len(records) != 1 {
		t.Fatalf("usage records = %d, want 1", len(records))
	}
	if record := records[0]; record.Model != "gpt-6-astra" || record.ResponseModel != "gpt-5.6-luna" ||
		record.AuthIndex != "auth-index-7" || record.Detail.TotalTokens != 12 || record.Failed {
		t.Fatalf("published response model record = %#v", record)
	}
}

func TestUsageReporterDoesNotWarnWhenCodexResponseModelMatches(t *testing.T) {
	hook := captureResponseModelWarnings(t)
	ctx, finishUsage := xaiOutcomeContext(t)
	reporter := newUsageReporter(ctx, "codex", "gpt-6-astra(high)", nil)
	reporter.observeResponseModel(
		[]byte(
			`data: {"type":"response.created","response":{"model":"gpt-6-astra-2026-05-13"}}`,
		),
	)
	reporter.publish(ctx, usage.Detail{TotalTokens: 3})
	if served, _ := reporter.responseModelSnapshot(); served != "gpt-6-astra-2026-05-13" {
		t.Fatalf("response model = %q, want gpt-6-astra-2026-05-13", served)
	}
	if messages := responseModelWarningMessages(hook); len(messages) != 0 {
		t.Fatalf("matching model warnings = %#v, want none", messages)
	}
	if records := finishUsage(); len(records) != 1 {
		t.Fatalf("usage records = %d, want 1", len(records))
	}
}

// noinspection GoBoolExpressions
func TestUsageReporterModelWarningExpiresAfterTenMinutes(t *testing.T) {
	hook := captureResponseModelWarnings(t)
	if modelWarningInterval != 10*time.Minute {
		t.Fatalf("warning interval = %s, want 10 minutes", modelWarningInterval)
	}
	ctx, finishUsage := xaiOutcomeContext(t)
	key := modelWarningKey{"codex", "codex-auth-1", "gpt-6-astra", "gpt-5.6-luna"}
	for index, age := range []time.Duration{0, 9 * time.Minute, 10*time.Minute + time.Second} {
		if index > 0 {
			responseModelWarnings.Lock()
			responseModelWarnings.last[key] = time.Now().Add(-age)
			responseModelWarnings.Unlock()
		}
		reporter := &usageReporter{
			provider: key.provider, authID: key.authID, model: key.expected,
			responseModel: key.served,
		}
		reporter.publish(ctx, usage.Detail{TotalTokens: 3})
		want := 1
		if index == 2 {
			want = 2
		}
		if messages := responseModelWarningMessages(hook); len(messages) != want {
			t.Fatalf("age %s: warnings = %#v, want %d", age, messages, want)
		}
	}
	if records := finishUsage(); len(records) != 3 {
		t.Fatalf("usage records = %d, want 3", len(records))
	}
}

// noinspection GoBoolExpressions
func TestUsageReporterModelWarningCacheBoundsAndExpiredEntries(t *testing.T) {
	for _, expired := range []bool{false, true} {
		t.Run(
			fmt.Sprintf("expired=%t", expired), func(t *testing.T) {
				hook := captureResponseModelWarnings(t)
				if modelWarningLimit != 1024 {
					t.Fatalf("warning limit = %d, want 1024", modelWarningLimit)
				}
				now := time.Now()
				responseModelWarnings.Lock()
				for index := range 1024 {
					key := modelWarningKey{"codex", fmt.Sprint(index), "gpt-6-astra", "gpt-5.6-luna"}
					last := now
					if expired && index < 512 {
						last = now.Add(-10*time.Minute - time.Second)
					}
					responseModelWarnings.last[key] = last
				}
				responseModelWarnings.Unlock()
				ctx, finishUsage := xaiOutcomeContext(t)
				reporter := &usageReporter{
					provider: "codex", authID: "new-auth", model: "gpt-6-astra",
					responseModel: "gpt-5.6-luna",
				}
				reporter.publish(ctx, usage.Detail{TotalTokens: 3})
				responseModelWarnings.Lock()
				count := len(responseModelWarnings.last)
				_, retained := responseModelWarnings.last[modelWarningKey{
					"codex", "1023", "gpt-6-astra", "gpt-5.6-luna",
				}]
				_, inserted := responseModelWarnings.last[modelWarningKey{
					"codex", "new-auth", "gpt-6-astra", "gpt-5.6-luna",
				}]
				responseModelWarnings.Unlock()
				want := 1
				if expired {
					want = 513
				}
				if count != want || retained != expired || !inserted {
					t.Fatalf(
						"cache size=%d retained=%t inserted=%t, want %d/%t/true", count, retained, inserted, want,
						expired,
					)
				}
				if messages := responseModelWarningMessages(hook); len(messages) != 1 {
					t.Fatalf("capacity cleanup warnings = %#v, want 1", messages)
				}
				if records := finishUsage(); len(records) != 1 {
					t.Fatalf("usage records = %d, want 1", len(records))
				}
			},
		)
	}
}

func TestUsageReporterWarnsOnceUnderConcurrentObservationsAndPublishes(t *testing.T) {
	for _, independent := range []bool{false, true} {
		t.Run(
			fmt.Sprintf("independent_requests=%t", independent), func(t *testing.T) {
				hook := captureResponseModelWarnings(t)
				const workers = 32
				manager := usage.NewManager(workers)
				capture := &captureUsagePlugin{records: make(chan usage.Record, 2*workers)}
				manager.Register(capture)
				manager.Start(t.Context())
				t.Cleanup(manager.Stop)
				ctx := usage.WithManager(t.Context(), manager)
				auth := &cliproxyauth.Auth{ID: "codex-auth-1", Index: "auth-index-7", Provider: "codex"}
				shared := newUsageReporter(ctx, "codex", "gpt-6-astra", auth)
				created := []byte(`data: {"type":"response.created","response":{"model":"gpt-5.6-luna"}}`)
				completed := []byte(`data: {"type":"response.completed","response":{"model":"gpt-5.6-luna",` +
					`"usage":{"total_tokens":4}}}`)
				start := make(chan struct{})
				var wg sync.WaitGroup
				for worker := range workers {
					wg.Go(
						func() {
							<-start
							reporter := shared
							if independent {
								reporter = newUsageReporter(ctx, "codex", "gpt-6-astra", auth)
							}
							if worker%2 == 0 {
								reporter.observeResponseModel(created)
							} else {
								reporter.observeResponseModel(completed)
							}
							if independent {
								reporter.publish(ctx, usage.Detail{TotalTokens: int64(worker + 1)})
							}
							reporter.ensurePublished(ctx)
						},
					)
				}
				close(start)
				wg.Wait()
				manager.Stop()
				want := 1
				if independent {
					want = workers
				}
				if len(capture.records) != want {
					t.Fatalf("usage records = %d, want %d", len(capture.records), want)
				}
				seen := make(map[int64]bool)
				for range want {
					record := <-capture.records
					if record.Provider != "codex" || record.AuthID != auth.ID || record.Failed ||
						record.Model != "gpt-6-astra" || record.ResponseModel != "gpt-5.6-luna" {
						t.Fatalf("concurrent record = %#v", record)
					}
					if independent && (record.Detail.TotalTokens < 1 || record.Detail.TotalTokens > workers ||
						seen[record.Detail.TotalTokens]) {
						t.Fatalf("request usage was lost or duplicated: %#v", record)
					}
					seen[record.Detail.TotalTokens] = true
				}
				if messages := responseModelWarningMessages(hook); len(messages) != 1 {
					t.Fatalf("concurrent warnings = %#v, want 1", messages)
				}
			},
		)
	}
}
