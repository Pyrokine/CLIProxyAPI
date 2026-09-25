package session

import (
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	sdktranslator "github.com/Pyrokine/CLIProxyAPI/v6/sdk/translator"
)

func testTurns(values ...string) []CanonicalTurn {
	turns := make([]CanonicalTurn, 0, len(values))
	for _, value := range values {
		turns = append(turns, CanonicalTurn{Role: "user", Parts: []CanonicalPart{{Kind: "text", Value: value}}})
	}
	return turns
}

func TestExtractCanonicalTurnsProtocols(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		format  sdktranslator.Format
		payload string
		roles   []string
	}{
		{
			name:    "openai",
			format:  sdktranslator.FormatOpenAI,
			payload: `{"messages":[{"role":"system","content":"sys"},{"role":"user","content":"hello"}]}`,
			roles:   []string{"system", "user"},
		},
		{
			name:    "claude reasoning ignored",
			format:  sdktranslator.FormatClaude,
			payload: `{"system":"sys","messages":[{"role":"assistant","content":[{"type":"thinking","thinking":"secret"},{"type":"text","text":"answer"}]},{"role":"user","content":"hello"}]}`,
			roles:   []string{"system", "assistant", "user"},
		},
		{
			name:    "responses",
			format:  sdktranslator.FormatOpenAIResponse,
			payload: `{"instructions":"sys","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"hello"}]}]}`,
			roles:   []string{"system", "user"},
		},
		{
			name:    "codex responses",
			format:  sdktranslator.FormatCodex,
			payload: `{"instructions":"sys","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"hello"}]}]}`,
			roles:   []string{"system", "user"},
		},
		{
			name:    "gemini",
			format:  sdktranslator.FormatGemini,
			payload: `{"systemInstruction":{"parts":[{"text":"sys"}]},"contents":[{"role":"user","parts":[{"text":"hello"}]}]}`,
			roles:   []string{"system", "user"},
		},
		{
			name:    "antigravity nested gemini",
			format:  sdktranslator.FormatAntigravity,
			payload: `{"request":{"systemInstruction":{"parts":[{"text":"sys"}]},"contents":[{"role":"user","parts":[{"text":"hello"}]}]}}`,
			roles:   []string{"system", "user"},
		},
		{
			name:    "interactions",
			format:  sdktranslator.FormatInteractions,
			payload: `{"system_instruction":"sys","input":[{"type":"user_input","content":"hello"}]}`,
			roles:   []string{"system", "user"},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			turns := ExtractCanonicalTurns(test.format, []byte(test.payload))
			if len(turns) != len(test.roles) {
				t.Fatalf("turn count = %d, want %d: %+v", len(turns), len(test.roles), turns)
			}
			for index, role := range test.roles {
				if turns[index].Role != role {
					t.Fatalf("turn[%d].Role = %q, want %q", index, turns[index].Role, role)
				}
			}
		})
	}
}

func TestCanonicalReasoningAndParallelToolsAreStable(t *testing.T) {
	t.Parallel()

	firstReasoning := ExtractCanonicalTurns(
		sdktranslator.FormatClaude,
		[]byte(`{"messages":[{"role":"assistant","content":[{"type":"thinking","thinking":"secret-a"},{"type":"text","text":"answer"}]}]}`),
	)
	secondReasoning := ExtractCanonicalTurns(
		sdktranslator.FormatClaude,
		[]byte(`{"messages":[{"role":"assistant","content":[{"type":"thinking","thinking":"secret-b"},{"type":"text","text":"answer"}]}]}`),
	)
	if len(firstReasoning) != 1 || len(secondReasoning) != 1 ||
		FastTurnFingerprint(firstReasoning[0]) != FastTurnFingerprint(secondReasoning[0]) {
		t.Fatalf("reasoning changed canonical fingerprint: %+v vs %+v", firstReasoning, secondReasoning)
	}

	firstTools := ExtractCanonicalTurns(
		sdktranslator.FormatOpenAI,
		[]byte(`{"messages":[{"role":"assistant","tool_calls":[{"id":"b","type":"function","function":{"name":"beta","arguments":"{}"}},{"id":"a","type":"function","function":{"name":"alpha","arguments":"{}"}}]}]}`),
	)
	secondTools := ExtractCanonicalTurns(
		sdktranslator.FormatOpenAI,
		[]byte(`{"messages":[{"role":"assistant","tool_calls":[{"id":"a","type":"function","function":{"name":"alpha","arguments":"{}"}},{"id":"b","type":"function","function":{"name":"beta","arguments":"{}"}}]}]}`),
	)
	if len(firstTools) != 1 || len(secondTools) != 1 ||
		FastTurnFingerprint(firstTools[0]) != FastTurnFingerprint(secondTools[0]) {
		t.Fatalf("parallel tool order changed canonical fingerprint: %+v vs %+v", firstTools, secondTools)
	}
}

