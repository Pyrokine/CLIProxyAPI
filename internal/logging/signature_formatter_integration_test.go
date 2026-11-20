package logging_test

import (
	"bytes"
	"strings"
	"testing"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/logging"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/signature"
	log "github.com/sirupsen/logrus"
)

func TestGeminiSignatureSanitizeUsesApplicationFormatter(t *testing.T) {
	logger := log.StandardLogger()
	oldOutput, oldFormatter, oldLevel := logger.Out, logger.Formatter, logger.GetLevel()
	var output bytes.Buffer
	logger.SetOutput(&output)
	logger.SetFormatter(&logging.LogFormatter{})
	logger.SetLevel(log.DebugLevel)
	t.Cleanup(
		func() {
			logger.SetOutput(oldOutput)
			logger.SetFormatter(oldFormatter)
			logger.SetLevel(oldLevel)
		},
	)
	const part = `{"functionResponse":{"name":"fixture","response":{}},"thoughtSignature":"fixture-private-signature"}`
	body := []byte(`{"contents":[{"role":"user","parts":[` + strings.TrimSuffix(
		strings.Repeat(part+",", 20), ",",
	) + `]}]}`)
	cleaned := signature.SanitizeGeminiRequestThoughtSignatures(body, "contents")
	text := output.String()
	if strings.Count(text, "\n") != 1 {
		t.Fatalf("one cleanup produced multiple application log lines: %q", text)
	}
	for _, expected := range []string{
		"component=signature_sanitizer", "target_provider=gemini", "sanitized_count=20",
		"dropped_signatures=20", "actions=map[drop_signature:20]", "positions=[map[",
		"content_index:0", "part_index:15", "omitted_positions=4",
	} {
		if !strings.Contains(text, expected) {
			t.Errorf("application formatter dropped %q: %s", expected, text)
		}
	}
	if strings.Contains(text, "fixture-private-signature") || bytes.Contains(
		cleaned, []byte("fixture-private-signature"),
	) {
		t.Fatalf("signature was logged or retained after cleanup: log=%s body=%s", text, cleaned)
	}
}
