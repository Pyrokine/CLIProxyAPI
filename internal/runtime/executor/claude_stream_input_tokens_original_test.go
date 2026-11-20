package executor

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	log "github.com/sirupsen/logrus"
	"github.com/tidwall/gjson"
	"github.com/tiktoken-go/tokenizer"

	sdktranslator "github.com/Pyrokine/CLIProxyAPI/v6/sdk/translator"
)

type failingClaudeInputCodec struct {
	errorMode tokenizer.ErrorMode
}

func (*failingClaudeInputCodec) GetName() string { return "failing" }
func (*failingClaudeInputCodec) Count(string) (int, error) {
	return 0, errors.New("count failed")
}
func (*failingClaudeInputCodec) Encode(string) ([]uint, []string, error) {
	return nil, nil, errors.New("encode failed")
}
func (*failingClaudeInputCodec) Decode([]uint) (string, error) {
	return "", errors.New("decode failed")
}
func (c *failingClaudeInputCodec) ErrorMode() tokenizer.ErrorMode {
	return c.errorMode
}
func (c *failingClaudeInputCodec) SetErrorMode(mode tokenizer.ErrorMode) {
	c.errorMode = mode
}

func TestTranslateStreamWithClaudeInputTokensPatchesMessageStartOnce(t *testing.T) {
	upstreamFormat := sdktranslator.Format("claude-input-token-test-upstream")
	sdktranslator.Register(
		sdktranslator.FormatClaude, upstreamFormat, nil, sdktranslator.ResponseTransform{
			Stream: func(_ context.Context, _ string, _, _, rawJSON []byte, _ *any) [][]byte {
				return [][]byte{rawJSON}
			},
		},
	)
	originalRequest := []byte(`{"system":"System text.","messages":[{"role":"user","content":"Hello."}]}`)
	handled := false
	var param any
	combined := []byte("event: message_start\ndata: " +
		`{"type":"message_start","message":{"usage":{"input_tokens":0,"output_tokens":0}}}` + "\n\n" +
		"event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0}\n\n")
	before := bytes.Clone(combined)
	translated := sdktranslator.TranslateStream(
		t.Context(), upstreamFormat, sdktranslator.FormatClaude,
		"claude-test", originalRequest, nil, combined, &param,
	)
	got := estimateClaudeStreamInputTokens(t.Context(), translated, originalRequest, &handled)
	if tokens := messageStartInputTokens(got); tokens <= 0 {
		t.Fatalf(
			"message_start input_tokens = %d, want positive estimate; output = %q", tokens, joinClaudeInputChunks(got),
		)
	}
	if !handled {
		t.Fatal("first message_start was not marked as handled")
	}
	if !strings.Contains(joinClaudeInputChunks(got), `"type":"content_block_start"`) {
		t.Fatalf("combined non-target event was not preserved: %q", joinClaudeInputChunks(got))
	}
	if !bytes.Equal(combined, before) {
		t.Fatal("caller stream bytes changed")
	}
	secondStart := []byte(`event: message_start
data: {"type":"message_start","message":{"usage":{"input_tokens":0}}}

`)
	translated = sdktranslator.TranslateStream(
		t.Context(), upstreamFormat, sdktranslator.FormatClaude,
		"claude-test", originalRequest, nil, secondStart, &param,
	)
	gotSecond := estimateClaudeStreamInputTokens(t.Context(), translated, originalRequest, &handled)
	if tokens := messageStartInputTokens(gotSecond); tokens != 0 {
		t.Fatalf("second message_start input_tokens = %d, want 0 after state handled", tokens)
	}
}

func TestClaudeInputTokenStatePreservesCRLFAndNonTargetEvents(t *testing.T) {
	originalRequest := []byte(`{"messages":[{"role":"user","content":"Hello."}]}`)
	handled := false
	chunk := []byte("event: message_start\r\ndata:  " +
		`{"type":"message_start","message":{"usage":{"input_tokens":0,"output_tokens":0}}}` + "  \r\n\r\n" +
		"event: ping\r\ndata: {\"type\":\"ping\",\"value\":\"keep\"}\r\n\r\n")
	before := bytes.Clone(chunk)
	got := estimateClaudeStreamInputTokens(t.Context(), [][]byte{chunk}, originalRequest, &handled)
	tokens := messageStartInputTokens(got)
	if tokens <= 0 {
		t.Fatalf("input_tokens = %d, want positive estimate", tokens)
	}
	want := fmt.Sprintf(
		"event: message_start\r\ndata:  "+
			`{"type":"message_start","message":{"usage":{"input_tokens":%d,"output_tokens":0}}}`+"  \r\n\r\n"+
			"event: ping\r\ndata: {\"type\":\"ping\",\"value\":\"keep\"}\r\n\r\n", tokens,
	)
	if joined := joinClaudeInputChunks(got); joined != want {
		t.Fatalf("output bytes changed unexpectedly:\n got: %q\nwant: %q", joined, want)
	}
	if !bytes.Equal(chunk, before) {
		t.Fatal("caller stream bytes changed")
	}
}

