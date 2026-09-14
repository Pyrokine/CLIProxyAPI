package cache

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	homekv "github.com/Pyrokine/CLIProxyAPI/v6/internal/home"
)

func claudeReplayTestContent(label string) []byte {
	return []byte(fmt.Sprintf(
		`[{"type":"thinking","thinking":%q,"signature":%q},{"type":"tool_use","id":%q,"name":"shell","input":{"value":%q}}]`,
		label,
		"sig-"+label,
		"tool-"+label,
		label,
	))
}

func claudeReplayLargeTestContent(label string, size int) []byte {
	return []byte(fmt.Sprintf(
		`[{"type":"thinking","thinking":%q,"signature":%q},{"type":"tool_use","id":%q,"name":"shell","input":{}}]`,
		label+strings.Repeat("x", size),
		"sig-"+label,
		"tool-"+label,
	))
}

func readClaudeReplay(
	t *testing.T,
	ctx context.Context,
	model,
	session string,
) ([][]byte, ClaudeThinkingReplaySnapshot, bool) {
	t.Helper()
	contents, snapshot, found, errGet := GetClaudeThinkingReplayWithSnapshotRequired(ctx, model, session)
	if errGet != nil {
		t.Fatalf("GetClaudeThinkingReplayWithSnapshotRequired() error = %v", errGet)
	}
	return contents, snapshot, found
}

func TestClaudeThinkingReplayLocalMultiTurnAndCopyIsolation(t *testing.T) {
	ClearClaudeThinkingReplayCache()
	t.Cleanup(ClearClaudeThinkingReplayCache)
	ctx := localKimiReplayContext("claude-runtime-a")
	const model, session = "claude:auth:model", "execution:multi-turn"

	_, snapshot, found := readClaudeReplay(t, ctx, model, session)
	if found {
		t.Fatal("initial replay read found content")
	}
	first := claudeReplayTestContent("first")
	written, errReplace := ReplaceClaudeThinkingReplayIfUnchanged(ctx, model, session, snapshot, first)
	if errReplace != nil || !written {
		t.Fatalf("first replace = %v, %v", written, errReplace)
	}
	first[0] = '!'

	contents, snapshot, found := readClaudeReplay(t, ctx, model, session)
	if !found || len(contents) != 1 || !bytes.Equal(contents[0], claudeReplayTestContent("first")) {
		t.Fatalf("first replay = %q, found %v", contents, found)
	}
	contents[0][0] = '!'
	second := claudeReplayTestContent("second")
	written, errReplace = ReplaceClaudeThinkingReplayIfUnchanged(ctx, model, session, snapshot, second)
	if errReplace != nil || !written {
		t.Fatalf("second replace = %v, %v", written, errReplace)
	}

	contents, snapshot, found = readClaudeReplay(t, ctx, model, session)
	if !found || len(contents) != 2 ||
		!bytes.Equal(contents[0], claudeReplayTestContent("first")) ||
		!bytes.Equal(contents[1], second) {
		t.Fatalf("multi-turn replay = %q, found %v", contents, found)
	}
	written, errReplace = ReplaceClaudeThinkingReplayIfUnchanged(
		ctx,
		model,
		session,
		snapshot,
		claudeReplayTestContent("first"),
	)
	if errReplace != nil || !written {
		t.Fatalf("duplicate replace = %v, %v", written, errReplace)
	}
	contents, _, found = readClaudeReplay(t, ctx, model, session)
	if !found || len(contents) != 2 {
		t.Fatalf("duplicate replay turns = %d, found %v; want 2", len(contents), found)
	}
}

