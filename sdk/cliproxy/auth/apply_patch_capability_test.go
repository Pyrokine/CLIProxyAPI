package auth

import "testing"

type patchCapabilityExecutor struct {
	ProviderExecutor
	provider  string
	supported bool
}

func (e *patchCapabilityExecutor) Identifier() string {
	if e == nil {
		return "nil-provider"
	}
	return e.provider
}

func (e *patchCapabilityExecutor) SupportsApplyPatch() bool {
	return e != nil && e.supported
}

type opaquePatchExecutor struct{ ProviderExecutor }

func (*opaquePatchExecutor) Identifier() string { return "opaque" }

type scopedPatchCapabilityExecutor struct {
	*patchCapabilityExecutor
	view ProviderExecutor
}

func (e *scopedPatchCapabilityExecutor) ForAPIKey() ProviderExecutor { return e.view }

func TestManagerApplyPatchRequiresAPIKeyExecutorView(t *testing.T) {
	for _, tc := range []struct {
		name string
		view ProviderExecutor
		want bool
	}{
		{name: "supported view", view: &patchCapabilityExecutor{supported: true}, want: true},
		{name: "unsupported view", view: &patchCapabilityExecutor{}},
		{name: "missing view"},
		{name: "no contract view", view: &opaquePatchExecutor{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			manager := NewManager(nil, nil, nil)
			manager.RegisterExecutor(&scopedPatchCapabilityExecutor{
				patchCapabilityExecutor: &patchCapabilityExecutor{provider: "scoped", supported: true},
				view:                    tc.view,
			})
			if got := manager.SupportsApplyPatchForProviders([]string{"scoped"}); got != tc.want {
				t.Fatalf("scoped capability = %t, want %t", got, tc.want)
			}
		})
	}
}

func TestManagerSupportsApplyPatchForProviders(t *testing.T) {
	manager := NewManager(nil, nil, nil)
	manager.RegisterExecutor(&patchCapabilityExecutor{provider: "fixture-provider", supported: true})
	manager.RegisterExecutor(&patchCapabilityExecutor{provider: "codex", supported: false})
	manager.RegisterExecutor(&opaquePatchExecutor{})
	manager.RegisterExecutor((*patchCapabilityExecutor)(nil))
	for _, tc := range []struct {
		name      string
		providers []string
		want      bool
	}{
		{name: "empty"},
		{name: "explicit", providers: []string{"fixture-provider"}, want: true},
		{name: "trim and case", providers: []string{" FIXTURE-PROVIDER "}, want: true},
		{name: "duplicate", providers: []string{"fixture-provider", "fixture-provider"}, want: true},
		{name: "provider name is insufficient", providers: []string{"codex"}},
		{name: "mixed", providers: []string{"fixture-provider", "codex"}},
		{name: "no optional contract", providers: []string{"opaque"}},
		{name: "missing", providers: []string{"missing"}},
		{name: "empty member", providers: []string{"fixture-provider", " "}},
		{name: "nil receiver", providers: []string{"nil-provider"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := manager.SupportsApplyPatchForProviders(tc.providers); got != tc.want {
				t.Fatalf("capability(%q) = %t, want %t", tc.providers, got, tc.want)
			}
		})
	}
	var absent *Manager
	if absent.SupportsApplyPatchForProviders([]string{"fixture-provider"}) {
		t.Fatal("nil manager declared support")
	}
}
