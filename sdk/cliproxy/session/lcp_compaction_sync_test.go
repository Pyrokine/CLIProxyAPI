package session

import (
	"fmt"
	"net/http"
	"testing"
	"time"

	cliproxyexecutor "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
)

func TestMerklePrefixMatcherCompactionContinuationAndFork(t *testing.T) {
	t.Parallel()

	matcher := NewMerklePrefixMatcher(time.Hour)
	const namespace = "lcp:v1::provider::model::caller"
	parent := testTurns("one", "two", "three", "four", "five")
	root := matcher.BindWithResult(namespace, parent, "auth-a")
	compacted := testTurns("summary of one to three", "four", "five", "six")
	match, ok := matcher.Match(namespace, compacted)
	if !ok || match.AuthID != "auth-a" || match.ParentSessionID != root.SessionID ||
		!match.IsCompaction || match.IsFork || match.NodeKind != "compaction" || match.SessionID == root.SessionID {
		t.Fatalf("compaction match = %+v, ok=%v; root=%+v", match, ok, root)
	}
	bound := matcher.BindWithResult(namespace, compacted, "auth-a")
	if !bound.IsCompaction || bound.IsFork || bound.NodeKind != "compaction" || bound.SessionID != match.SessionID ||
		bound.ParentSessionID != root.SessionID {
		t.Fatalf("compaction bind = %+v, match=%+v", bound, match)
	}
	continued := testTurns("summary of one to three", "four", "five", "six", "seven")
	if next, nextOK := matcher.Match(namespace, continued); !nextOK || next.IsFork || !next.IsCompaction ||
		next.SessionID != bound.SessionID {
		t.Fatalf("continuation = %+v, ok=%v", next, nextOK)
	}
	fork := testTurns("one", "two", "three", "four", "different")
	if branch, branchOK := matcher.Match(namespace, fork); !branchOK || !branch.IsFork || branch.IsCompaction ||
		branch.NodeKind != "fork" {
		t.Fatalf("branch = %+v, ok=%v", branch, branchOK)
	}
}

func TestMerklePrefixMatcherResolveDoesNotBindCredential(t *testing.T) {
	t.Parallel()

	matcher := NewMerklePrefixMatcher(time.Hour)
	const namespace = "lcp:v1::provider::model::caller"
	fingerprints, minimum, tails, envDigest := matcher.PrepareExt(testTurns("one", "two", "three"))
	resolved, ok := matcher.ResolveFingerprintsWithContext(namespace, fingerprints, tails, envDigest, minimum)
	if !ok || resolved.SessionID == "" || resolved.AuthID != "" {
		t.Fatalf("resolved session = %+v, ok = %t", resolved, ok)
	}
	if authIDs, _, found := matcher.LookupSession(resolved.SessionID); found || len(authIDs) != 0 {
		t.Fatalf("resolved session created auth bindings: %v", authIDs)
	}
	bound := matcher.BindFingerprintsWithContext(namespace, fingerprints, tails, envDigest, minimum, "auth-a")
	if bound.SessionID != resolved.SessionID {
		t.Fatalf("bound session = %q, resolved = %q", bound.SessionID, resolved.SessionID)
	}
}

func TestMerklePrefixMatcherHomeCompletionPreservesNewerSelection(t *testing.T) {
	t.Parallel()

	matcher := NewMerklePrefixMatcher(time.Hour)
	const namespace = "lcp:v1::provider::model::caller"
	parentFingerprints, minimum, tails, envDigest := matcher.PrepareExt(testTurns("one", "two", "three"))
	first := matcher.BindFingerprintsWithContext(namespace, parentFingerprints, tails, envDigest, minimum, "auth-a")
	continuedFingerprints, nextMinimum, nextTails, nextEnv := matcher.PrepareExt(testTurns("one", "two", "three", "four"))
	second := matcher.BindFingerprintsWithContext(namespace, continuedFingerprints, nextTails, nextEnv, nextMinimum, "auth-b")
	if second.SessionID != first.SessionID {
		t.Fatalf("continued session = %+v, first = %+v", second, first)
	}
	if matcher.CompleteSessionSelection(namespace, parentFingerprints, first.SessionID, "auth-a", first.AccessNumber) {
		t.Fatal("stale completion displaced the newer Home selection")
	}
	if !matcher.CompleteSessionSelection(namespace, continuedFingerprints, second.SessionID, "auth-b", second.AccessNumber) {
		t.Fatal("current Home selection was not completed")
	}
	if authIDs, _, found := matcher.LookupSession(first.SessionID); !found || len(authIDs) != 1 || authIDs[0] != "auth-b" {
		t.Fatalf("completed session bindings = %v, found = %t", authIDs, found)
	}
}

