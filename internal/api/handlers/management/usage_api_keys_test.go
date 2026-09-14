package management

import (
	"context"
	"testing"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/util"
	coreusage "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/usage"
)

func TestUsageAPIKeyPresenterIncludesEveryConfiguredKeySource(t *testing.T) {
	apiKey := "api-key-source"
	aliasKey := "alias-key-source"
	ruleKey := "rule-key-source"
	presenter := newUsageAPIKeyPresenter(
		&config.Config{
			APIKeys:       []string{apiKey},
			APIKeyAliases: map[string]string{aliasKey: "alias"},
			APIKeyRules:   map[string]config.APIKeyRule{ruleKey: {PolicyProfile: "default"}},
		},
	)

	tests := []struct {
		raw   string
		alias string
	}{
		{raw: apiKey},
		{raw: aliasKey, alias: "alias"},
		{raw: ruleKey},
	}
	for _, test := range tests {
		for _, value := range []string{test.raw, coreusage.HashAPIKeyIdentity(test.raw)} {
			presentation := presenter.present(value)
			if presentation.display != util.HideAPIKey(test.raw) || presentation.alias != test.alias {
				t.Fatalf("Present(%q) = %+v", value, presentation)
			}
		}
	}
}

func TestUsageAPIKeyPresenterExpandsLiteralAndMaskedFilters(t *testing.T) {
	literalMask := "abcd...wxyz"
	maskedKey := "abcd-secret-wxyz"
	commaA := "ab,c-first-zzzz"
	commaB := "ab,c-second-zzzz"
	presenter := newUsageAPIKeyPresenter(
		&config.Config{
			APIKeys: []string{literalMask, maskedKey, "(none)", commaA, commaB},
		},
	)

	_, expanded := presenter.expandFilters(literalMask, nil)
	want := map[string]bool{
		coreusage.HashAPIKeyIdentity(literalMask): true,
		coreusage.HashAPIKeyIdentity(maskedKey):   true,
	}
	if len(expanded) != len(want) {
		t.Fatalf("literal and masked expansion = %#v", expanded)
	}
	for _, identity := range expanded {
		if !want[identity] {
			t.Fatalf("unexpected identity %q in %#v", identity, expanded)
		}
	}

	_, expanded = presenter.expandFilters("(none)", nil)
	want = map[string]bool{
		"(none)":                               true,
		coreusage.HashAPIKeyIdentity("(none)"): true,
	}
	if len(expanded) != len(want) {
		t.Fatalf("none expansion = %#v", expanded)
	}
	for _, identity := range expanded {
		if !want[identity] {
			t.Fatalf("unexpected none identity %q in %#v", identity, expanded)
		}
	}

	mask := util.HideAPIKey(commaA)
	_, expanded = presenter.expandFilters("", []string{mask + "#2", mask, mask})
	if len(expanded) != 2 {
		t.Fatalf("ordinal and repeated expansion = %#v", expanded)
	}
	if expanded[0] != coreusage.HashAPIKeyIdentity(commaB) ||
		expanded[1] != coreusage.HashAPIKeyIdentity(commaA) {
		t.Fatalf("ordinal and repeated expansion order = %#v", expanded)
	}
}

func TestPresentRedactedUsageAPIKeysRestoresConfiguredDisplaysWithoutCollisionLoss(t *testing.T) {
	rawA := "presenter-key-97"
	rawB := "presenter-key-424"
	identityA := coreusage.HashAPIKeyIdentity(rawA)
	identityB := coreusage.HashAPIKeyIdentity(rawB)
	if util.HideAPIKey(identityA) != util.HideAPIKey(identityB) {
		t.Fatalf("invalid identity collision fixture: %q/%q", identityA, identityB)
	}
	presenter := newUsageAPIKeyPresenter(
		&config.Config{
			APIKeys: []string{rawA, rawB},
		},
	)
	payload := map[string]any{
		"usage": map[string]any{
			"apis": map[string]any{
				identityA: map[string]any{"api_key": identityA},
				identityB: map[string]any{"api_key": identityB},
			},
		},
	}

	redacted, err := redactedManagementPayload(payload)
	if err != nil {
		t.Fatalf("redactedManagementPayload: %v", err)
	}
	if err = presentRedactedUsageAPIKeysContext(context.Background(), redacted, presenter); err != nil {
		t.Fatalf("presentRedactedUsageAPIKeysContext: %v", err)
	}
	usagePayload := redacted.(map[string]any)["usage"].(map[string]any)
	apis := usagePayload["apis"].(map[string]any)
	if len(apis) != 2 {
		t.Fatalf("presented API keys = %#v", apis)
	}
	for raw, identity := range map[string]string{rawA: identityA, rawB: identityB} {
		display := util.HideAPIKey(raw)
		entry, ok := apis[display].(map[string]any)
		if !ok {
			t.Fatalf("missing display %q in %#v", display, apis)
		}
		if entry["api_key"] != display {
			t.Fatalf("entry %q API key = %#v, want %q for %q", display, entry["api_key"], display, identity)
		}
	}
}
