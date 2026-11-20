package pluginhost

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestPluginTokenStoragePreservesExactJSONNumbers(t *testing.T) {
	const integer = "9007199254740993"
	const fraction = "0.12345678901234567890123456789"
	for _, removePriority := range []bool{false, true} {
		name := "metadata merge"
		if removePriority {
			name = "priority removal"
		}
		t.Run(
			name, func(t *testing.T) {
				storage := &pluginTokenStorage{
					provider: "precision-fixture",
					rawJSON:  []byte(`{"identity":` + integer + `,"fraction":` + fraction + `,"priority":2}`),
				}
				if removePriority {
					storage.meta = map[string]any{"priority": 2}
				}
				storage.SetMetadata(map[string]any{"label": "updated"})
				path := filepath.Join(t.TempDir(), "auth.json")
				if err := storage.SaveTokenToFile(path); err != nil {
					t.Fatal(err)
				}
				data, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				var fields map[string]json.RawMessage
				if err := json.Unmarshal(data, &fields); err != nil {
					t.Fatal(err)
				}
				if string(fields["identity"]) != integer || string(fields["fraction"]) != fraction {
					t.Fatalf("numeric credential fields changed: %s", data)
				}
				if _, exists := fields["priority"]; removePriority && exists {
					t.Fatalf("removed priority reappeared: %s", data)
				}
			},
		)
	}
}

func TestPluginTokenStorageWritesAdjacentLargeIntegerChange(t *testing.T) {
	path := filepath.Join(t.TempDir(), "auth.json")
	if err := os.WriteFile(
		path, []byte(`{"identity":9007199254740992,"type":"precision-fixture"}`), 0o600,
	); err != nil {
		t.Fatal(err)
	}
	storage := &pluginTokenStorage{provider: "precision-fixture", rawJSON: []byte(`{"identity":9007199254740993}`)}
	if err := storage.SaveTokenToFile(path); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		t.Fatal(err)
	}
	if string(fields["identity"]) != "9007199254740993" {
		t.Fatalf("changed credential identity was not saved: %s", data)
	}
}

func TestPluginTokenStorageRejectsMultipleJSONValues(t *testing.T) {
	if _, err := mergedStorageJSON([]byte(`{} {}`), nil, "precision-fixture"); err == nil {
		t.Fatal("multiple storage JSON values were accepted")
	}
	if jsonPayloadEqual([]byte(`{} {}`), []byte(`{}`)) {
		t.Fatal("invalid storage JSON was treated as current")
	}
}