func TestMerklePrefixMatcherCompactionSlidingWindowAndSystemIsolation(t *testing.T) {
	t.Parallel()

	matcher := NewMerklePrefixMatcher(time.Hour)
	const namespace = "lcp:v1::provider::model::caller"
	parent := []CanonicalTurn{
		{Role: "developer", Parts: []CanonicalPart{{Kind: "text", Value: "instruction A"}}},
		{Role: "user", Parts: []CanonicalPart{{Kind: "text", Value: "one"}}},
		{Role: "assistant", Parts: []CanonicalPart{{Kind: "text", Value: "two"}}},
		{Role: "user", Parts: []CanonicalPart{{Kind: "text", Value: "three"}}},
		{Role: "assistant", Parts: []CanonicalPart{{Kind: "text", Value: "four"}}},
	}
	root := matcher.BindWithResult(namespace, parent, "auth-a")
	truncated := append([]CanonicalTurn{{Role: "developer", Parts: parent[0].Parts}}, parent[3:]...)
	truncated = append(truncated, CanonicalTurn{Role: "user", Parts: []CanonicalPart{{Kind: "text", Value: "five"}}})
	if match, ok := matcher.Match(namespace, truncated); !ok || !match.IsCompaction || match.ParentSessionID != root.SessionID {
		t.Fatalf("truncated continuation = %+v, ok=%v", match, ok)
	}
	otherSystem := append([]CanonicalTurn{{Role: "developer", Parts: []CanonicalPart{{Kind: "text", Value: "instruction B"}}}}, truncated[1:]...)
	if match, ok := matcher.Match(namespace, otherSystem); ok {
		t.Fatalf("different developer instruction matched: %+v", match)
	}
	withoutSystem := truncated[1:]
	if match, ok := matcher.Match(namespace, withoutSystem); ok {
		t.Fatalf("missing developer instruction matched: %+v", match)
	}
}

func TestMerklePrefixMatcherCompactionRejectsAmbiguousParents(t *testing.T) {
	t.Parallel()

	matcher := NewMerklePrefixMatcher(time.Hour)
	const namespace = "lcp:v1::provider::model::caller"
	matcher.Bind(namespace, testTurns("parent-a", "earlier-a", "shared-one", "shared-two"), "auth-a")
	matcher.Bind(namespace, testTurns("parent-b", "earlier-b", "shared-one", "shared-two"), "auth-b")
	if match, ok := matcher.Match(namespace, testTurns("summary", "shared-one", "shared-two", "next")); ok {
		t.Fatalf("ambiguous compaction selected a credential: %+v", match)
	}
}

func TestMerklePrefixMatcherCompactionPrefersCurrentAuthForSameSession(t *testing.T) {
	t.Parallel()

	matcher := NewMerklePrefixMatcher(time.Hour)
	const namespace = "lcp:v1::provider::model::caller"
	original := matcher.BindWithResult(namespace, testTurns("a", "b", "shared-one", "shared-two"), "auth-old")
	extended := matcher.BindWithResult(namespace,
		testTurns("a", "b", "shared-one", "shared-two", "extra", "shared-one", "shared-two"), "auth-new")
	if extended.SessionID != original.SessionID {
		t.Fatalf("linear extension changed session: original=%+v extended=%+v", original, extended)
	}
	candidate := testTurns("summary", "shared-one", "shared-two", "new")
	for range 32 {
		match, ok := matcher.Match(namespace, candidate)
		if !ok || !match.IsCompaction || match.AuthID != "auth-new" {
			t.Fatalf("same-session tail used stale auth: %+v, ok=%v", match, ok)
		}
	}
}

func TestMerklePrefixMatcherCompactionUsesActualTailBeyondPrefixLimit(t *testing.T) {
	t.Parallel()

	matcher := NewMerklePrefixMatcher(time.Hour)
	const namespace = "lcp:v1::provider::model::caller"
	items := make([]string, defaultMatcherMaxTurns+80)
	for index := range items {
		items[index] = fmt.Sprintf("turn-%d", index)
	}
	parent := testTurns(items...)
	root := matcher.BindWithResult(namespace, parent, "auth-a")
	candidate := append([]string{"summary"}, items[len(items)-8:]...)
	candidate = append(candidate, "new turn")
	if match, ok := matcher.Match(namespace, testTurns(candidate...)); !ok || !match.IsCompaction ||
		match.ParentSessionID != root.SessionID {
		t.Fatalf("long parent actual tail = %+v, ok=%v", match, ok)
	}
}

