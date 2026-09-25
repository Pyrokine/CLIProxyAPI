package executor

import (
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
			out, touched := applyPayloadConfigWithRootTracked(cfg, "test-model", "claude", "", payload, payload, "", "diagnostics", "context_management", "untouched")
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
	out, touched := applyPayloadConfigWithRootTracked(cfg, "test-model", "claude", "", payload, payload, "", "diagnostics")
	if touched["diagnostics"] || string(out) != string(payload) {
		t.Fatal("skipped default acquired ownership or changed caller data")
	}
}
