package executor

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	internalcache "github.com/Pyrokine/CLIProxyAPI/v6/internal/cache"
	homekv "github.com/Pyrokine/CLIProxyAPI/v6/internal/home"
	log "github.com/sirupsen/logrus"
	logtest "github.com/sirupsen/logrus/hooks/test"
)

func TestAntigravityReasoningReplayCommitReportsConditionalCacheFailure(t *testing.T) {
	for _, kind := range []string{"write", "empty", "overflow"} {
		t.Run(
			kind, func(t *testing.T) {
				const model, session = "gemini-3-flash-agent", "session:private-commit-cache"
				const privateError = "synthetic-private-commit-error"
				item := []byte(`{"type":"function_call_part","contentIndex":1,"partIndex":0,` +
					`"name":"Read","call_id":"id1","args":{"file_path":"/a"},` +
					`"thoughtSignature":"sig-first"}`)
				original, err := json.Marshal([][]byte{item})
				if err != nil {
					t.Fatal(err)
				}
				home := &primaryErrorHome{
					t: t, value: bytes.Clone(original),
					casErr: fmt.Errorf("%s: %w", privateError, homekv.ErrNotConnected),
				}
				ctx := homekv.WithClientScope(
					t.Context(), homekv.ClientScope{
						Client: home, ID: "commit-cache", Enabled: true,
					},
				)
				_, snapshot, found, err := internalcache.GetAntigravityReasoningReplayItemsWithSnapshotRequired(
					ctx, model, session,
				)
				if err != nil || !found {
					t.Fatalf("snapshot = %t, %v", found, err)
				}
				request := []byte(`{"sessionId":"sess-1","request":{"contents":[{"role":"user",` +
					`"parts":[{"text":"hi"}]}]}}`)
				acc := newAntigravityReasoningReplayAccumulator(
					antigravityReasoningReplayScope{
						modelName: model, sessionKey: session, cacheSnapshot: snapshot,
					}, request,
				)
				part := `{"thoughtSignature":"sig-first","functionCall":{"name":"Read",` +
					`"args":{"file_path":"/a"},"id":"id1"}}`
				if kind == "empty" {
					part = `{"text":"ok"}`
				}
				acc.observeResponsePayload(
					[]byte(`{"response":{"candidates":[{"content":{"parts":[` +
						part + `]},"finishReason":"STOP"}]}}`),
				)
				if kind == "overflow" {
					acc.overflow = true
				}
				logger := log.StandardLogger()
				oldLevel, oldHooks := logger.GetLevel(), logger.ReplaceHooks(log.LevelHooks{})
				capture := &logtest.Hook{}
				logger.SetLevel(log.WarnLevel)
				logger.AddHook(capture)
				t.Cleanup(func() { logger.ReplaceHooks(oldHooks); logger.SetLevel(oldLevel) })
				acc.commit(ctx)
				want := 1
				if kind == "write" {
					want = 2
				}
				warnings := 0
				for _, entry := range capture.AllEntries() {
					if strings.Contains(entry.Message, "reasoning replay") && strings.Contains(
						entry.Message, "failed",
					) {
						warnings++
					}
					if strings.Contains(entry.Message, privateError) || strings.Contains(entry.Message, session) ||
						strings.Contains(fmt.Sprint(entry.Data), privateError) {
						t.Fatal("cache diagnostic exposed a private error or session")
					}
				}
				if warnings != want || home.casCalls != want || !bytes.Equal(home.value, original) {
					t.Fatalf(
						"conditional commit: warnings=%d calls=%d want=%d unchanged=%t",
						warnings, home.casCalls, want, bytes.Equal(home.value, original),
					)
				}
			},
		)
	}
}