func TestClaudeInputTokenStatePatchesMissingAndPreservesNonZero(t *testing.T) {
	originalRequest := []byte(`{"messages":[{"role":"user","content":"Hello."}]}`)
	t.Run(
		"missing", func(t *testing.T) {
			handled := false
			chunks := [][]byte{
				[]byte("data: {\"type\":\"message_start\",\"message\":{\"usage\":{\"output_tokens\":0}}}\n\n"),
			}
			got := estimateClaudeStreamInputTokens(t.Context(), chunks, originalRequest, &handled)
			if tokens := messageStartInputTokens(got); tokens <= 0 {
				t.Fatalf("input_tokens = %d, want positive estimate", tokens)
			}
		},
	)
	t.Run(
		"non-zero", func(t *testing.T) {
			handled := false
			chunks := [][]byte{
				[]byte("data: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":73}}}\n\n"),
			}
			got := estimateClaudeStreamInputTokens(t.Context(), chunks, []byte(`not valid json`), &handled)
			if tokens := messageStartInputTokens(got); tokens != 73 {
				t.Fatalf("input_tokens = %d, want preserved value 73", tokens)
			}
			if !handled {
				t.Fatal("non-zero message_start was not marked as handled")
			}
		},
	)
}

func TestClaudeInputTokenStateCountErrorKeepsZero(t *testing.T) {
	const childKey = "CPA_TEST_CLAUDE_INPUT_COUNT_FAILURE"
	if os.Getenv(childKey) != "1" {
		ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
		defer cancel()
		binary, err := os.Executable()
		if err != nil {
			t.Fatal(err)
		}
		command := exec.CommandContext(
			ctx, binary,
			"-test.run=^TestClaudeInputTokenStateCountErrorKeepsZero$", "-test.count=1",
		)
		command.Env = append(os.Environ(), childKey+"=1")
		if output, err := command.CombinedOutput(); err != nil {
			t.Fatalf("isolated count failure: %v\n%s", err, output)
		}
		return
	}
	// 子进程独立设置失败编码器，其余请求继续使用正常工作的共享实例
	claudeInputTokenizerOnce.Do(func() { claudeInputTokenizerCodec = &failingClaudeInputCodec{} })
	if _, ok := claudeInputTokenizerCodec.(*failingClaudeInputCodec); !ok {
		t.Fatal("isolated failing codec was not installed")
	}
	originalLogOutput := log.StandardLogger().Out
	log.SetOutput(io.Discard)
	defer log.SetOutput(originalLogOutput)
	handled := false
	originalRequest := []byte(`{"messages":[{"role":"user","content":"Hello."}]}`)
	chunks := [][]byte{[]byte("data: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":0}}}\n\n")}
	got := estimateClaudeStreamInputTokens(t.Context(), chunks, originalRequest, &handled)
	if tokens := messageStartInputTokens(got); tokens != 0 {
		t.Fatalf("input_tokens = %d, want unchanged 0", tokens)
	}
	if !handled {
		t.Fatal("failed estimate was not marked as handled")
	}
}

func TestClaudeInputTokenStateInvalidJSONKeepsZeroWithoutLoggingRequest(t *testing.T) {
	originalLogOutput := log.StandardLogger().Out
	var logOutput bytes.Buffer
	log.SetOutput(&logOutput)
	defer log.SetOutput(originalLogOutput)
	const sensitiveRequest = `{"messages":["sensitive-original-request"`
	handled := false
	chunks := [][]byte{[]byte("data: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":0}}}\n\n")}
	got := estimateClaudeStreamInputTokens(t.Context(), chunks, []byte(sensitiveRequest), &handled)
	if tokens := messageStartInputTokens(got); tokens != 0 {
		t.Fatalf("input_tokens = %d, want unchanged 0", tokens)
	}
	if !handled {
		t.Fatal("invalid original request was not marked as handled")
	}
	if !strings.Contains(logOutput.String(), "failed to estimate Claude input tokens") {
		t.Fatalf("warning not logged: %q", logOutput.String())
	}
	if strings.Contains(logOutput.String(), "sensitive-original-request") {
		t.Fatalf("warning leaked original request: %q", logOutput.String())
	}
}

func messageStartInputTokens(chunks [][]byte) int64 {
	for _, chunk := range chunks {
		for line := range strings.SplitSeq(string(chunk), "\n") {
			trimmed := strings.TrimSpace(line)
			if !strings.HasPrefix(trimmed, "data:") {
				continue
			}
			payload := strings.TrimSpace(strings.TrimPrefix(trimmed, "data:"))
			if gjson.Get(payload, "type").String() == "message_start" {
				return gjson.Get(payload, "message.usage.input_tokens").Int()
			}
		}
	}
	return 0
}

func joinClaudeInputChunks(chunks [][]byte) string {
	var builder strings.Builder
	for _, chunk := range chunks {
		builder.Write(chunk)
	}
	return builder.String()
}
