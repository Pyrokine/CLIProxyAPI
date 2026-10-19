package registry

import "testing"

func TestModelRegistryConfigurationUpdatesRequireEveryExactRoute(t *testing.T) {
	reg := newTestModelRegistry()
	reg.RegisterClient("capable", "codex", []*ModelInfo{
		{ID: "shared", SupportConfigurationUpdate: true},
		{ID: "alias", CatalogName: "upstream-capable", SupportConfigurationUpdate: true},
		{ID: "team/alias", CatalogName: "upstream-capable", SupportConfigurationUpdate: true},
		{ID: "missing-info", SupportConfigurationUpdate: true},
	})
	reg.RegisterClient("incapable", "codex", []*ModelInfo{{ID: "shared"}})
	reg.RegisterClient("missing-metadata", "codex", []*ModelInfo{{ID: "missing-info"}})
	delete(reg.clientModelInfos["missing-metadata"], "missing-info")
	for _, test := range []struct {
		id   string
		want bool
	}{
		{id: "shared"},
		{id: "alias", want: true},
		{id: "team/alias", want: true},
		{id: " alias ", want: true},
		{id: "upstream-capable"},
		{id: "missing-info"},
		{id: "unknown"},
		{id: ""},
	} {
		if got := reg.SupportsConfigurationUpdates(test.id); got != test.want {
			t.Errorf("%q configuration updates = %t, want %t", test.id, got, test.want)
		}
	}
	reg.UnregisterClient("incapable")
	if !reg.SupportsConfigurationUpdates("shared") {
		t.Fatal("supported route remained disabled after the unsupported route was removed")
	}
	var missing *ModelRegistry
	if missing.SupportsConfigurationUpdates("alias") {
		t.Fatal("nil registry advertised configuration updates")
	}
}
