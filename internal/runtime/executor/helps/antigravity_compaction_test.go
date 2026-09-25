package helps

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/tidwall/gjson"
)

func testAntigravityCompactionScope() AntigravityCompactionScope {
	return AntigravityCompactionScope{
		Provider:    "antigravity",
		Model:       "gemini-3.7-flash",
		Session:     "session:test",
		Owner:       "id:test-owner",
		KeyMaterial: []byte("test-only-compaction-key-material"),
		Now:         time.Date(2026, time.September, 23, 12, 0, 0, 0, time.UTC),
		TTL:         time.Hour,
	}
}

func TestAntigravityCompactionDetection(t *testing.T) {
	payload := []byte(`{"input":[{"type":"message"},{"type":"compaction_trigger"},{"type":"compaction"}]}`)
	if !HasResponsesCompactionTrigger(payload) {
		t.Fatal("compaction trigger was not detected")
	}
	if !HasResponsesCompactionItem(payload) {
		t.Fatal("compaction item was not detected")
	}
	if HasResponsesCompactionTrigger([]byte(`{"input":"text"}`)) || HasResponsesCompactionItem([]byte(`{"input":[]}`)) {
		t.Fatal("compaction detection matched payload without target item")
	}
}

func TestAntigravityCompactionSealAndUnseal(t *testing.T) {
	scope := testAntigravityCompactionScope()
	summary := "summary with control chars \x1b[31mred\x1b[0m, quotes \"hello\", and a newline\nline two"
	first, err := SealAntigravityCompaction(summary, scope)
	if err != nil {
		t.Fatalf("SealAntigravityCompaction first: %v", err)
	}
	second, err := SealAntigravityCompaction(summary, scope)
	if err != nil {
		t.Fatalf("SealAntigravityCompaction second: %v", err)
	}
	if !strings.HasPrefix(first, antigravityCompactionCapsulePrefix) {
		t.Fatalf("capsule = %q, want prefix %q", first, antigravityCompactionCapsulePrefix)
	}
	if first == second {
		t.Fatal("two capsules reused the same nonce/ciphertext")
	}
	for _, capsule := range []string{first, second} {
		got, errUnseal := UnsealAntigravityCompaction(capsule, scope)
		if errUnseal != nil {
			t.Fatalf("UnsealAntigravityCompaction: %v", errUnseal)
		}
		if got != summary {
			t.Fatalf("unsealed summary = %q, want %q", got, summary)
		}
	}

	tamperIndex := len(antigravityCompactionCapsulePrefix) + 5
	tamperedByte := byte('A')
	if first[tamperIndex] == tamperedByte {
		tamperedByte = 'B'
	}
	tampered := first[:tamperIndex] + string(tamperedByte) + first[tamperIndex+1:]
	if _, errTampered := UnsealAntigravityCompaction(tampered, scope); errTampered == nil {
		t.Fatal("tampered capsule was accepted")
	}
	if _, errPrefix := UnsealAntigravityCompaction("other:abc", scope); errPrefix == nil {
		t.Fatal("unrecognized capsule prefix was accepted")
	}
	if _, errShort := UnsealAntigravityCompaction(
		antigravityCompactionCapsulePrefix+"YQ",
		scope,
	); errShort == nil {
		t.Fatal("short capsule was accepted")
	}
}

func TestAntigravityCompactionRejectsWrongScopeAndLifetime(t *testing.T) {
	scope := testAntigravityCompactionScope()
	capsule, errSeal := SealAntigravityCompaction("summary", scope)
	if errSeal != nil {
		t.Fatalf("SealAntigravityCompaction: %v", errSeal)
	}

	tests := []struct {
		name  string
		scope AntigravityCompactionScope
	}{
		{name: "provider", scope: scope},
		{name: "model", scope: scope},
		{name: "session", scope: scope},
		{name: "owner", scope: scope},
		{name: "key", scope: scope},
		{name: "expired", scope: scope},
		{name: "issued in future", scope: scope},
	}
	tests[0].scope.Provider = "other"
	tests[1].scope.Model = "gemini-3.7-pro"
	tests[2].scope.Session = "session:other"
	tests[3].scope.Owner = "id:other"
	tests[4].scope.KeyMaterial = []byte("other-test-key-material")
	tests[5].scope.Now = scope.Now.Add(2 * time.Hour)
	tests[6].scope.Now = scope.Now.Add(-time.Hour)
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if summary, errUnseal := UnsealAntigravityCompaction(capsule, test.scope); errUnseal == nil {
				t.Fatalf("scope accepted capsule with summary %q", summary)
			}
		})
	}
}

