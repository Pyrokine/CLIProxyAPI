package executor

import (
	"context"
	"encoding/base64"
	"strconv"
	"strings"
	"testing"

	"github.com/tidwall/gjson"
)

// noinspection GoUnusedGlobalVariable
var benchmarkSanitizeOpenAIResponsesReasoningOutput []byte

func validOpenAIResponsesReasoningEncryptedContentForTest() string {
	payload := make([]byte, 1+8+16+16+32)
	payload[0] = 0x80
	for i := 9; i < len(payload); i++ {
		payload[i] = byte(i)
	}
	return base64.RawURLEncoding.EncodeToString(payload)
}

func TestSanitizeOpenAIResponsesReasoningEncryptedContent_StripsOrphanIDsWhenStoreDisabled(t *testing.T) {
	valid := validOpenAIResponsesReasoningEncryptedContentForTest()
	body := []byte(`{"store":false,"input":[` +
		`{"id":"rs_bad","type":"reasoning","encrypted_content":"bad","summary":[]},` +
		`{"id":"rs_orphan","type":"reasoning","summary":[]},` +
		`{"id":"rs_good","type":"reasoning","encrypted_content":"` + valid + `","summary":[]},` +
		`{"id":"msg_1","type":"message","role":"user","content":"hi"}` +
		`]}`)

	got := sanitizeOpenAIResponsesReasoningEncryptedContent(context.Background(), "test", body)

	if gjson.GetBytes(got, "input.0.encrypted_content").Exists() {
		t.Fatalf("invalid encrypted_content still present: %s", got)
	}
	if gjson.GetBytes(got, "input.0.id").Exists() {
		t.Fatalf("invalid reasoning id should be stripped when store=false: %s", got)
	}
	if gjson.GetBytes(got, "input.1.id").Exists() {
		t.Fatalf("orphan reasoning id should be stripped when store=false: %s", got)
	}
	if gotID := gjson.GetBytes(got, "input.2.id").String(); gotID != "rs_good" {
		t.Fatalf("valid reasoning id = %q, want rs_good; body=%s", gotID, got)
	}
	if gotEC := gjson.GetBytes(got, "input.2.encrypted_content").String(); gotEC != valid {
		t.Fatalf("valid encrypted_content not preserved: %s", got)
	}
	if gotID := gjson.GetBytes(got, "input.3.id").String(); gotID != "msg_1" {
		t.Fatalf("non-reasoning id should stay: %s", got)
	}
}

func TestSanitizeOpenAIResponsesReasoningEncryptedContent_KeepsIDsWhenStoreEnabled(t *testing.T) {
	body := []byte(`{"store":true,"input":[` +
		`{"id":"rs_bad","type":"reasoning","encrypted_content":"bad","summary":[]},` +
		`{"id":"rs_orphan","type":"reasoning","summary":[]}` +
		`]}`)

	got := sanitizeOpenAIResponsesReasoningEncryptedContent(context.Background(), "test", body)

	if gjson.GetBytes(got, "input.0.encrypted_content").Exists() {
		t.Fatalf("invalid encrypted_content still present: %s", got)
	}
	if gotID := gjson.GetBytes(got, "input.0.id").String(); gotID != "rs_bad" {
		t.Fatalf(
			"store=true should keep reasoning id after dropping invalid encrypted_content, got %q body=%s", gotID, got,
		)
	}
	if gotID := gjson.GetBytes(got, "input.1.id").String(); gotID != "rs_orphan" {
		t.Fatalf("store=true should keep orphan reasoning id, got %q body=%s", gotID, got)
	}
}

func TestSanitizeOpenAIResponsesReasoningEncryptedContent_NoopReturnsOriginalBody(t *testing.T) {
	valid := validOpenAIResponsesReasoningEncryptedContentForTest()
	body := []byte(`{"store":false,"input":[{"id":"rs_good","type":"reasoning","encrypted_content":"` + valid + `","summary":[]},{"role":"user","content":"hi"}]}`)
	got := sanitizeOpenAIResponsesReasoningEncryptedContent(context.Background(), "test", body)
	if string(got) != string(body) {
		t.Fatalf("noop path should return original body unchanged\ngot=%s\nwant=%s", got, body)
	}
	if len(got) > 0 && len(body) > 0 && &got[0] != &body[0] {
		t.Fatalf("noop path should return the original body slice")
	}
}

