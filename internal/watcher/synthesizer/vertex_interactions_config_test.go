package synthesizer_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/runtime/executor"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/watcher/synthesizer"
	cliproxyexecutor "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
	sdktranslator "github.com/Pyrokine/CLIProxyAPI/v6/sdk/translator"
)

func TestVertexConfigInteractionsRouting(t *testing.T) {
	for _, tc := range []struct {
		name string
		flag string
		want sdktranslator.Format
	}{
		{name: "enabled", flag: "  interactions: true\n", want: sdktranslator.FormatInteractions},
		{name: "disabled", flag: "  interactions: false\n"},
		{name: "omitted"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			raw := "port: 8317\nvertex-api-key:\n- api-key: synthetic-key\n  base-url: https://vertex-config.invalid\n" + tc.flag
			cfg, err := config.ParseConfigBytes([]byte(raw))
			if err != nil {
				t.Fatal(err)
			}
			auths, err := synthesizer.NewConfigSynthesizer().Synthesize(&synthesizer.SynthesisContext{
				Config: cfg, Now: time.Unix(1, 0), IDGenerator: synthesizer.NewStableIDGenerator(),
			})
			if err != nil || len(auths) != 1 || auths[0].Provider != "vertex" {
				t.Fatalf("synthesized auth count = %d, error = %v", len(auths), err)
			}
			vertex := executor.NewGeminiVertexExecutor(cfg)
			opts := cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatInteractions}
			if got := vertex.RequestToFormatForAuth(auths[0], cliproxyexecutor.Request{}, opts); got != tc.want {
				t.Fatalf("request format = %q, want %q; interactions attribute = %q", got, tc.want, auths[0].Attributes["interactions"])
			}
			for _, excluded := range []cliproxyexecutor.Options{
				{SourceFormat: sdktranslator.FormatInteractions, CountTokens: true},
				{SourceFormat: sdktranslator.FormatInteractions, Alt: "responses/compact"},
				{SourceFormat: sdktranslator.FormatOpenAI},
			} {
				if got := vertex.RequestToFormatForAuth(auths[0], cliproxyexecutor.Request{}, excluded); got != "" {
					t.Fatalf("unexpected native format for excluded operation: %q", got)
				}
			}
			data, err := json.Marshal(cfg)
			if err != nil {
				t.Fatal(err)
			}
			var document struct {
				Keys []map[string]any `json:"vertex-api-key"`
			}
			if err = json.Unmarshal(data, &document); err != nil || len(document.Keys) != 1 {
				t.Fatalf("serialized config entries = %d, error = %v", len(document.Keys), err)
			}
			value, present := document.Keys[0]["interactions"]
			if tc.flag == "" && present || tc.flag != "" && (!present || value != (tc.want != "")) {
				t.Fatalf("serialized interactions = %#v, present = %t", value, present)
			}
		})
	}
	if _, err := config.ParseConfigBytes([]byte("vertex-api-key:\n- api-key: synthetic-key\n  interactions: invalid\n")); err == nil {
		t.Fatal("invalid interactions value was accepted")
	}
}

func TestVertexConfigInteractionsSaveDisable(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	prefix := "port: 8317\nvertex-api-key:\n- api-key: synthetic-key\n  base-url: https://vertex-config.invalid\n"
	if err := os.WriteFile(path, []byte(prefix+"  interactions: true\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, flag := range []string{"  interactions: false\n", ""} {
		cfg, err := config.ParseConfigBytes([]byte(prefix + flag))
		if err != nil {
			t.Fatal(err)
		}
		if err = config.SaveConfigPreserveComments(path, cfg); err != nil {
			t.Fatal(err)
		}
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(data), "interactions: true") || flag != "" && !strings.Contains(string(data), "interactions: false") || flag == "" && strings.Contains(string(data), "interactions:") {
			t.Fatal("disabled interactions flag was not preserved")
		}
		cfg, err = config.LoadConfig(path)
		if err != nil {
			t.Fatal(err)
		}
		auths, err := synthesizer.NewConfigSynthesizer().Synthesize(&synthesizer.SynthesisContext{
			Config: cfg, Now: time.Unix(1, 0), IDGenerator: synthesizer.NewStableIDGenerator(),
		})
		if err != nil || len(auths) != 1 || auths[0].Attributes["interactions"] != "" {
			t.Fatalf("disabled config synthesized %d auths, error = %v", len(auths), err)
		}
	}
}

func TestVertexConfigInteractionsNewDisabledEntry(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte("port: 8317\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.ParseConfigBytes([]byte("port: 8317\nvertex-api-key:\n- api-key: synthetic-key\n  interactions: false\n"))
	if err != nil {
		t.Fatal(err)
	}
	if err = config.SaveConfigPreserveComments(path, cfg); err != nil {
		t.Fatal(err)
	}
	cfg, err = config.LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.VertexCompatAPIKey) != 1 || cfg.VertexCompatAPIKey[0].Interactions == nil || *cfg.VertexCompatAPIKey[0].Interactions {
		t.Fatal("new credential lost its explicit false interactions value")
	}
}