func TestPrepareAntigravityCompactionSummaryPayload(t *testing.T) {
	payload := []byte(`{
		"model":"gemini-3.7-flash",
		"stream":true,
		"tools":[{"type":"function"}],
		"tool_choice":"auto",
		"previous_response_id":"resp-old",
		"parallel_tool_calls":true,
		"additional_tools":[{}],
		"truncation":"auto",
		"metadata":{"key":"value"},
		"input":[
			{"type":"message","role":"user","content":"hi"},
			{"type":"compaction_trigger"}
		]
	}`)
	prepared := PrepareAntigravityCompactionSummaryPayload(payload, "gemini-3.7-flash")
	if !json.Valid(prepared) {
		t.Fatalf("prepared payload is invalid JSON: %s", prepared)
	}
	if gjson.GetBytes(prepared, "stream").Bool() {
		t.Fatal("summary payload stream = true, want false")
	}
	for _, field := range []string{
		"tools", "tool_choice", "previous_response_id", "parallel_tool_calls",
		"additional_tools", "truncation", "metadata",
	} {
		if gjson.GetBytes(prepared, field).Exists() {
			t.Fatalf("summary payload retained %q: %s", field, prepared)
		}
	}
	input := gjson.GetBytes(prepared, "input").Array()
	if len(input) != 2 {
		t.Fatalf("summary input length = %d, want 2", len(input))
	}
	for _, item := range input {
		if item.Get("type").String() == "compaction_trigger" {
			t.Fatal("summary payload retained compaction_trigger")
		}
	}
	if input[len(input)-1].Get("role").String() != "user" || input[len(input)-1].Get("content.0.type").String() != "input_text" {
		t.Fatalf("summary prompt item = %s", input[len(input)-1].Raw)
	}
}

func TestPrepareAntigravityCompactionSummaryPayloadStringInput(t *testing.T) {
	want := "before\x1b[31mafter\nnewline and \"quotes\""
	payload, errMarshal := json.Marshal(map[string]any{
		"model": "gemini-3.7-flash",
		"input": want,
	})
	if errMarshal != nil {
		t.Fatalf("marshal string payload: %v", errMarshal)
	}
	prepared := PrepareAntigravityCompactionSummaryPayload(payload, "gemini-3.7-flash")
	if !json.Valid(prepared) {
		t.Fatalf("prepared string payload is invalid JSON: %s", prepared)
	}
	input := gjson.GetBytes(prepared, "input").Array()
	if len(input) != 2 {
		t.Fatalf("summary input length = %d, want 2", len(input))
	}
	if got := input[0].Get("content.0.text").String(); got != want {
		t.Fatalf("converted string input = %q, want %q", got, want)
	}
}

func TestExpandAntigravityCompactionCapsules(t *testing.T) {
	scope := testAntigravityCompactionScope()
	summary := "previous work with control chars \x1b[32mgreen\x1b[0m"
	capsule, err := SealAntigravityCompaction(summary, scope)
	if err != nil {
		t.Fatalf("seal capsule: %v", err)
	}
	payload := []byte(`{"model":"gemini-3.7-flash","input":[` +
		`{"type":"compaction","id":"cmp-1","encrypted_content":"` + capsule + `"},` +
		`{"type":"message","role":"user","content":"next"}]}`)
	expanded, err := ExpandAntigravityCompactionCapsules(payload, scope)
	if err != nil {
		t.Fatalf("ExpandAntigravityCompactionCapsules: %v", err)
	}
	if !json.Valid(expanded) {
		t.Fatalf("expanded payload is invalid JSON: %s", expanded)
	}
	first := gjson.GetBytes(expanded, "input.0")
	if first.Get("type").String() != "message" || first.Get("role").String() != "developer" {
		t.Fatalf("expanded item = %s", first.Raw)
	}
	if text := first.Get("content.0.text").String(); !strings.Contains(text, summary) {
		t.Fatalf("expanded summary = %q, want %q", text, summary)
	}
	if gjson.GetBytes(expanded, "input.1.content").String() != "next" {
		t.Fatal("non-compaction input changed during expansion")
	}

	unchanged := []byte(`{"input":[{"type":"message","role":"user","content":"hi"}]}`)
	got, err := ExpandAntigravityCompactionCapsules(unchanged, scope)
	if err != nil || !bytes.Equal(got, unchanged) {
		t.Fatalf("payload without capsule = (%s, %v), want unchanged", got, err)
	}
	bad := []byte(`{"input":[{"type":"compaction","encrypted_content":"cpa-ag-compact-v1:corrupted"}]}`)
	if _, errBad := ExpandAntigravityCompactionCapsules(bad, scope); errBad == nil {
		t.Fatal("corrupted capsule was accepted")
	}
}