func TestClaudeThinkingReplayRejectsStaleMutationAndABA(t *testing.T) {
	ClearClaudeThinkingReplayCache()
	t.Cleanup(ClearClaudeThinkingReplayCache)
	ctx := localKimiReplayContext("claude-runtime-a")
	const model, session = "claude:auth:model", "execution:stale"

	_, firstSnapshot, _ := readClaudeReplay(t, ctx, model, session)
	_, staleSnapshot, _ := readClaudeReplay(t, ctx, model, session)
	content := claudeReplayTestContent("same")
	written, errReplace := ReplaceClaudeThinkingReplayIfUnchanged(
		ctx,
		model,
		session,
		firstSnapshot,
		content,
	)
	if errReplace != nil || !written {
		t.Fatalf("initial replace = %v, %v", written, errReplace)
	}
	written, errReplace = ReplaceClaudeThinkingReplayIfUnchanged(
		ctx,
		model,
		session,
		staleSnapshot,
		claudeReplayTestContent("stale"),
	)
	if errReplace != nil || written {
		t.Fatalf("stale replace = %v, %v; want rejected", written, errReplace)
	}

	_, currentSnapshot, found := readClaudeReplay(t, ctx, model, session)
	if !found {
		t.Fatal("current replay content missing")
	}
	deleted, errDelete := DeleteClaudeThinkingReplayIfUnchanged(ctx, model, session, currentSnapshot)
	if errDelete != nil || !deleted {
		t.Fatalf("delete = %v, %v", deleted, errDelete)
	}
	_, tombstoneSnapshot, found := readClaudeReplay(t, ctx, model, session)
	if found {
		t.Fatal("deleted replay remained visible")
	}
	written, errReplace = ReplaceClaudeThinkingReplayIfUnchanged(
		ctx,
		model,
		session,
		tombstoneSnapshot,
		content,
	)
	if errReplace != nil || !written {
		t.Fatalf("ABA restore = %v, %v", written, errReplace)
	}
	deleted, errDelete = DeleteClaudeThinkingReplayIfUnchanged(ctx, model, session, currentSnapshot)
	if errDelete != nil || deleted {
		t.Fatalf("pre-ABA snapshot delete = %v, %v; want rejected", deleted, errDelete)
	}
	contents, _, found := readClaudeReplay(t, ctx, model, session)
	if !found || len(contents) != 1 || !bytes.Equal(contents[0], content) {
		t.Fatalf("ABA replay = %q, found %v", contents, found)
	}
}

func TestClaudeThinkingReplayConcurrentReplaceHasOneWinner(t *testing.T) {
	ClearClaudeThinkingReplayCache()
	t.Cleanup(ClearClaudeThinkingReplayCache)
	ctx := localKimiReplayContext("claude-runtime-a")
	const model, session = "claude:auth:model", "execution:concurrent"
	_, snapshot, _ := readClaudeReplay(t, ctx, model, session)

	var winners atomic.Int32
	var failures atomic.Int32
	var wait sync.WaitGroup
	for _, label := range []string{"first", "second"} {
		wait.Add(1)
		go func(label string) {
			defer wait.Done()
			written, errReplace := ReplaceClaudeThinkingReplayIfUnchanged(
				ctx,
				model,
				session,
				snapshot,
				claudeReplayTestContent(label),
			)
			if errReplace != nil {
				failures.Add(1)
				return
			}
			if written {
				winners.Add(1)
			}
		}(label)
	}
	wait.Wait()
	if failures.Load() != 0 || winners.Load() != 1 {
		t.Fatalf("concurrent replace = %d winners, %d failures; want 1, 0", winners.Load(), failures.Load())
	}
}

