package registry

import (
	"encoding/json"
	"testing"
	"time"
)

func TestModelRefreshIntervalCanBeDisabled(t *testing.T) {
	t.Parallel()

	for _, hours := range []int{-1, 0} {
		if interval, enabled := modelRefreshInterval(hours); enabled || interval != 0 {
			t.Fatalf("modelRefreshInterval(%d) = (%s, %v), want disabled", hours, interval, enabled)
		}
	}
	if interval, enabled := modelRefreshInterval(6); !enabled || interval != 6*time.Hour {
		t.Fatalf("modelRefreshInterval(6) = (%s, %v), want (6h, true)", interval, enabled)
	}
}

func TestMergeRemoteModelsCatalogKeepsLastKnownGoodSections(t *testing.T) {
	t.Parallel()

	current := &staticModelsJSON{
		Gemini: []*ModelInfo{{ID: "gemini-old"}},
		Qwen:   []*ModelInfo{{ID: "qwen-current"}},
		IFlow:  []*ModelInfo{{ID: "iflow-current"}},
	}
	remote := map[string]any{
		"gemini": []map[string]any{{"id": "gemini-new"}},
		"qwen":   []any{},
		"iflow":  []map[string]any{{"id": ""}},
	}
	data, err := json.Marshal(remote)
	if err != nil {
		t.Fatalf("json.Marshal() error = %v", err)
	}

	merged, err := mergeRemoteModelsCatalog(data, current)
	if err != nil {
		t.Fatalf("mergeRemoteModelsCatalog() error = %v", err)
	}
	assertOnlyModelID(t, merged.Gemini, "gemini-new")
	assertOnlyModelID(t, merged.Qwen, "qwen-current")
	assertOnlyModelID(t, merged.IFlow, "iflow-current")
	assertOnlyModelID(t, current.Gemini, "gemini-old")
}

func TestMergeRemoteModelsCatalogRejectsCatalogWithoutValidSections(t *testing.T) {
	t.Parallel()

	_, err := mergeRemoteModelsCatalog([]byte(`{"qwen":[],"iflow":[{"id":""}]}`), &staticModelsJSON{})
	if err == nil {
		t.Fatal("mergeRemoteModelsCatalog() error = nil, want rejected catalog")
	}
}

func assertOnlyModelID(t *testing.T, models []*ModelInfo, want string) {
	t.Helper()
	if len(models) != 1 || models[0] == nil || models[0].ID != want {
		t.Fatalf("models = %#v, want only %q", models, want)
	}
}