func TestMerklePrefixMatcherCompactionTouchUpdatesEnvironment(t *testing.T) {
	t.Parallel()

	matcher := NewMerklePrefixMatcher(time.Hour)
	const namespace = "lcp:v1::provider::model::caller"
	items := make([]string, defaultMatcherMaxTurns+16)
	for index := range items {
		items[index] = fmt.Sprintf("turn-%d", index)
	}
	parent := testTurns(items...)
	root := matcher.BindWithResult(namespace, parent, "auth-a")
	updated := append(parent, CanonicalTurn{Role: "developer", Parts: []CanonicalPart{{Kind: "text", Value: "late instruction"}}})
	updated = append(updated, testTurns("new turn", "response")...)
	if !matcher.Touch(namespace, updated, "auth-a") {
		t.Fatal("touch did not update long conversation")
	}
	candidate := []CanonicalTurn{{Role: "user", Parts: []CanonicalPart{{Kind: "text", Value: "summary"}}}}
	candidate = append(candidate, updated[len(updated)-3:]...)
	candidate = append(candidate, testTurns("next turn")...)
	if match, ok := matcher.Match(namespace, candidate); !ok || !match.IsCompaction ||
		match.ParentSessionID != root.SessionID {
		t.Fatalf("late instruction continuation = %+v, ok=%v", match, ok)
	}
	candidateWithoutInstruction := append([]CanonicalTurn{candidate[0]}, candidate[2:]...)
	if match, ok := matcher.Match(namespace, candidateWithoutInstruction); ok {
		t.Fatalf("candidate missing developer instruction matched: %+v", match)
	}
}

func TestMerklePrefixMatcherConsecutiveCompactionChoosesLeaf(t *testing.T) {
	t.Parallel()

	matcher := NewMerklePrefixMatcher(time.Hour)
	const namespace = "lcp:v1::provider::model::caller"
	parent := matcher.BindWithResult(namespace, testTurns("a", "b", "c", "d"), "auth-a")
	first := matcher.BindWithResult(namespace, testTurns("summary-one", "c", "d"), "auth-a")
	if !first.IsCompaction || first.ParentSessionID != parent.SessionID {
		t.Fatalf("first compaction = %+v, parent=%+v", first, parent)
	}
	second, ok := matcher.Match(namespace, testTurns("summary-two", "c", "d", "e"))
	if !ok || !second.IsCompaction || second.ParentSessionID != first.SessionID {
		t.Fatalf("second compaction = %+v, ok=%v; first=%+v", second, ok, first)
	}
}

func TestMerklePrefixMatcherCompactionBucketOverflowAndRemoval(t *testing.T) {
	t.Parallel()

	matcher := NewMerklePrefixMatcher(time.Hour)
	const namespace = "lcp:v1::provider::model::caller"
	other := make([]CanonicalTurn, 0)
	for index := range maxTailsPerKey {
		turns := append([]CanonicalTurn{{Role: "system", Parts: []CanonicalPart{{Kind: "text", Value: "other"}}}},
			testTurns(fmt.Sprintf("unrelated-%d", index), "different", "same-tail-a", "same-tail-b")...)
		matcher.BindWithResult(namespace, turns, fmt.Sprintf("auth-%d", index))
		if index == 0 {
			other = turns
		}
	}
	target := append([]CanonicalTurn{{Role: "system", Parts: []CanonicalPart{{Kind: "text", Value: "target"}}}},
		testTurns("target-start", "different", "same-tail-a", "same-tail-b")...)
	matcher.BindWithResult(namespace, target, "target-auth")
	candidate := append([]CanonicalTurn{{Role: "system", Parts: []CanonicalPart{{Kind: "text", Value: "target"}}}},
		testTurns("summary", "same-tail-a", "same-tail-b", "new")...)
	if match, ok := matcher.Match(namespace, candidate); ok {
		t.Fatalf("overfull shared-tail bucket selected a credential: %+v", match)
	}
	if !matcher.Remove(namespace, other, "auth-0") {
		t.Fatal("failed to remove one group from the shared-tail bucket")
	}
	if match, ok := matcher.Match(namespace, candidate); !ok || !match.IsCompaction || match.AuthID != "target-auth" {
		t.Fatalf("compaction after bucket removal = %+v, ok=%v", match, ok)
	}
}

