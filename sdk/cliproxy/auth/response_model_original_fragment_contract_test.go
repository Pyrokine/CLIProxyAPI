package auth

import (
	"bytes"
	"slices"
	"strings"
	"testing"

	"github.com/tidwall/gjson"
)

func TestStreamRewriterOriginalGluedFrameContracts(t *testing.T) {
	for _, tc := range []struct {
		name, input, want string
	}{
		{
			name: "event frames",
			input: "event: response.created\ndata: {\"type\":\"response.created\"}" +
				"event: response.completed\ndata: {\"type\":\"response.completed\"}",
			want: "event: response.created\ndata: {\"type\":\"response.created\"}\n\n" +
				"event: response.completed\ndata: {\"type\":\"response.completed\"}\n\n",
		},
		{
			name:  "data frames",
			input: `data: {"type":"response.created"}data: {"type":"response.completed"}`,
			want:  "data: {\"type\":\"response.created\"}\ndata: {\"type\":\"response.completed\"}",
		},
		{
			name: "event text inside JSON",
			input: "event: response.output_text.delta\ndata: {\"type\":\"delta\",\"text\":" +
				"\"literal }event: inside " +
				"string\"}",
			want: "event: response.output_text.delta\ndata: {\"type\":\"delta\",\"text\":" +
				"\"literal }event: inside " +
				"string\"}",
		},
		{
			name:  "data text inside JSON",
			input: `data: {"type":"delta","text":"literal }data: inside"}`,
			want:  `data: {"type":"delta","text":"literal }data: inside"}`,
		},
		{
			name: "CRLF event frames",
			input: "event: response.created\r\ndata: {\"type\":\"response.created\"}\r\n" +
				"event: response.completed\r\ndata: {\"type\":\"response.completed\"}",
			want: "event: response.created\r\ndata: {\"type\":\"response.created\"}\r\n\r\n" +
				"event: response.completed\r\ndata: {\"type\":\"response.completed\"}",
		},
		{
			name: "CRLF data frames",
			input: "data: {\"type\":\"response.created\"}\r\n" +
				"data: {\"type\":\"response.completed\"}",
			want: "data: {\"type\":\"response.created\"}\r\n" +
				"data: {\"type\":\"response.completed\"}",
		},
		{
			name: "literal followed by actual data boundary",
			input: `data: {"type":"delta","text":"literal }data: inside"}` +
				`data: {"type":"response.completed"}`,
			want: "data: {\"type\":\"delta\",\"text\":\"literal }data: inside\"}\n" +
				`data: {"type":"response.completed"}`,
		},
		{
			name: "three consecutive data frames",
			input: `data: {"type":"response.created"}data: {"type":"response.in_progress"}` +
				`data: {"type":"response.completed"}`,
			want: "data: {\"type\":\"response.created\"}\ndata: {\"type\":\"response.in_progress\"}\n" +
				`data: {"type":"response.completed"}`,
		},
	} {
		t.Run(
			tc.name, func(t *testing.T) {
				rewriter := NewStreamRewriter(StreamRewriteOptions{RewriteModel: "gpt-5.4-fast"})
				input := []byte(tc.input)
				original := bytes.Clone(input)
				got := rewriteForceMappedStreamChunk(rewriter, input)
				got = append(got, finishForceMappedStreamChunks(rewriter)...)
				if string(got) != tc.want {
					t.Fatalf("rewritten fragment = %q, want %q", got, tc.want)
				}
				if !bytes.Equal(input, original) || len(finishForceMappedStreamChunks(rewriter)) != 0 {
					t.Fatal("input changed or a completed fragment was delivered again")
				}
			},
		)
	}
}

func TestStreamRewriterGluedFramesKeepPendingSizeLimit(t *testing.T) {
	const frame = `data: {"model":"upstream-model"}`
	const partial = `data: {"model":"unfinished`
	input := bytes.Repeat([]byte(frame), (maxPendingBufSize-len(partial))/len(frame))
	input = append(input, partial...)
	rewriter := NewStreamRewriter(StreamRewriteOptions{RewriteModel: "public-model"})
	got := rewriter.RewriteChunk(input)
	if len(got) == 0 || len(rewriter.pendingBuf) != 0 ||
		!bytes.Contains(got, []byte("upstream-model")) || bytes.Contains(got, []byte("public-model")) {
		t.Fatalf("expanded frame output=%d pending=%d", len(got), len(rewriter.pendingBuf))
	}
}

func parseResponsesWSDataEventTypes(payload []byte) []string {
	var types []string
	for line := range bytes.SplitSeq(payload, []byte("\n")) {
		line = bytes.TrimSpace(line)
		if len(line) == 0 || bytes.HasPrefix(line, []byte("event:")) {
			continue
		}
		if data, found := bytes.CutPrefix(line, []byte("data:")); found {
			line = bytes.TrimSpace(data)
		}
		if gjson.ValidBytes(line) {
			types = append(types, gjson.GetBytes(line, "type").String())
		}
	}
	return types
}