func TestCanonicalBoundsAndFullDigest(t *testing.T) {
	t.Parallel()

	parts := make([]map[string]string, maxCanonicalPartsPerTurn+50)
	for index := range parts {
		parts[index] = map[string]string{"text": fmt.Sprintf("part-%d", index)}
	}
	messages := make([]map[string]any, maxCanonicalTurns+50)
	for index := range messages {
		content := any(fmt.Sprintf("turn-%d", index))
		if index == 0 {
			content = parts
		}
		messages[index] = map[string]any{"role": "user", "content": content}
	}
	payload, err := json.Marshal(map[string]any{"messages": messages})
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}
	turns := ExtractCanonicalTurns(sdktranslator.FormatOpenAI, payload)
	if len(turns) != maxCanonicalTurns {
		t.Fatalf("turn count = %d, want %d", len(turns), maxCanonicalTurns)
	}
	if len(turns[0].Parts) > maxCanonicalPartsPerTurn+1 {
		t.Fatalf("part count = %d, want <= %d", len(turns[0].Parts), maxCanonicalPartsPerTurn+1)
	}

	base := strings.Repeat("a", largePartThreshold+8000)
	changed := []byte(base)
	changed[5000] = 'b'
	first := FastTurnFingerprint(CanonicalTurn{Role: "user", Parts: []CanonicalPart{{Kind: "text", Value: base}}})
	second := FastTurnFingerprint(CanonicalTurn{Role: "user", Parts: []CanonicalPart{{Kind: "text", Value: string(changed)}}})
	if first == second {
		t.Fatal("large parts with the same sparse sample collided")
	}
}

func TestMerklePrefixMatcherRollingPrefixAndForkLineage(t *testing.T) {
	t.Parallel()

	matcher := NewMerklePrefixMatcher(time.Hour)
	namespace := "lcp:v1::test::model::caller"
	trunk := testTurns("1", "2", "3", "4", "5", "6", "7", "A")
	root := matcher.BindWithResult(namespace, trunk, "auth-root")
	if root.SessionID == "" || root.ParentSessionID != "" || root.IsFork {
		t.Fatalf("root bind = %+v", root)
	}

	firstFork := testTurns("1", "2", "3", "4", "5", "6", "7", "B")
	match, ok := matcher.Match(namespace, firstFork)
	if !ok || !match.IsFork || match.PrefixLength != 7 || match.ParentSessionID == "" || match.SessionID == root.SessionID {
		t.Fatalf("first fork match = %+v, ok=%v", match, ok)
	}
	boundFork := matcher.BindWithResult(namespace, firstFork, "auth-root")
	if boundFork.SessionID != match.SessionID || boundFork.ParentSessionID != match.ParentSessionID {
		t.Fatalf("fork bind = %+v, match = %+v", boundFork, match)
	}

	nestedFork := testTurns("1", "2", "3", "4", "5", "6", "7", "B", "nested")
	linear := append(testTurns("1", "2", "3", "4", "5", "6", "7", "B"), CanonicalTurn{
		Role: "assistant", Parts: []CanonicalPart{{Kind: "text", Value: "continued"}},
	})
	linearMatch, ok := matcher.Match(namespace, linear)
	if !ok || linearMatch.IsFork || linearMatch.SessionID != boundFork.SessionID {
		t.Fatalf("linear continuation = %+v, ok=%v", linearMatch, ok)
	}
	matcher.BindWithResult(namespace, linear, "auth-root")

	nestedMatch, ok := matcher.Match(namespace, nestedFork)
	if !ok || !nestedMatch.IsFork || nestedMatch.ParentSessionID != boundFork.SessionID {
		t.Fatalf("nested fork = %+v, ok=%v, parent want %q", nestedMatch, ok, boundFork.SessionID)
	}
}

func TestMerklePrefixMatcherRejectsSystemOnlyTrajectory(t *testing.T) {
	t.Parallel()

	matcher := NewMerklePrefixMatcher(time.Hour)
	systemOnly := []CanonicalTurn{{
		Role:  "system",
		Parts: []CanonicalPart{{Kind: "text", Value: "shared instructions"}},
	}}
	if result := matcher.BindWithResult("lcp:v1::test::model::caller", systemOnly, "auth-a"); result.SessionID != "" {
		t.Fatalf("system-only trajectory bound session %q", result.SessionID)
	}
	if match, ok := matcher.Match("lcp:v1::test::model::caller", systemOnly); ok {
		t.Fatalf("system-only trajectory matched: %+v", match)
	}
}

func TestMerklePrefixMatcherRollingPrefixSeparatesSystemPrompts(t *testing.T) {
	t.Parallel()

	matcher := NewMerklePrefixMatcher(time.Hour)
	namespace := "lcp:v1::test::model::caller"
	first := []CanonicalTurn{
		{Role: "system", Parts: []CanonicalPart{{Kind: "text", Value: "tenant A"}}},
		{Role: "user", Parts: []CanonicalPart{{Kind: "text", Value: "same request"}}},
	}
	second := []CanonicalTurn{
		{Role: "system", Parts: []CanonicalPart{{Kind: "text", Value: "tenant B"}}},
		{Role: "user", Parts: []CanonicalPart{{Kind: "text", Value: "same request"}}},
	}
	matcher.Bind(namespace, first, "auth-a")
	if match, ok := matcher.Match(namespace, second); ok {
		t.Fatalf("different system prompt matched: %+v", match)
	}
}

