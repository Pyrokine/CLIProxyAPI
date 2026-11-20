package handlers

import (
	"fmt"
	"testing"

	coreexecutor "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/translator"
)

func TestMediaAPIKeyRequirementOnlyAppliesToExplicitPublicImageTools(t *testing.T) {
	for _, test := range []struct {
		format, tool, model string
		want                bool
	}{
		{"openai-response", "image_generation", "gpt-image-2.6-flare", true},
		{"codex", "image_generation", "gpt-image-2.6-sunburst", true},
		{"openai-response", "image_generation", "gpt-image-2.5-flare", false},
		{"openai-response", "image_generation", "", false},
		{"openai-response", "function", "gpt-image-2.6-flare", false},
		{"openai-response", "image_generation", "muse-image-1.0", false},
		{"openai", "image_generation", "gpt-image-2.6-flare", false},
		{"gemini", "image_generation", "gpt-image-2.6-flare", false},
	} {
		t.Run(
			test.format+"/"+test.tool+"/"+test.model, func(t *testing.T) {
				req := coreexecutor.Request{
					Model:   "gpt-6.1-sol",
					Payload: []byte(fmt.Sprintf(`{"tools":[{"type":%q,"model":%q}]}`, test.tool, test.model)),
				}
				opts := coreexecutor.Options{
					SourceFormat: translator.FromString(test.format),
					Metadata:     map[string]any{coreexecutor.PinnedAuthMetadataKey: "selected-credential"},
				}
				setImageCredentialRequirement(req, &opts)
				if required, _ := opts.Metadata[coreexecutor.RequireAPIKeyAuthMetadataKey].(bool); required != test.want {
					t.Fatalf("media API-key requirement=%t want=%t", required, test.want)
				}
				if opts.Metadata[coreexecutor.PinnedAuthMetadataKey] != "selected-credential" {
					t.Fatal("media requirement changed the pinned credential")
				}
			},
		)
	}
}

func TestPublicImage26UsesImageOnlyEntryAndExistingPrefixRules(t *testing.T) {
	handler := &BaseAPIHandler{}
	for _, model := range []string{
		"gpt-image-2.6-flare", "gpt-image-2.6-sunburst", "codex/gpt-image-2.6-flare", "openai/gpt-image-2.6-sunburst",
	} {
		if canonicalImageOnlyModel(model) == "" || !allowsBuiltinImageModelBareFallback(model) {
			t.Fatalf("public image route missing: %s", model)
		}
		if handler.validateImageOnlyModel(model, false) == nil || handler.validateImageOnlyModel(model, true) != nil {
			t.Fatalf("public image-only entry restriction lost: %s", model)
		}
	}
	if allowsBuiltinImageModelBareFallback("xai/gpt-image-2.6-flare") {
		t.Fatal("public image inherited another provider prefix")
	}
	if handler.validateImageOnlyModel("muse-image-1.0", false) != nil {
		t.Fatal("Meta Image Responses top-level model rejected")
	}
}