func TestRewriteForceMappedStreamChunkCodexDataLinesWithoutNewlinesFinishParsesCompleted(t *testing.T) {
	rewriter := NewStreamRewriter(StreamRewriteOptions{RewriteModel: "gpt-5.4-fast"})
	lines := [][]byte{
		[]byte(`data: {"type":"response.created","response":{"model":"gpt-5.4"}}`),
		[]byte(`data: {"type":"response.in_progress","response":{"model":"gpt-5.4"}}`),
		[]byte(`data: {"type":"response.completed","response":{"model":"gpt-5.4","output":[]}}`),
	}
	var types []string
	for _, line := range lines {
		types = append(types, parseResponsesWSDataEventTypes(rewriteForceMappedStreamChunk(rewriter, line))...)
	}
	types = append(types, parseResponsesWSDataEventTypes(finishForceMappedStreamChunks(rewriter))...)
	if !slices.Equal(types, []string{"response.created", "response.in_progress", "response.completed"}) {
		t.Fatalf("forwarded event types = %v", types)
	}
}

func TestCodexForceMapPerLineSSEForwardsCompleted(t *testing.T) {
	rewriter := NewStreamRewriter(StreamRewriteOptions{RewriteModel: "gpt-5.4-fast"})
	lines := [][]byte{
		[]byte("event: response.created"),
		[]byte(`data: {"type":"response.created","response":{"model":"gpt-5.4"}}`),
		[]byte("event: response.output_text.delta"),
		[]byte(`data: {"type":"response.output_text.delta","delta":"OK"}`),
		[]byte("event: response.completed"),
		[]byte(`data: {"type":"response.completed","response":{"model":"gpt-5.4","output":[]}}`),
	}
	var types []string
	for _, line := range lines {
		types = append(types, parseResponsesWSDataEventTypes(rewriteForceMappedStreamChunk(rewriter, line))...)
	}
	types = append(types, parseResponsesWSDataEventTypes(finishForceMappedStreamChunks(rewriter))...)
	if !slices.Equal(types, []string{"response.created", "response.output_text.delta", "response.completed"}) {
		t.Fatalf("forwarded event types = %v", types)
	}
}

func TestRewriteForceMappedStreamChunkFallbackWhenPendingBuffersEvent(t *testing.T) {
	rewriter := NewStreamRewriter(StreamRewriteOptions{RewriteModel: "gpt-5.4-fast"})
	if got := rewriteForceMappedStreamChunk(rewriter, []byte("event: response.completed")); len(got) != 0 {
		t.Fatalf("incomplete event was delivered: %q", got)
	}
	out := rewriteForceMappedStreamChunk(
		rewriter,
		[]byte(`data: {"type":"response.completed","response":{"model":"gpt-5.4","output":[]}}`),
	)
	out = append(out, finishForceMappedStreamChunks(rewriter)...)
	if !slices.Equal(parseResponsesWSDataEventTypes(out), []string{"response.completed"}) ||
		!bytes.Contains(out, []byte(`"model":"gpt-5.4-fast"`)) {
		t.Fatalf("completed event = %q", out)
	}
}

func TestStreamRewriterOriginalLoggedOnceAndRewrittenChunksCount(t *testing.T) {
	rewriter := NewStreamRewriter(StreamRewriteOptions{RewriteModel: "gemini-3.8-flash"})
	chunks := [][]byte{
		[]byte("data: {\"modelVersion\":\"gemini-3.8-flash-high\",\"text\":\"part 1\"}\n\n"),
		[]byte("data: {\"modelVersion\":\"gemini-3.8-flash-high\",\"text\":\"part 2\"}\n\n"),
		[]byte("data: {\"modelVersion\":\"gemini-3.8-flash-high\",\"text\":\"part 3\"}\n\n"),
		[]byte("data: [DONE]\n\n"),
	}
	for _, chunk := range chunks {
		if got := rewriter.RewriteChunk(chunk); len(got) == 0 || strings.Contains(
			string(got), "gemini-3.8-flash-high",
		) {
			t.Fatalf("rewritten fragment = %q", got)
		}
	}
	finishForceMappedStreamChunks(rewriter)
	_, tracked := rewriter.loggedPaths["modelVersion"]
	if rewriter.rewrittenEvents != 3 || !tracked || !rewriter.finishedLogged {
		t.Fatalf(
			"events=%d paths=%v finished=%t",
			rewriter.rewrittenEvents,
			rewriter.loggedPaths,
			rewriter.finishedLogged,
		)
	}
}