func TestSanitizeOpenAIResponsesReasoningEncryptedContentKeepForeign(t *testing.T) {
	const museSignature = "Q-PaDg-mock-muse-spark-encrypted-reasoning-payload"
	body := []byte(`{"store":false,"input":[` +
		`{"id":"rs_muse","type":"reasoning","summary":[],"content":[{"type":"reasoning_text","text":"cleartext thinking"}],"encrypted_content":"` + museSignature + `"},` +
		`{"id":"rs_gemini","type":"reasoning","encrypted_content":"skip_thought_signature_validator"},` +
		`{"id":"rs_badprefix","type":"reasoning","encrypted_content":"claude#invalid"},` +
		`{"id":"rs_empty","type":"reasoning","encrypted_content":""},` +
		`{"id":"rs_orphan","type":"reasoning"}]}`)

	got := sanitizeOpenAIResponsesReasoningEncryptedContentKeepForeign(context.Background(), "meta executor", body)
	if value := gjson.GetBytes(got, "input.0.encrypted_content").String(); value != museSignature {
		t.Fatalf("foreign signature = %q, want %q; body=%s", value, museSignature, got)
	}
	if value := gjson.GetBytes(got, "input.0.id").String(); value != "rs_muse" {
		t.Fatalf("foreign reasoning id = %q, want rs_muse; body=%s", value, got)
	}
	if value := gjson.GetBytes(got, "input.0.summary.0.text").String(); value != "cleartext thinking" {
		t.Fatalf("promoted reasoning summary = %q; body=%s", value, got)
	}
	if len(gjson.GetBytes(got, "input.0.content").Array()) != 0 {
		t.Fatalf("raw reasoning content remained on strict Responses request: %s", got)
	}
	for _, index := range []int{1, 2, 3} {
		path := "input." + strconv.Itoa(index)
		if gjson.GetBytes(got, path+".encrypted_content").Exists() || gjson.GetBytes(got, path+".id").Exists() {
			t.Fatalf("incompatible or empty encrypted content remained at %s: %s", path, got)
		}
	}
	if gjson.GetBytes(got, "input.4.id").Exists() {
		t.Fatalf("orphan reasoning id remained: %s", got)
	}

	strict := sanitizeOpenAIResponsesReasoningEncryptedContent(context.Background(), "codex", body)
	if gjson.GetBytes(strict, "input.0.encrypted_content").Exists() {
		t.Fatalf("strict Codex unexpectedly accepted foreign signature: %s", strict)
	}
	compat := sanitizeOpenAIResponsesReasoningEncryptedContent(context.Background(), "compat", body, true)
	if gjson.GetBytes(compat, "input.0.encrypted_content").String() != museSignature ||
		gjson.GetBytes(compat, "input.0.content.0.text").String() != "cleartext thinking" ||
		!gjson.GetBytes(compat, "input.3.encrypted_content").Exists() {
		t.Fatalf("compat request lost replayed reasoning content: %s", compat)
	}
}

func BenchmarkSanitizeOpenAIResponsesReasoningEncryptedContentLargeNoopPayload(b *testing.B) {
	body := []byte(`{"store":false,"input":[{"type":"message","role":"user","content":"` + strings.Repeat(
		"x", 8<<20,
	) + `"}]}`)
	b.ReportAllocs()
	b.SetBytes(int64(len(body)))
	b.ResetTimer()
	for b.Loop() {
		benchmarkSanitizeOpenAIResponsesReasoningOutput = sanitizeOpenAIResponsesReasoningEncryptedContent(
			context.Background(), "benchmark", body,
		)
	}
}