func TestMerklePrefixMatcherLateResultsDoNotChangeReboundBinding(t *testing.T) {
	t.Parallel()

	matcher := NewMerklePrefixMatcher(time.Hour)
	namespace := "lcp:v1::test::model::caller"
	turns := testTurns("one", "two")
	initial := matcher.BindWithResult(namespace, turns, "auth-old")
	rebound := matcher.BindWithResult(namespace, turns, "auth-new")
	if rebound.AccessNumber <= initial.AccessNumber {
		t.Fatalf("generation did not advance: initial=%d rebound=%d", initial.AccessNumber, rebound.AccessNumber)
	}
	fingerprints, minimum := matcher.Prepare(turns)
	if matcher.TouchFingerprints(namespace, fingerprints, minimum, "auth-old") {
		t.Fatal("late success refreshed the old auth")
	}
	if matcher.RemoveFingerprintsBefore(namespace, fingerprints, "auth-old", initial.AccessNumber) {
		t.Fatal("late failure removed a rebound trajectory")
	}
	match, ok := matcher.Match(namespace, turns)
	if !ok || match.AuthID != "auth-new" {
		t.Fatalf("binding = %+v, ok=%v, want auth-new", match, ok)
	}
}

func TestMerklePrefixMatcherGenerationSurvivesClear(t *testing.T) {
	t.Parallel()

	matcher := NewMerklePrefixMatcher(time.Hour)
	namespace := "lcp:v1::test::model::caller"
	turns := testTurns("one", "two")
	before := matcher.BindWithResult(namespace, turns, "auth-a")
	matcher.Clear()
	after := matcher.BindWithResult(namespace, turns, "auth-a")
	if after.AccessNumber <= before.AccessNumber {
		t.Fatalf("generation reset across Clear: before=%d after=%d", before.AccessNumber, after.AccessNumber)
	}
	fingerprints, _ := matcher.Prepare(turns)
	if matcher.RemoveFingerprintsBefore(namespace, fingerprints, "auth-a", before.AccessNumber) {
		t.Fatal("pre-clear result removed post-clear binding")
	}
}

func TestMerklePrefixMatcherEvictsToConfiguredBounds(t *testing.T) {
	t.Parallel()

	matcher := NewMerklePrefixMatcherWithConfig(MerklePrefixMatcherConfig{
		TTL:         time.Hour,
		MaxTurns:    4,
		MaxGroups:   3,
		MaxPrefixes: 6,
	})
	namespace := "lcp:v1::bounded::model::caller"
	sequences := make([][]CanonicalTurn, 0, 5)
	for index := 0; index < 5; index++ {
		turns := testTurns(fmt.Sprintf("root-%d", index), "second", "third")
		sequences = append(sequences, turns)
		matcher.BindWithResult(namespace, turns, fmt.Sprintf("auth-%d", index))
	}

	matcher.mu.Lock()
	groupCount := matcher.groupCount
	prefixCount := matcher.prefixCount
	matcher.mu.Unlock()
	if groupCount > 3 || prefixCount > 6 {
		t.Fatalf("matcher bounds = groups:%d prefixes:%d, want <= 3 and <= 6", groupCount, prefixCount)
	}
	if match, ok := matcher.Match(namespace, sequences[0]); ok {
		t.Fatalf("oldest trajectory remains after eviction: %+v", match)
	}
	if match, ok := matcher.Match(namespace, sequences[len(sequences)-1]); !ok || match.AuthID != "auth-4" {
		t.Fatalf("newest trajectory = %+v, ok=%v", match, ok)
	}
}

func TestMerklePrefixMatcherConcurrentAccess(t *testing.T) {
	t.Parallel()

	matcher := NewMerklePrefixMatcherWithConfig(MerklePrefixMatcherConfig{
		TTL:         time.Hour,
		MaxTurns:    32,
		MaxGroups:   64,
		MaxPrefixes: 512,
	})
	var workers sync.WaitGroup
	for worker := 0; worker < 16; worker++ {
		workers.Add(1)
		go func(worker int) {
			defer workers.Done()
			for iteration := 0; iteration < 200; iteration++ {
				namespace := fmt.Sprintf("namespace-%d", worker%4)
				turns := testTurns("root", fmt.Sprintf("worker-%d", worker), fmt.Sprintf("iteration-%d", iteration))
				result := matcher.BindWithResult(namespace, turns, fmt.Sprintf("auth-%d", worker%3))
				matcher.Match(namespace, turns)
				fingerprints, minimum := matcher.Prepare(turns)
				matcher.TouchFingerprints(namespace, fingerprints, minimum, fmt.Sprintf("auth-%d", worker%3))
				matcher.RemoveFingerprintsBefore(namespace, fingerprints, fmt.Sprintf("auth-%d", worker%3), result.AccessNumber-1)
			}
		}(worker)
	}
	workers.Wait()
}
