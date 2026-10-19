package executor

import (
	"net/http"
	"testing"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	"github.com/tidwall/gjson"
)

func TestPayloadTracksIndependentPathOwnership(t *testing.T) {
	models := []config.PayloadModelRule{{Name: "test-model", Protocol: "claude"}}
	for _, kind := range []string{"default", "default-raw", "override", "override-raw", "filter"} {
		t.Run(kind, func(t *testing.T) {
			cfg := &config.Config{}
			rule := config.PayloadRule{Models: models, Params: map[string]any{"diagnostics": true, "context_management.mode": "keep"}}
			payload := []byte(`{}`)
			switch kind {
			case "default":
				cfg.Payload.Default = []config.PayloadRule{rule}
			case "default-raw":
				rule.Params = map[string]any{"diagnostics": `true`, "context_management.mode": `"keep"`}
				cfg.Payload.DefaultRaw = []config.PayloadRule{rule}
			case "override":
				payload = []byte(`{"diagnostics":true,"context_management":{"mode":"keep"}}`)
				cfg.Payload.Override = []config.PayloadRule{rule}
			case "override-raw":
				rule.Params = map[string]any{"diagnostics": `true`, "context_management.mode": `"keep"`}
				cfg.Payload.OverrideRaw = []config.PayloadRule{rule}
			case "filter":
				cfg.Payload.Filter = []config.PayloadFilterRule{{Models: models, Params: []string{"diagnostics", "context_management.mode"}}}
			}
			out, touched := applyPayloadConfigWithRootTracked(cfg, "test-model", "claude", "", payload, payload, "", "claude", nil, "diagnostics", "context_management", "untouched")
			if !touched["diagnostics"] || !touched["context_management"] || touched["untouched"] {
				t.Fatalf("ownership = %v", touched)
			}
			if kind != "filter" && (!gjson.GetBytes(out, "diagnostics").Bool() || gjson.GetBytes(out, "context_management.mode").String() != "keep") {
				t.Fatalf("unexpected payload: %s", out)
			}
		})
	}
}

func TestPayloadSkippedDefaultDoesNotOwnDiagnostics(t *testing.T) {
	cfg := &config.Config{Payload: config.PayloadConfig{Default: []config.PayloadRule{{
		Models: []config.PayloadModelRule{{Name: "test-model"}}, Params: map[string]any{"diagnostics": false},
	}}}}
	payload := []byte(`{"diagnostics":true}`)
	out, touched := applyPayloadConfigWithRootTracked(cfg, "test-model", "claude", "", payload, payload, "", "claude", nil, "diagnostics")
	if touched["diagnostics"] || string(out) != string(payload) {
		t.Fatal("skipped default acquired ownership or changed caller data")
	}
}

func applyPayloadConfigWithRoot(
	cfg *config.Config,
	model, protocol, root string,
	payload, original []byte,
	requestedModel, targetExecutor string,
	headers http.Header,
) []byte {
	out, _ := applyPayloadConfigWithRootTracked(
		cfg,
		model,
		protocol,
		root,
		payload,
		original,
		requestedModel,
		targetExecutor,
		headers,
		"",
	)
	return out
}

func applyPayloadConfigWithRootTracked(
	cfg *config.Config,
	model, protocol, root string,
	payload, original []byte,
	requestedModel, targetExecutor string,
	headers http.Header,
	trackedPaths ...string,
) ([]byte, map[string]bool) {
	return applyPayloadConfigWithRequestTracked(
		cfg,
		model,
		protocol,
		"",
		root,
		payload,
		original,
		requestedModel,
		"",
		headers,
		targetExecutor,
		trackedPaths...,
	)
}

func TestPayloadFilterOwnsAbsentQueryTarget(t *testing.T) {
	cfg := &config.Config{Payload: config.PayloadConfig{Filter: []config.PayloadFilterRule{{Models: []config.PayloadModelRule{{Name: "*"}}, Params: []string{`tools.#(type=="image_generation")#`}}}}}
	payload := []byte(`{"input":"synthetic"}`)
	out, touched := applyPayloadConfigWithRequestTracked(cfg, "gpt-5.4", "codex", "codex", "", payload, payload, "", "", nil, "codex", "tools", "input")
	if !touched["tools"] || touched["input"] || string(out) != string(payload) {
		t.Fatalf("absent filter ownership=%v payload=%s", touched, out)
	}
}
