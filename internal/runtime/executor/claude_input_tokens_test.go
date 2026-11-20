package executor

import (
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/tiktoken-go/tokenizer"
)

func TestCollectClaudeInputTokenSegments(t *testing.T) {
	payload := []byte(`{
        "model":"claude-test",
        "system":[
            {"type":"text","text":"Follow repository rules.","cache_control":{"type":"ephemeral"}},
            {"type":"image","source":{"type":"base64","media_type":"image/png","data":"ignored-system-image"}}
        ],
        "messages":[
            {"role":"user","content":[
                {"type":"text","text":"Review the implementation."},
                {"type":"document","source":{"type":"text","data":"Reference document text."}},
                {"type":"image","source":{"type":"base64","media_type":"image/png","data":"ignored-image"}}
            ]},
            {"role":"assistant","content":[
                {"type":"thinking","thinking":"Inspect the relevant files.","signature":"ignored-signature"},
                {"type":"tool_use","id":"toolu_1","name":"read_file","input":{"path":"main.go"}}
            ]},
            {"role":"user","content":[
                {"type":"tool_result","tool_use_id":"toolu_1","content":[
                    {"type":"text","text":"package main"},
                    {"type":"image","source":{"type":"base64","data":"ignored-tool-image"}}
                ]}
            ]}
        ],
        "tools":[{
            "name":"read_file",
            "description":"Reads a repository file.",
            "input_schema":{"type":"object","properties":{"path":{"type":"string"}}},
            "cache_control":{"type":"ephemeral"}
        }],
        "tool_choice":{"type":"tool","name":"read_file"},
        "metadata":{"user_id":"ignored-metadata"},
        "max_tokens":4096,
        "stream":true
    }`)

	got, err := collectClaudeInputTokenSegments(payload)
	if err != nil {
		t.Fatalf("collectClaudeInputTokenSegments() error = %v", err)
	}
	want := []string{
		"Follow repository rules.", "user", "Review the implementation.", "Reference document text.",
		"assistant", "Inspect the relevant files.", "toolu_1", "read_file", `{"path":"main.go"}`,
		"user", "toolu_1", "package main", "read_file", "Reads a repository file.",
		`{"type":"object","properties":{"path":{"type":"string"}}}`, "tool", "read_file",
	}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("segments = %#v, want %#v", got, want)
	}
}

func TestCollectClaudeInputTokenSegmentsIncludesKnownToolResults(t *testing.T) {
	payload := []byte(("{\n        \"messages\":[{\"role\":\"user\",\"content\":[\n            {\"type\":\"" +
		"web_search_tool_result\",\"tool_use_id\":\"ws_tool_1\",\"content\":[\n                {\"type\":\"" +
		"web_search_result\",\"source\":\"Search source\",\"title\":\"Search result title\",\"url\":\"https:/" +
		"/search.example/result\",\"page_age\":\"1 day\",\"encrypted_content\":\"ignored-secret\"}\n         " +
		"   ]},\n            {\"type\":\"web_fetch_tool_result\",\"tool_use_id\":\"fetch_tool_1\",\"content\"" +
		":{\n                \"type\":\"web_fetch_result\",\"url\":\"https://docs.example/page\",\"" +
		"retrieved_at\":\"2026-07-22T00:00:00Z\",\"content\":{\n                    \"type\":\"document\",\"" +
		"title\":\"Fetched document\",\"source\":{\"type\":\"text\",\"data\":\"Fetched body\"}\n             " +
		"   }\n            }},\n            {\"type\":\"bash_code_execution_tool_result\",\"tool_use_id\":\"" +
		"bash_tool_1\",\"content\":{\n                \"type\":\"bash_code_execution_result\",\"stdout\":\"" +
		"command output\",\"stderr\":\"command error\",\"return_code\":1,\n                \"content\":[{\"" +
		"type\":\"text\",\"text\":\"additional output\"}]\n            }},\n            {\"type\":\"" +
		"tool_result\",\"tool_use_id\":\"toolu_1\",\"content\":[\n                {\"type\":\"" +
		"tool_reference\",\"tool_name\":\"proxy_mcp__nia__manage_resource\"}\n            ]}\n        ]}]\n  " +
		"  }"))

	segments, err := collectClaudeInputTokenSegments(payload)
	if err != nil {
		t.Fatalf("collectClaudeInputTokenSegments() error = %v", err)
	}
	joined := "\n" + strings.Join(segments, "\n") + "\n"
	for _, want := range []string{
		"ws_tool_1", "Search source", "Search result title", "https://search.example/result", "1 day",
		"fetch_tool_1", "https://docs.example/page", "2026-07-22T00:00:00Z", "Fetched document", "Fetched body",
		"bash_tool_1", "command output", "command error", "1", "additional output", "toolu_1",
		"proxy_mcp__nia__manage_resource",
	} {
		if !strings.Contains(joined, "\n"+want+"\n") {
			t.Errorf("segments do not contain %q: %#v", want, segments)
		}
	}
	if strings.Contains(joined, "ignored-secret") {
		t.Fatalf("segments contain encrypted content: %#v", segments)
	}
}

