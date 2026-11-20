package auth

import (
	"net/http"
	"testing"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/registry"
	core "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
)

func TestAuditModelContextCapabilityUsesEitherLimit(t *testing.T) {
	for _, tc := range []struct {
		name string
		info *registry.ModelInfo
		want bool
	}{
		{"missing", nil, false},
		{"small", &registry.ModelInfo{ContextLength: 999_999, MaxContextLength: 128_000}, false},
		{"context", &registry.ModelInfo{ContextLength: 1_000_000, MaxContextLength: 128_000}, true},
		{"maximum", &registry.ModelInfo{ContextLength: 128_000, MaxContextLength: 1_000_000}, true},
	} {
		t.Run(
			tc.name, func(t *testing.T) {
				if got := ModelSupportsContext1M(tc.info); got != tc.want {
					t.Fatalf("supports=%t want=%t", got, tc.want)
				}
			},
		)
	}
}

func TestAuditFinalModelContextBindsExactCredentialSnapshot(t *testing.T) {
	reg := registry.GetGlobalRegistry()
	credential := &Auth{
		ID: t.Name(), Provider: "openai-compatible", Attributes: map[string]string{"api_key": "synthetic-key"},
	}
	reg.RegisterClient(
		credential.ID, credential.Provider, []*registry.ModelInfo{
			{ID: "public-high", CatalogName: "high-upstream", ContextLength: 1_000_000},
			{ID: "public-small", CatalogName: "gpt-4.1-mini", ContextLength: 128_000},
			{
				ID: "public-other", CatalogName: "other-upstream", ContextLength: 128_000, MaxContextLength: 1_000_000,
				IsCompat: true,
			},
		},
	)
	t.Cleanup(func() { reg.UnregisterClient(credential.ID) })
	foreignID := t.Name() + "-foreign"
	reg.RegisterClient(
		foreignID, credential.Provider, []*registry.ModelInfo{{ID: "foreign-upstream", ContextLength: 2_000_000}},
	)
	t.Cleanup(func() { reg.UnregisterClient(foreignID) })
	selected := &registry.ModelInfo{ID: "high-upstream", ContextLength: 1_000_000}
	original := attachResolvedModelInfo(
		core.Request{
			Model: "high-upstream[1m]", Metadata: map[string]any{"caller": "retained"},
		}, resolvedAPIKeyModelInfoMetadataKey, selected,
	)
	for _, tc := range []struct {
		name      string
		final     string
		requested string
		auth      *Auth
		wantError bool
	}{
		{"unchanged", "high-upstream", "public-high[1m]", credential, false},
		{"new-capable", "other-upstream", "public-high[1m]", credential, false},
		{"small-selected-credential", "gpt-4.1-mini", "public-high[1m]", credential, true},
		{"unknown", "unknown-upstream", "public-high[1m]", credential, true},
		{"other-credential", "foreign-upstream", "public-high[1m]", credential, true},
		{"no-context-tag", "gpt-4.1-mini", "public-high", credential, false},
		{
			"home-changed-target", "other-upstream", "public-high[1m]",
			&Auth{ID: credential.ID, Attributes: map[string]string{homeUpstreamModelAttributeKey: "high-upstream"}},
			true,
		},
	} {
		t.Run(
			tc.name, func(t *testing.T) {
				bound, err := BindFinalModelContext(original, tc.auth, tc.requested, tc.final)
				if (err != nil) != tc.wantError {
					t.Fatalf("error=%v expected rejection=%t", err, tc.wantError)
				}
				if err != nil {
					status, ok := err.(interface{ StatusCode() int })
					if !ok || status.StatusCode() != http.StatusBadRequest {
						t.Fatalf("status error=%v", err)
					}
					return
				}
				info, ok := ResolvedModelInfo(bound)
				wantID := "high-upstream"
				if tc.name == "new-capable" {
					wantID = "other-upstream"
				}
				if !ok || info == nil || info.ID != wantID || bound.Metadata["caller"] != "retained" {
					t.Fatalf("bound=%+v metadata=%v", info, bound.Metadata)
				}
				if tc.name == "new-capable" && (!info.IsCompat || info.MaxContextLength != 1_000_000) {
					t.Fatalf("final capabilities=%+v", info)
				}
				if originalInfo,
					_ := ResolvedModelInfo(original); originalInfo == nil || originalInfo.ID != "high-upstream" {
					t.Fatalf("original snapshot changed=%+v", originalInfo)
				}
			},
		)
	}
	if info := reg.GetModelInfoForClient(
		credential.ID, "public-other",
	); info == nil || info.ID != "public-other" || info.CatalogName != "other-upstream" {
		t.Fatalf("published model changed=%+v", info)
	}
	legacy := core.Request{Model: "legacy-model[1m]"}
	if bound, err := BindFinalModelContext(
		legacy, nil, legacy.Model, "legacy-model",
	); err != nil || bound.Metadata != nil {
		t.Fatalf("unchanged direct-executor contract: request=%+v error=%v", bound, err)
	}
}
