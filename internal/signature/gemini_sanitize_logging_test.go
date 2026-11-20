package signature

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	log "github.com/sirupsen/logrus"
	"github.com/tidwall/gjson"
)

func captureGeminiSanitizeLog(t *testing.T) *bytes.Buffer {
	t.Helper()
	logger := log.StandardLogger()
	oldOutput, oldFormatter, oldLevel := logger.Out, logger.Formatter, logger.GetLevel()
	var output bytes.Buffer
	logger.SetOutput(&output)
	logger.SetFormatter(&log.JSONFormatter{DisableTimestamp: true})
	logger.SetLevel(log.DebugLevel)
	t.Cleanup(
		func() {
			logger.SetOutput(oldOutput)
			logger.SetFormatter(oldFormatter)
			logger.SetLevel(oldLevel)
		},
	)
	return &output
}

func decodeGeminiSanitizeLog(t *testing.T, output *bytes.Buffer) map[string]any {
	t.Helper()
	lines := bytes.Split(bytes.TrimSpace(output.Bytes()), []byte("\n"))
	if len(lines) != 1 || len(lines[0]) == 0 {
		t.Fatalf("want one summary log, got %q", output.String())
	}
	var entry map[string]any
	if err := json.Unmarshal(lines[0], &entry); err != nil {
		t.Fatal(err)
	}
	return entry
}

func TestGeminiSanitizeLogsOneRequestSummary(t *testing.T) {
	for _, path := range []string{"contents", "request.contents"} {
		t.Run(
			path, func(t *testing.T) {
				output := captureGeminiSanitizeLog(t)
				geminiSignature, foreignSignature := validGeminiThoughtSignatureForTest(),
					validGPTReasoningSignatureForTest()
				body := []byte(`{"contents":[{"role":"model","parts":[` +
					(`{"functionCall":{"id":"a","name":"first","args":{}},` +
						`"thoughtSignature":"fixture-primary-signature"},`) +
					(`{"functionCall":{"id":"b","name":"second","args":{}},` +
						`"thoughtSignature":"fixture-sibling-signature"},`) +
					`{"thought":true,"text":"retained thought","thoughtSignature":"` + foreignSignature + `"}` +
					`]},{"role":"user","parts":[` +
					(`{"functionResponse":{"id":"a","name":"first","response":{"value":1}},` +
						`"thoughtSignature":` +
						`"`) +
					geminiSignature +
					`"},` +
					`{"functionResponse":{"id":"b","name":"second","response":{"value":2}}}` +
					`]}]}`)
				if path == "request.contents" {
					body = []byte(`{"request":` + string(body) + `}`)
				}
				got := SanitizeGeminiRequestThoughtSignatures(body, path)
				entry := decodeGeminiSanitizeLog(t, output)
				if entry["component"] != "signature_sanitizer" || entry["target_provider"] != "gemini" ||
					entry["contents_path"] != path ||
					entry["sanitized_count"] != float64(4) ||
					entry["dropped_signatures"] != float64(3) ||
					entry["replaced_signatures"] != float64(1) ||
					entry["omitted_positions"] != float64(0) {
					t.Fatalf("incorrect request summary: %s", output.String())
				}
				actions := entry["actions"].(map[string]any)
				if actions["drop_signature"] != float64(3) ||
					actions["replace_with_gemini_bypass"] != float64(1) {
					t.Fatalf("incorrect action totals: %v", actions)
				}
				positions := entry["positions"].([]any)
				if len(positions) != 4 {
					t.Fatalf("positions = %v", positions)
				}
				for index, position := range positions {
					fields := position.(map[string]any)
					wantContent, wantPart := float64(0), float64(index)
					if index == 3 {
						wantContent, wantPart = 1, 0
					}
					if fields["content_index"] != wantContent || fields["part_index"] != wantPart ||
						fields["has_signature"] != true ||
						fields["signature_length"].(float64) <= 0 ||
						fields["block_kind"] == "" ||
						fields["reason"] == "" {
						t.Fatalf("position %d = %v", index, fields)
					}
				}
				if positions[2].(map[string]any)["detected_provider"] != "gpt" ||
					positions[3].(map[string]any)["detected_provider"] != "gemini" {
					t.Fatalf("provider attribution lost: %v", positions)
				}
				for _, raw := range []string{
					"fixture-primary-signature", "fixture-sibling-signature", foreignSignature, geminiSignature,
					"retained thought",
				} {
					if strings.Contains(output.String(), raw) {
						t.Fatalf("signature or request content leaked in log: %q", raw)
					}
				}
				if gjson.GetBytes(
					got, path+".0.parts.0.thoughtSignature",
				).String() != GeminiSkipThoughtSignatureValidator || gjson.GetBytes(
					got, path+".0.parts.1.thoughtSignature",
				).Exists() || gjson.GetBytes(
					got, path+".0.parts.2.text",
				).String() != "retained thought" || gjson.GetBytes(
					got, path+".1.parts.0.functionResponse.response.value",
				).Int() != 1 {
					t.Fatalf("sanitation semantics changed: %s", got)
				}
				t.Logf("synthetic_summary=%s", bytes.TrimSpace(output.Bytes()))
			},
		)
	}
}

