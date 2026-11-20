package models

import (
	"reflect"
	"testing"
)

func TestCatalogSortsOriginalClaudeModelInputs(t *testing.T) {
	for _, tc := range []struct {
		name   string
		models []map[string]any
		ids    []string
	}{
		{
			"Claude handler", []map[string]any{
				{"id": "claude-fable-5-dd-b", "display_name": "Zebra"},
				{"id": "claude-a", "display_name": "Alpha"},
				{"id": "claude-c", "display_name": "Alpha"},
				{"id": "claude-fable-5-dd-d", "display_name": "Beta"},
			}, []string{"claude-a", "claude-c", "claude-fable-5-dd-d", "claude-fable-5-dd-b"},
		},
		{
			"Home handler", []map[string]any{
				{"id": "claude-z", "display_name": "Zebra"},
				{"id": "gpt-4o", "display_name": "Alpha"},
				{"id": "claude-b", "display_name": "Beta"},
			}, []string{"gpt-4o", "claude-b", "claude-z"},
		},
	} {
		t.Run(
			tc.name, func(t *testing.T) {
				for _, disabled := range []bool{false, true} {
					data := responseModels(t, NewCatalog(tc.models, disabled).Response())
					if len(data) != len(tc.ids) {
						t.Fatalf("sorted model count = %d, want %d", len(data), len(tc.ids))
					}
					for index, id := range tc.ids {
						want := id
						if !disabled {
							want = ClientModelID(id)
						}
						if data[index]["id"] != want {
							t.Fatalf("model %d = %#v, want %q", index, data[index], want)
						}
					}
				}
			},
		)
	}
}

func TestPublishedLegacyModelPrefixHelpers(t *testing.T) {
	//goland:noinspection SpellCheckingInspection
	for _, tt := range []struct{ id, encoded string }{
		{"", ""},
		{"claude-sonnet-4-6", "claude-sonnet-4-6"},
		{"my-claude-custom", "claude-fable-5-dd-motsuc-edualc-ym"},
		{"Claude-Opus-4", "claude-fable-5-dd-4-supO-edualC"},
		{"gpt-4o", "claude-fable-5-dd-o4-tpg"},
		{"gemini-2.5-pro", "claude-fable-5-dd-orp-5.2-inimeg"},
		{"custom-model-x", "claude-fable-5-dd-x-ledom-motsuc"},
		{"模型/alpha", "claude-fable-5-dd-ahpla/型模"},
	} {
		t.Run(
			tt.id, func(t *testing.T) {
				if got := EnsureClaudeModelIDPrefix(tt.id); got != tt.encoded {
					t.Fatalf("encoded=%q want=%q", got, tt.encoded)
				}
				if got := ResolveClaudeModelIDPrefix(tt.encoded); got != tt.id {
					t.Fatalf("resolved=%q want=%q", got, tt.id)
				}
				if tt.id != "" {
					if got := ResolveClaudeModelIDPrefix(tt.encoded + "(high)"); got != tt.id+"(high)" {
						t.Fatalf("resolved suffix=%q want=%q", got, tt.id+"(high)")
					}
				}
			},
		)
	}
	for _, id := range []string{"gpt-4o", clientModelPrefix, clientModelPrefix + "(high)"} {
		if got := ResolveClaudeModelIDPrefix(id); got != id {
			t.Fatalf("plain or empty legacy ID=%q resolved=%q", id, got)
		}
	}
}

func TestBuildResponsePreservesPublishedModelList(t *testing.T) {
	available := []map[string]any{
		{"id": "claude-z", "display_name": "Zebra", "max_tokens": 64000},
		{"id": "gpt-4o", "display_name": "Alpha"},
		{"id": "claude-c", "display_name": "Alpha"},
		{"id": "claude-b", "display_name": "Beta"},
	}
	for _, disabled := range []bool{false, true} {
		response := buildResponse(available, disabled)
		models := responseModels(t, response)
		wantIDs := []string{"claude-c", "gpt-4o", "claude-b", "claude-z"}
		if !disabled {
			wantIDs[1] = "claude-fable-5-dd-o4-tpg"
		}
		if len(models) != len(wantIDs) {
			t.Fatalf("model count=%d want=%d", len(models), len(wantIDs))
		}
		for index, want := range wantIDs {
			if got := models[index]["id"]; got != want {
				t.Fatalf("disabled=%t model[%d]=%q want=%q", disabled, index, got, want)
			}
		}
		if response["first_id"] != wantIDs[0] || response["last_id"] != wantIDs[3] || response["has_more"] != false ||
			models[3]["max_tokens"] != 64000 {
			t.Fatalf("model list fields changed: %#v", response)
		}
	}
	if available[0]["id"] != "claude-z" || available[1]["id"] != "gpt-4o" {
		t.Fatalf("input changed: %#v", available)
	}
	if got := buildResponse(nil, false); !reflect.DeepEqual(got, emptyResponse()) {
		t.Fatalf("empty response=%#v", got)
	}
}

func TestCatalogResolvesPublishedLegacyModelIDs(t *testing.T) {
	models := []map[string]any{
		{"id": "gpt-4o"},
		{"id": "gemini-2.5-pro"},
		{"id": "custom-model-x"},
		{"id": "模型/alpha"},
		{"id": "claude-fable-5-dd-provider-model"},
	}
	//goland:noinspection SpellCheckingInspection
	cases := []struct{ input, want string }{
		{"claude-fable-5-dd-o4-tpg", "gpt-4o"},
		{"claude-fable-5-dd-o4-tpg(high)", "gpt-4o(high)"},
		{"claude-fable-5-dd-orp-5.2-inimeg", "gemini-2.5-pro"},
		{"claude-fable-5-dd-x-ledom-motsuc", "custom-model-x"},
		{"claude-fable-5-dd-ahpla/型模", "模型/alpha"},
		{"claude-fable-5-dd-provider-model", "claude-fable-5-dd-provider-model"},
	}
	for _, disabled := range []bool{false, true} {
		catalog := NewCatalog(models, disabled)
		for _, tt := range cases {
			t.Run(
				tt.input, func(t *testing.T) {
					if got := catalog.ResolveModelID(tt.input); got != tt.want {
						t.Fatalf("disabled=%t input=%q resolved=%q want=%q", disabled, tt.input, got, tt.want)
					}
				},
			)
		}
	}
}
