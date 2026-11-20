package auth

import (
	"bytes"
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	core "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
)

func TestForceMappedCompleteFragmentsDoNotWaitForFinish(t *testing.T) {
	for _, body := range []string{
		`{"model":"upstream-model"}`,
		`{"modelVersion":"upstream-model"}`,
		`{"response":{"model":"upstream-model"}}`,
		`{"response":{"modelVersion":"upstream-model"}}`,
		`{"message":{"model":"upstream-model"}}`,
	} {
		for index, prefix := range []string{"", "data:", "data: ", "event: response.created\ndata: "} {
			t.Run(
				fmt.Sprintf("%s/prefix=%d", body, index), func(t *testing.T) {
					rewriter := NewStreamRewriter(StreamRewriteOptions{RewriteModel: "public-model"})
					input := []byte(prefix + body)
					original := bytes.Clone(input)
					got := rewriteForceMappedStreamChunk(rewriter, input)
					if !strings.Contains(string(got), `"public-model"`) || strings.Contains(
						string(got), "upstream-model",
					) || !bytes.HasPrefix(got, []byte(prefix)) {
						t.Fatalf("complete fragment was not delivered with its rewritten model: %q", got)
					}
					if len(rewriter.pendingBuf) != 0 || len(finishForceMappedStreamChunks(rewriter)) != 0 {
						t.Fatal("complete fragment was retained or delivered twice")
					}
					if !bytes.Equal(input, original) {
						t.Fatal("rewriter mutated the caller fragment")
					}
				},
			)
		}
	}
}

func TestForceMappedSeparateUndelimitedEventAndDataFragments(t *testing.T) {
	rewriter := NewStreamRewriter(StreamRewriteOptions{RewriteModel: "public-model"})
	for _, event := range []string{"response.created", "response.output_text.delta", "response.completed"} {
		if got := rewriteForceMappedStreamChunk(rewriter, []byte("event: "+event)); len(got) != 0 {
			t.Fatalf("partial event header was forwarded: %q", got)
		}
		body := []byte(fmt.Sprintf(`data: {"type":%q,"response":{"model":"upstream-model"}}`, event))
		got := rewriteForceMappedStreamChunk(rewriter, body)
		if !bytes.HasPrefix(got, []byte("event: "+event+"\ndata: ")) || !bytes.Contains(
			got, []byte(`"model":"public-model"`),
		) || bytes.Count(got, []byte(event)) != 2 {
			t.Fatalf("undelimited event/data fragments changed or were withheld: %q", got)
		}
	}
	if got := finishForceMappedStreamChunks(rewriter); len(got) != 0 {
		t.Fatalf("complete per-line events were duplicated at EOF: %q", got)
	}
}

func TestForceMappedStreamDeliversBeforeUpstreamCloses(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	remaining := make(chan core.StreamChunk)
	t.Cleanup(func() { close(remaining) })
	manager := NewManager(nil, nil, nil)
	manager.SetHomeClientProvider(func() HomeClient { return nil })
	result := manager.wrapStreamResultWithOptions(
		ctx, ctx, &Auth{ID: t.Name(), Provider: "fixture"}, "fixture", "upstream-model", core.Options{}, nil,
		[]core.StreamChunk{{Payload: []byte(`data: {"model":"upstream-model"}`)}}, remaining,
		OAuthModelAliasResult{ForceMapping: true, OriginalAlias: "public-model"},
	)
	select {
	case chunk := <-result.Chunks:
		if chunk.Err != nil || !bytes.Contains(chunk.Payload, []byte(`"model":"public-model"`)) {
			t.Fatalf("managed stream fragment: payload=%q error=%v", chunk.Payload, chunk.Err)
		}
	case <-ctx.Done():
		t.Fatal("managed stream withheld a complete fragment until upstream EOF")
	}
}

func TestStreamRewriterSplitFragmentAndSizeLimit(t *testing.T) {
	rewriter := NewStreamRewriter(StreamRewriteOptions{RewriteModel: "public-model"})
	if got := rewriter.RewriteChunk([]byte(`data: {"model":"upstream-`)); len(got) != 0 {
		t.Fatalf("incomplete fragment was delivered: %q", got)
	}
	if got := rewriter.RewriteChunk([]byte(`model"}`)); !bytes.Contains(got, []byte(`"model":"public-model"`)) {
		t.Fatalf("completed fragment was not delivered immediately: %q", got)
	}
	if tail := rewriter.Finish(); len(tail) != 0 {
		t.Fatalf("completed fragment was duplicated at EOF: %q", tail)
	}
	large := []byte(`data: {"model":"upstream-model","text":"` + strings.Repeat("x", maxPendingBufSize) + `"}`)
	if got := rewriter.RewriteChunk(large); !bytes.Equal(got, large) || len(rewriter.pendingBuf) != 0 {
		t.Fatal("oversized fragment changed or was retained")
	}
}
