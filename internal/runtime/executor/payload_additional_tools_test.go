package executor

import (
	"bytes"
	"os"
	"strings"
	"testing"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
)

const codexAdditionalToolsFilterPath = `input.#(type=="additional_tools")#.tools.#(name=="functions")#.tools.#(name=="apply_patch")#`

func TestConfigExampleDocumentsCodexAdditionalToolsPayloadFilter(t *testing.T) {
	data, err := os.ReadFile("../../../config.example.yaml")
	if err != nil {
		t.Fatalf("read config.example.yaml: %v", err)
	}
	if !strings.Contains(string(data), "'"+codexAdditionalToolsFilterPath+"'") {
		t.Fatalf("config.example.yaml does not contain %q", codexAdditionalToolsFilterPath)
	}
}

func TestApplyPayloadConfigCodexAdditionalToolsFilter(t *testing.T) {
	cfg := &config.Config{Payload: config.PayloadConfig{Filter: []config.PayloadFilterRule{{
		Models: []config.PayloadModelRule{{Name: "gpt-*", Protocol: "codex"}},
		Params: []string{codexAdditionalToolsFilterPath},
	}}}}

	t.Run("multiple additional_tools items", func(t *testing.T) {
		input := []byte(`{"model":"gpt-5.6","input":[{"type":"additional_tools","tools":[{"type":"namespace","name":"functions","tools":[{"type":"custom","name":"apply_patch"}]}]},{"type":"message","role":"user","content":"hello"},{"type":"additional_tools","tools":[{"type":"namespace","name":"functions","tools":[{"type":"custom","name":"apply_patch"},{"type":"custom","name":"view_image"}]}]}]}`)
		output := applyPayloadConfigWithRoot(cfg, "gpt-5.6", "codex", "", input, nil, "", "codex", nil)
		if strings.Contains(string(output), "apply_patch") {
			t.Fatalf("apply_patch remained: %s", output)
		}
		for _, expected := range []string{"view_image", "hello"} {
			if !strings.Contains(string(output), expected) {
				t.Fatalf("%s was removed: %s", expected, output)
			}
		}
	})

	t.Run("multiple matches in one namespace", func(t *testing.T) {
		input := []byte(`{"model":"gpt-5.6","input":[{"type":"additional_tools","tools":[{"type":"namespace","name":"functions","tools":[{"type":"custom","name":"apply_patch","id":"p1"},{"type":"custom","name":"exec_command"},{"type":"custom","name":"apply_patch","id":"p2"}]}]}]}`)
		output := applyPayloadConfigWithRoot(cfg, "gpt-5.6", "codex", "", input, nil, "", "codex", nil)
		if strings.Contains(string(output), "apply_patch") || !strings.Contains(string(output), "exec_command") {
			t.Fatalf("unexpected filtered payload: %s", output)
		}
	})

	t.Run("no match preserves bytes", func(t *testing.T) {
		input := []byte(`{"model":"gpt-5.6","input":[{"type":"additional_tools","tools":[{"type":"namespace","name":"functions","tools":[{"type":"custom","name":"exec_command"}]}]}]}`)
		output := applyPayloadConfigWithRoot(cfg, "gpt-5.6", "codex", "", input, nil, "", "codex", nil)
		if !bytes.Equal(output, input) {
			t.Fatalf("payload changed without a match: %s", output)
		}
	})
}