func TestClaudeThinkingReplayLimitsAndPurge(t *testing.T) {
	contents := make([][]byte, 0, claudeThinkingReplayCacheMaxTurnsPerSession+1)
	for index := 0; index <= claudeThinkingReplayCacheMaxTurnsPerSession; index++ {
		contents = appendClaudeThinkingReplayContent(contents, claudeReplayTestContent(fmt.Sprintf("turn-%03d", index)))
	}
	if len(contents) != claudeThinkingReplayCacheMaxTurnsPerSession {
		t.Fatalf("turn count = %d, want %d", len(contents), claudeThinkingReplayCacheMaxTurnsPerSession)
	}
	if bytes.Equal(contents[0], claudeReplayTestContent("turn-000")) ||
		!bytes.Equal(contents[len(contents)-1], claudeReplayTestContent("turn-064")) {
		t.Fatalf("turn limit did not remove the oldest content")
	}

	contents = nil
	for _, label := range []string{"large-a", "large-b", "large-c"} {
		contents = appendClaudeThinkingReplayContent(contents, claudeReplayLargeTestContent(label, 3<<20))
	}
	if len(contents) != 2 || claudeThinkingReplayEntryBytes(contents) > claudeThinkingReplayCacheMaxBytesPerSession {
		t.Fatalf("byte-limited contents = %d turns, %d bytes", len(contents), claudeThinkingReplayEntryBytes(contents))
	}

	var blocks strings.Builder
	blocks.WriteByte('[')
	for index := 0; index <= claudeThinkingReplayCacheMaxBlocksPerTurn; index++ {
		if index > 0 {
			blocks.WriteByte(',')
		}
		blocks.WriteString(`{"type":"text","text":"x"}`)
	}
	blocks.WriteByte(']')
	if validClaudeThinkingReplayContent([]byte(blocks.String())) {
		t.Fatal("content above the block limit was accepted")
	}

	ClearClaudeThinkingReplayCache()
	now := time.Now()
	claudeThinkingReplayMu.Lock()
	for index := 0; index <= claudeThinkingReplayCacheMaxEntries; index++ {
		key := fmt.Sprintf("entry-%05d", index)
		claudeThinkingReplayEntries[key] = claudeThinkingReplayEntry{
			Timestamp:  now.Add(time.Duration(index) * time.Nanosecond),
			Generation: key,
			Deleted:    true,
		}
	}
	enforceClaudeThinkingReplayLimitsLocked()
	entryCount := len(claudeThinkingReplayEntries)
	_, oldestFound := claudeThinkingReplayEntries["entry-00000"]
	_, newestFound := claudeThinkingReplayEntries[fmt.Sprintf("entry-%05d", claudeThinkingReplayCacheMaxEntries)]
	claudeThinkingReplayMu.Unlock()
	if entryCount > claudeThinkingReplayCacheMaxEntries || oldestFound || !newestFound {
		t.Fatalf("entry eviction = count %d, oldest %v, newest %v", entryCount, oldestFound, newestFound)
	}

	ClearClaudeThinkingReplayCache()
	expiredContent := claudeReplayTestContent("expired")
	claudeThinkingReplayMu.Lock()
	claudeThinkingReplayEntries["expired"] = claudeThinkingReplayEntry{
		Contents:   [][]byte{expiredContent},
		Timestamp:  now.Add(-claudeThinkingReplayCacheTTL - time.Second),
		Generation: "expired",
	}
	claudeThinkingReplayTotalBytes = len(expiredContent)
	claudeThinkingReplayMu.Unlock()
	purgeExpiredClaudeThinkingReplayCache(now)
	claudeThinkingReplayMu.Lock()
	_, expiredFound := claudeThinkingReplayEntries["expired"]
	totalBytes := claudeThinkingReplayTotalBytes
	claudeThinkingReplayMu.Unlock()
	if expiredFound || totalBytes != 0 {
		t.Fatalf("expired replay = found %v, total bytes %d", expiredFound, totalBytes)
	}
}

func TestClaudeThinkingReplayRuntimeScopeAndHomeBehavior(t *testing.T) {
	ClearClaudeThinkingReplayCache()
	t.Cleanup(ClearClaudeThinkingReplayCache)
	const model, session = "claude:auth:model", "execution:scope"
	localA := localKimiReplayContext("claude-runtime-a")
	localB := localKimiReplayContext("claude-runtime-b")
	_, snapshot, _ := readClaudeReplay(t, localA, model, session)
	written, errReplace := ReplaceClaudeThinkingReplayIfUnchanged(
		localA,
		model,
		session,
		snapshot,
		claudeReplayTestContent("local"),
	)
	if errReplace != nil || !written {
		t.Fatalf("local replace = %v, %v", written, errReplace)
	}
	if _, _, found := readClaudeReplay(t, localB, model, session); found {
		t.Fatal("local replay crossed runtime scopes")
	}

	client := newFakeKimiThinkingReplayKVClient()
	homeA := homeKimiReplayContext("claude-home-a", client)
	homeB := homeKimiReplayContext("claude-home-b", client)
	_, snapshot, _ = readClaudeReplay(t, homeA, model, session)
	written, errReplace = ReplaceClaudeThinkingReplayIfUnchanged(
		homeA,
		model,
		session,
		snapshot,
		claudeReplayTestContent("home"),
	)
	if errReplace != nil || !written {
		t.Fatalf("home replace = %v, %v", written, errReplace)
	}
	contents, _, found := readClaudeReplay(t, homeB, model, session)
	if !found || len(contents) != 1 || !bytes.Equal(contents[0], claudeReplayTestContent("home")) {
		t.Fatalf("home replay = %q, found %v", contents, found)
	}

	unavailableHome := homekv.WithClientScope(
		context.Background(), homekv.ClientScope{
			ID:      "claude-home-unavailable",
			Enabled: true,
		},
	)
	contents, _, found, errGet := GetClaudeThinkingReplayWithSnapshotRequired(
		unavailableHome,
		model,
		session,
	)
	if errGet == nil || found || len(contents) != 0 {
		t.Fatalf("unavailable Home read = %q, found %v, error %v", contents, found, errGet)
	}
}