func TestExtractAntigravitySummaryText(t *testing.T) {
	tests := []struct {
		name    string
		payload string
		want    string
	}{
		{
			name: "responses skips reasoning and joins text",
			payload: `{"output":[` +
				`{"type":"reasoning","summary":[{"type":"summary_text","text":"thinking"}]},` +
				`{"type":"message","content":[{"type":"output_text","text":"part one"},{"type":"output_text","text":"part two"}]}]}`,
			want: "part one\npart two",
		},
		{name: "wrapped Gemini", payload: `{"response":{"candidates":[{"content":{"parts":[{"thought":true,"text":"reason"},{"text":"summary"}]}}]}}`, want: "summary"},
		{name: "bare Gemini", payload: `{"candidates":[{"content":{"parts":[{"text":"summary"}]}}]}`, want: "summary"},
		{name: "Claude", payload: `{"content":[{"type":"thinking","thinking":"reason"},{"type":"text","text":"summary"}]}`, want: "summary"},
		{name: "OpenAI chat", payload: `{"choices":[{"message":{"content":"summary"}}]}`, want: "summary"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := ExtractAntigravitySummaryText([]byte(test.payload))
			if err != nil {
				t.Fatalf("ExtractAntigravitySummaryText: %v", err)
			}
			if got != test.want {
				t.Fatalf("summary = %q, want %q", got, test.want)
			}
		})
	}
	if _, err := ExtractAntigravitySummaryText([]byte(`{"output":[{"type":"reasoning"}]}`)); err == nil {
		t.Fatal("missing summary text returned no error")
	}
}

func TestBuildAntigravityCompactionResponse(t *testing.T) {
	response := BuildAntigravityCompactionResponse("gemini-3.7-flash", "cpa-ag-compact-v1:abc", 10, 20, 30)
	if !json.Valid(response) {
		t.Fatalf("compaction response is invalid JSON: %s", response)
	}
	if gjson.GetBytes(response, "object").String() != "response.compaction" || gjson.GetBytes(response, "status").String() != "completed" {
		t.Fatalf("compaction response status = %s", response)
	}
	if gjson.GetBytes(response, "output.0.type").String() != "compaction" || gjson.GetBytes(response, "output.0.status").String() != "completed" {
		t.Fatalf("compaction output item = %s", gjson.GetBytes(response, "output.0").Raw)
	}
	if gjson.GetBytes(response, "usage.input_tokens").Int() != 10 ||
		gjson.GetBytes(response, "usage.output_tokens").Int() != 20 ||
		gjson.GetBytes(response, "usage.total_tokens").Int() != 30 {
		t.Fatalf("compaction response usage = %s", gjson.GetBytes(response, "usage").Raw)
	}
}

func TestBuildAntigravityCompactionStreamChunks(t *testing.T) {
	chunks := BuildAntigravityCompactionStreamChunks("gemini-3.7-flash", "cpa-ag-compact-v1:abc", 10, 20, 30)
	wantEvents := []string{
		"response.created",
		"response.in_progress",
		"response.output_item.added",
		"response.output_item.done",
		"response.completed",
	}
	if len(chunks) != len(wantEvents) {
		t.Fatalf("stream chunk count = %d, want %d", len(chunks), len(wantEvents))
	}
	for index, chunk := range chunks {
		lines := strings.Split(string(chunk), "\n")
		if len(lines) < 3 || lines[0] != "event: "+wantEvents[index] || !strings.HasPrefix(lines[1], "data: ") {
			t.Fatalf("stream chunk %d = %q", index, chunk)
		}
		data := []byte(strings.TrimPrefix(lines[1], "data: "))
		if !json.Valid(data) {
			t.Fatalf("stream chunk %d data is invalid JSON: %s", index, data)
		}
		if gjson.GetBytes(data, "type").String() != wantEvents[index] || gjson.GetBytes(data, "sequence_number").Int() != int64(index) {
			t.Fatalf("stream chunk %d data = %s", index, data)
		}
	}
	completedData := []byte(strings.TrimPrefix(strings.Split(string(chunks[len(chunks)-1]), "\n")[1], "data: "))
	if gjson.GetBytes(completedData, "response.usage.total_tokens").Int() != 30 ||
		gjson.GetBytes(completedData, "response.output.0.encrypted_content").String() != "cpa-ag-compact-v1:abc" {
		t.Fatalf("completed compaction event = %s", completedData)
	}
}
