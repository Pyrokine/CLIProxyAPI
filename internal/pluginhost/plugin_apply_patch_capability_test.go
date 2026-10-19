package pluginhost

import (
	"testing"

	coreauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
)

type patchAwareCompatExecutor struct {
	coreauth.ProviderExecutor
	supported bool
	scoped    coreauth.ProviderExecutor
}

func (e *patchAwareCompatExecutor) SupportsApplyPatch() bool { return e != nil && e.supported }

func (e *patchAwareCompatExecutor) ForAPIKey() coreauth.ProviderExecutor { return e.scoped }

func TestPluginRefreshCompatApplyPatchCapability(t *testing.T) {
	for _, tc := range []struct {
		name  string
		inner coreauth.ProviderExecutor
		want  bool
	}{
		{name: "missing"},
		{name: "no contract", inner: &stubRefreshCompatExecutor{}},
		{name: "disabled", inner: &patchAwareCompatExecutor{}},
		{name: "enabled", inner: &patchAwareCompatExecutor{supported: true}, want: true},
		{name: "nil receiver", inner: (*patchAwareCompatExecutor)(nil)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			wrapped := &pluginRefreshCompatExecutor{inner: tc.inner}
			if got := wrapped.SupportsApplyPatch(); got != tc.want {
				t.Fatalf("support = %t, want %t", got, tc.want)
			}
		})
	}
	inner := &patchAwareCompatExecutor{supported: true, scoped: &patchAwareCompatExecutor{supported: false}}
	wrapped := &pluginRefreshCompatExecutor{inner: inner, host: New()}
	view, ok := wrapped.ForAPIKey().(coreauth.ApplyPatchSupport)
	if !ok || view.SupportsApplyPatch() {
		t.Fatal("API-key view did not retain its own capability")
	}
	if !wrapped.SupportsApplyPatch() {
		t.Fatal("scoped view changed original capability")
	}
	var absent *pluginRefreshCompatExecutor
	if absent.SupportsApplyPatch() {
		t.Fatal("nil wrapper declared support")
	}
}