func TestClaudeThinkingReplayHomeCASAndFailures(t *testing.T) {
	ClearClaudeThinkingReplayCache()
	t.Cleanup(ClearClaudeThinkingReplayCache)
	client := newFakeKimiThinkingReplayKVClient()
	ctx := homeKimiReplayContext("claude-home", client)
	const model, session = "claude:auth:model", "execution:home-cas"

	_, firstSnapshot, _ := readClaudeReplay(t, ctx, model, session)
	_, staleSnapshot, _ := readClaudeReplay(t, ctx, model, session)
	written, errReplace := ReplaceClaudeThinkingReplayIfUnchanged(
		ctx,
		model,
		session,
		firstSnapshot,
		claudeReplayTestContent("current"),
	)
	if errReplace != nil || !written {
		t.Fatalf("home replace = %v, %v", written, errReplace)
	}
	written, errReplace = ReplaceClaudeThinkingReplayIfUnchanged(
		ctx,
		model,
		session,
		staleSnapshot,
		claudeReplayTestContent("stale"),
	)
	if errReplace != nil || written {
		t.Fatalf("stale Home replace = %v, %v; want rejected", written, errReplace)
	}

	client.getErr = errors.New("get failed")
	if _, _, _, errGet := GetClaudeThinkingReplayWithSnapshotRequired(ctx, model, session); errGet == nil {
		t.Fatal("Home get failure was ignored")
	}
	client.getErr = nil
	client.expireErr = errors.New("expire failed")
	if _, _, _, errGet := GetClaudeThinkingReplayWithSnapshotRequired(ctx, model, session); errGet == nil {
		t.Fatal("Home expire failure was ignored")
	}
	client.expireErr = nil
	_, snapshot, _ := readClaudeReplay(t, ctx, model, session)
	client.casErr = errors.New("cas failed")
	if _, errReplace = ReplaceClaudeThinkingReplayIfUnchanged(
		ctx,
		model,
		session,
		snapshot,
		claudeReplayTestContent("next"),
	); errReplace == nil {
		t.Fatal("Home CAS failure was ignored")
	}
	client.casErr = nil

	client.mu.Lock()
	client.values[claudeThinkingReplayKVKey(
		model, "invalid",
	)] = []byte(`{"generation":"g","contents":[{"invalid":true}]}`)
	client.mu.Unlock()
	if _, _, _, errGet := GetClaudeThinkingReplayWithSnapshotRequired(ctx, model, "invalid"); errGet == nil {
		t.Fatal("invalid Home replay value was accepted")
	}
}

func TestClearClaudeThinkingReplayDoesNotClearKimiState(t *testing.T) {
	ClearClaudeThinkingReplayCache()
	ClearKimiThinkingReplayCache()
	t.Cleanup(ClearClaudeThinkingReplayCache)
	t.Cleanup(ClearKimiThinkingReplayCache)
	ctx := localKimiReplayContext("shared-runtime")
	const model, session = "shared-model", "execution:shared"
	content := claudeReplayTestContent("shared")

	_, kimiSnapshot, _, errKimi := GetKimiThinkingReplayWithSnapshotRequired(ctx, model, session)
	if errKimi != nil {
		t.Fatal(errKimi)
	}
	if written, errReplace := ReplaceKimiThinkingReplayIfUnchanged(
		ctx,
		model,
		session,
		kimiSnapshot,
		content,
	); errReplace != nil || !written {
		t.Fatalf("Kimi seed = %v, %v", written, errReplace)
	}
	_, claudeSnapshot, _ := readClaudeReplay(t, ctx, model, session)
	if written, errReplace := ReplaceClaudeThinkingReplayIfUnchanged(
		ctx,
		model,
		session,
		claudeSnapshot,
		content,
	); errReplace != nil || !written {
		t.Fatalf("Claude seed = %v, %v", written, errReplace)
	}

	ClearClaudeThinkingReplayCache()
	gotKimi, _, foundKimi, errKimi := GetKimiThinkingReplayWithSnapshotRequired(ctx, model, session)
	if errKimi != nil || !foundKimi || !bytes.Equal(gotKimi, content) {
		t.Fatalf("Kimi replay after Claude clear = %q, found %v, error %v", gotKimi, foundKimi, errKimi)
	}
	if gotClaude, _, foundClaude := readClaudeReplay(t, ctx, model, session); foundClaude || len(gotClaude) != 0 {
		t.Fatalf("Claude replay after clear = %q, found %v", gotClaude, foundClaude)
	}
}