func TestMerklePrefixMatcherCompactionLongCandidate(t *testing.T) {
	t.Parallel()

	matcher := NewMerklePrefixMatcher(time.Hour)
	const namespace = "lcp:v1::provider::model::caller"
	items := make([]string, defaultMatcherMaxTurns+100)
	for index := range items {
		items[index] = fmt.Sprintf("turn-%d", index)
	}
	parent := matcher.BindWithResult(namespace, testTurns(items...), "auth-a")
	candidate := append([]string{"summary"}, items[24:]...)
	candidate = append(candidate, "new turn")
	if match, ok := matcher.Match(namespace, testTurns(candidate...)); !ok || !match.IsCompaction ||
		match.ParentSessionID != parent.SessionID {
		t.Fatalf("long candidate continuation = %+v, ok=%v", match, ok)
	}
}

func TestExtractSessionInfoLCPCompactionAndFork(t *testing.T) {
	t.Parallel()

	for _, test := range []struct {
		name       string
		compaction bool
		wantKind   string
	}{
		{name: "compaction", compaction: true, wantKind: "compaction"},
		{name: "fork", compaction: false, wantKind: "fork"},
	} {
		t.Run(test.name, func(t *testing.T) {
			metadata := map[string]any{
				cliproxyexecutor.LCPAffinitySessionIDMetadataKey: "lcp:v1:child",
				cliproxyexecutor.ParentSessionIDMetadataKey:      "lcp:v1:parent",
				cliproxyexecutor.IsCompactionMetadataKey:         test.compaction,
			}
			info, ok := ExtractSessionInfo(http.Header{}, nil, metadata)
			if !ok || info.SessionID != "lcp:v1:child" || info.ParentSessionID != "lcp:v1:parent" ||
				info.IsCompaction != test.compaction || info.IsFork == test.compaction || info.NodeKind != test.wantKind {
				t.Fatalf("LCP session info = %+v, ok=%v", info, ok)
			}
		})
	}
}

func TestMerklePrefixMatcherCompactionPreservesPrefix(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name   string
		prefix []CanonicalTurn
	}{
		{name: "system", prefix: []CanonicalTurn{{Role: "system", Parts: []CanonicalPart{{Kind: "text", Value: "instruction"}}}}},
		{name: "user", prefix: testTurns("first")},
	} {
		t.Run(test.name, func(t *testing.T) {
			matcher := NewMerklePrefixMatcher(time.Hour)
			parent := append(append([]CanonicalTurn(nil), test.prefix...), testTurns("second", "third", "fourth")...)
			root := matcher.BindWithResult("lcp:v1:prefix", parent, "auth-a")
			compacted := append(append([]CanonicalTurn(nil), test.prefix...), testTurns("summary", "third", "fourth", "fifth")...)
			match, ok := matcher.Match("lcp:v1:prefix", compacted)
			if !ok || !match.IsCompaction || match.IsFork || match.ParentSessionID != root.SessionID || match.AuthID != "auth-a" {
				t.Fatalf("compaction preserving %s prefix = %+v, ok=%v", test.name, match, ok)
			}
		})
	}
}

func TestMerklePrefixMatcherTruncationRequiresNearbySharedTail(t *testing.T) {
	t.Parallel()
	matcher := NewMerklePrefixMatcher(time.Hour)
	const namespace = "lcp:v1:truncation"
	parent := matcher.BindWithResult(namespace, testTurns("first", "second", "third"), "auth-a")
	match, ok := matcher.Match(namespace, testTurns("second", "third", "fourth"))
	if !ok || !match.IsCompaction || match.ParentSessionID != parent.SessionID {
		t.Fatalf("single-turn truncation = %+v, ok=%v", match, ok)
	}
	unrelated := make([]string, 0, 40)
	for index := range 38 {
		unrelated = append(unrelated, fmt.Sprintf("unrelated-%d", index))
	}
	unrelated = append(unrelated, "second", "third")
	if candidate, found := matcher.Match(namespace, testTurns(unrelated...)); found {
		t.Fatalf("unrelated long conversation matched: %+v", candidate)
	}
}