func TestGeminiSanitizeSummaryPositionsBounded(t *testing.T) {
	output := captureGeminiSanitizeLog(t)
	var parts []string
	for index := range 40 {
		parts = append(
			parts, fmt.Sprintf(
				`{"functionResponse":{"name":"tool_%d","response":{}},"thoughtSignature":"fixture-private-signature"}`,
				index,
			),
		)
	}
	body := []byte(`{"request":{"contents":[{"role":"user","parts":[` + strings.Join(parts, ",") + `]}]}}`)
	SanitizeGeminiRequestThoughtSignatures(body, "request.contents")
	entry := decodeGeminiSanitizeLog(t, output)
	if entry["sanitized_count"] != float64(40) || entry["dropped_signatures"] != float64(40) ||
		entry["omitted_positions"] != float64(24) ||
		len(entry["positions"].([]any)) != 16 {
		t.Fatalf("bounded summary lost totals: %s", output.String())
	}
	if strings.Contains(output.String(), "fixture-private-signature") {
		t.Fatal("private signature appeared in summary")
	}
	t.Logf("synthetic_bounded_summary=%s", bytes.TrimSpace(output.Bytes()))
}

func TestGeminiSanitizeRoutineBypassSiblingIsQuiet(t *testing.T) {
	for _, bypass := range []string{
		GeminiSkipThoughtSignatureValidator, geminiContextEngineeringBypass,
		"gemini#" + GeminiSkipThoughtSignatureValidator,
	} {
		t.Run(
			bypass, func(t *testing.T) {
				output := captureGeminiSanitizeLog(t)
				body := []byte(`{"contents":[{"role":"model","parts":[` +
					(`{"functionCall":{"name":"first","args":{}},"thoughtSignature":` +
						`"`) +
					GeminiSkipThoughtSignatureValidator +
					`"},` +
					`{"functionCall":{"name":"second","args":{}},"thoughtSignature":"` + bypass + `"}` +
					`]}]}`)
				got := SanitizeGeminiRequestThoughtSignatures(body, "contents")
				if output.Len() != 0 || gjson.GetBytes(got, "contents.0.parts.1.thoughtSignature").Exists() {
					t.Fatalf("routine sibling cleanup should be quiet: log=%s body=%s", output.String(), got)
				}
			},
		)
	}
}

func TestGeminiSanitizeSummaryIsRequestScoped(t *testing.T) {
	output := captureGeminiSanitizeLog(t)
	body := []byte((`{"contents":[{"role":"model","parts":[{"functionCall":{"name":"first",` +
		`"args":{}},"thoughtSignature":"fixture-invalid"}]}]}`))
	for range 2 {
		SanitizeGeminiRequestThoughtSignatures(body, "contents")
	}
	lines := bytes.Split(bytes.TrimSpace(output.Bytes()), []byte("\n"))
	if len(lines) != 2 {
		t.Fatalf("want two independent request summaries, got %s", output.String())
	}
	for _, line := range lines {
		var entry map[string]any
		if err := json.Unmarshal(
			line, &entry,
		); err != nil || entry["sanitized_count"] != float64(1) || entry["replaced_signatures"] != float64(1) {
			t.Fatalf("request totals accumulated: err=%v line=%s", err, line)
		}
	}
}

func TestGeminiSanitizeLoggingLevelDoesNotChangePayload(t *testing.T) {
	output := captureGeminiSanitizeLog(t)
	body := []byte((`{"contents":[{"role":"model","parts":[{"functionCall":{"name":"first",` +
		`"args":{}},"thoughtSignature":"fixture-invalid"}]}]}`))
	debugBody := SanitizeGeminiRequestThoughtSignatures(body, "contents")
	output.Reset()
	log.SetLevel(log.InfoLevel)
	infoBody := SanitizeGeminiRequestThoughtSignatures(body, "contents")
	if !bytes.Equal(debugBody, infoBody) || output.Len() != 0 {
		t.Fatalf("log level changed sanitation: debug=%s info=%s log=%s", debugBody, infoBody, output.String())
	}
}

func TestGeminiSanitizeForeignPrefixedBypassIsDiagnostic(t *testing.T) {
	output := captureGeminiSanitizeLog(t)
	body := []byte(`{"contents":[{"role":"model","parts":[` +
		`{"functionCall":{"name":"first","args":{}},"thoughtSignature":"skip_thought_signature_validator"},` +
		`{"functionCall":{"name":"second","args":{}},"thoughtSignature":"openai#skip_thought_signature_validator"}` +
		`]}]}`)
	SanitizeGeminiRequestThoughtSignatures(body, "contents")
	entry := decodeGeminiSanitizeLog(t, output)
	positions := entry["positions"].([]any)
	if entry["sanitized_count"] != float64(1) || entry["dropped_signatures"] != float64(1) ||
		len(positions) != 1 ||
		positions[0].(map[string]any)["detected_provider"] != "unknown" {
		t.Fatalf("foreign prefix was treated as a native bypass: %s", output.String())
	}
}
