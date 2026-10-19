package auth

import (
	"fmt"
	"testing"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/registry"
	cliproxyexecutor "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
)

func TestResolvedGeminiNativeSearchMetadataAttemptIsolation(t *testing.T) {
	for _, credential := range []struct {
		provider string
		kind     string
	}{
		{provider: "gemini", kind: AuthKindAPIKey},
		{provider: "vertex", kind: AuthKindAPIKey},
		{provider: "vertex", kind: AuthKindOAuth},
		{provider: "gemini-cli", kind: AuthKindOAuth},
		{provider: "aistudio", kind: AuthKindOAuth},
	} {
		for _, prefix := range []string{"", "tenant"} {
			for _, supported := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/%s/prefix=%s/search=%t", credential.provider, credential.kind, prefix, supported), func(t *testing.T) {
					const upstream = "gemini-2.5-flash"
					public := "native-search-attempt-fixture"
					if prefix != "" {
						public = prefix + "/" + public
					}
					auth := &Auth{ID: "native-search-attempt-" + credential.provider, Provider: credential.provider, Prefix: prefix,
						Attributes: map[string]string{AttributeAuthKind: credential.kind}}
					if credential.kind == AuthKindAPIKey {
						auth.Attributes[AttributeAPIKey] = "fixture-key"
					}
					catalog := registry.GetGlobalRegistry()
					catalog.RegisterClient(auth.ID, credential.provider, []*registry.ModelInfo{{
						ID: public, CatalogName: upstream, NativeCapabilities: &registry.NativeCapabilities{WebSearch: &supported},
					}})
					t.Cleanup(func() { catalog.UnregisterClient(auth.ID) })
					original := cliproxyexecutor.Request{Model: upstream, Metadata: map[string]any{"fixture": "unchanged"}}
					bound, err := attachResolvedAPIKeyModelInfo(original, auth, public, upstream)
					if err != nil {
						t.Fatal(err)
					}
					selected, ok := ResolvedModelInfo(bound)
					if !ok || selected.ID != upstream || selected.NativeCapabilities == nil || selected.NativeCapabilities.WebSearch == nil || *selected.NativeCapabilities.WebSearch != supported {
						t.Fatalf("selected model metadata = (%+v, %t)", selected, ok)
					}
					if len(original.Metadata) != 1 || original.Metadata["fixture"] != "unchanged" {
						t.Fatalf("binding mutated caller metadata: %+v", original.Metadata)
					}
					if _, apiKey := ResolvedAPIKeyModelInfo(bound); apiKey != (credential.kind == AuthKindAPIKey) {
						t.Fatalf("API-key accessor claimed an OAuth binding: %t", apiKey)
					}
					changed, err := attachResolvedAPIKeyModelInfo(bound, auth, public, "different-gemini-upstream")
					if err != nil {
						t.Fatal(err)
					}
					if stale, ok := ResolvedModelInfo(changed); ok || stale != nil {
						t.Fatalf("previous upstream metadata remained bound: %+v", stale)
					}
					if retained, ok := ResolvedModelInfo(bound); !ok || retained.ID != upstream {
						t.Fatalf("clearing a new attempt mutated the previous attempt: %+v", retained)
					}
					*selected.NativeCapabilities.WebSearch = !supported
					registered := catalog.GetModelInfoForClient(auth.ID, public)
					if registered == nil || registered.NativeCapabilities == nil || registered.NativeCapabilities.WebSearch == nil || *registered.NativeCapabilities.WebSearch != supported {
						t.Fatalf("selected pointer mutation reached registered metadata: %+v", registered)
					}
					unbound, err := attachResolvedAPIKeyModelInfo(bound, nil, public, upstream)
					if err != nil {
						t.Fatal(err)
					}
					if stale, ok := ResolvedModelInfo(unbound); ok || stale != nil {
						t.Fatalf("model metadata remained bound without a selected credential: %+v", stale)
					}
				})
			}
		}
	}
}