func TestCountClaudeInputTokensExcludesMultimediaAndControlFields(t *testing.T) {
	enc, err := tokenizer.Get(tokenizer.O200kBase)
	if err != nil {
		t.Fatalf("tokenizer.Get() error = %v", err)
	}
	base := []byte(`{
        "system":"System text.",
        "messages":[{"role":"user","content":[{"type":"text","text":"User text."}]}],
        "tools":[{"name":"lookup","description":"Looks up data.","input_schema":{"type":"object"}}]
    }`)
	withExcludedFields := []byte(("{\n        \"model\":\"claude-test\",\n        \"system\":\"System text.\"," +
		"\n        \"messages\":[{\"role\":\"user\",\"content\":[\n            {\"type\":\"text\",\"text\":\"" +
		"User text.\"},\n            {\"type\":\"image\",\"source\":{\"type\":\"base64\",\"media_type\":\"" +
		"image/png\",\"data\":\"very-large-image-data\"}},\n            {\"type\":\"input_audio\",\"source\":" +
		"{\"type\":\"base64\",\"data\":\"very-large-audio-data\"}},\n            {\"type\":\"video\",\"" +
		"source\":{\"type\":\"url\",\"url\":\"https://example.com/video.mp4\"}},\n            {\"type\":\"" +
		"document\",\"source\":{\"type\":\"base64\",\"media_type\":\"application/pdf\",\"data\":\"very-large-" +
		"pdf-data\"}}\n        ]}],\n        \"tools\":[{\"name\":\"lookup\",\"description\":\"Looks up data." +
		"\",\"input_schema\":{\"type\":\"object\"},\"cache_control\":{\"type\":\"ephemeral\"}}],\n        \"" +
		"metadata\":{\"large_wrapper\":\"ignored\"},\n        \"max_tokens\":8192,\n        \"temperature\":" +
		"0.8,\n        \"top_p\":0.9,\n        \"thinking\":{\"type\":\"enabled\",\"budget_tokens\":4096},\n " +
		"       \"stream\":true\n    }"))
	baseCount, err := countClaudeInputTokensWithCodec(enc, base)
	if err != nil {
		t.Fatal(err)
	}
	excludedCount, err := countClaudeInputTokensWithCodec(enc, withExcludedFields)
	if err != nil {
		t.Fatal(err)
	}
	if excludedCount != baseCount {
		t.Fatalf("count with excluded fields = %d, want %d", excludedCount, baseCount)
	}
}

func TestClaudeInputTokenizerConcurrentCount(t *testing.T) {
	first, err := claudeInputTokenizer()
	if err != nil {
		t.Fatal(err)
	}
	second, err := claudeInputTokenizer()
	if err != nil {
		t.Fatal(err)
	}
	if first != second {
		t.Fatal("claudeInputTokenizer() returned different codec instances")
	}
	const workers = 32
	const iterations = 50
	var wg sync.WaitGroup
	errs := make(chan error, workers)
	for worker := range workers {
		wg.Go(
			func() {
				for iteration := range iterations {
					payload := []byte(fmt.Sprintf(
						`{"messages":[{"role":"user","content":"worker %d iteration %d 你好"}]}`,
						worker, iteration,
					))
					count, err := countClaudeInputTokensWithCodec(first, payload)
					if err != nil {
						errs <- err
						return
					}
					if count <= 0 {
						errs <- fmt.Errorf("non-positive count: %d", count)
						return
					}
				}
			},
		)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
}
