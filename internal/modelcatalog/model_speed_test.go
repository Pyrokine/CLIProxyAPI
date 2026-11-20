package modelcatalog

import (
	"path/filepath"
	"testing"
)

func TestModelCatalogSpeedPersistsAndSurvivesDefaultAdoption(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	doc := DefaultFile()
	model, exists := doc.Find("gpt-6.2-astra")
	if !exists {
		t.Fatal("Astra missing")
	}
	model.Speed, model.Enabled = "ultrafast", false
	if err := Save(path, doc); err != nil {
		t.Fatal(err)
	}
	loaded, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ApplyDefaultDecisions(
		&loaded, []ApplyDecision{{Name: model.Name, Action: "use_default"}},
	); err != nil {
		t.Fatal(err)
	}
	updated, _ := loaded.Find(model.Name)
	if updated.Speed != "ultrafast" || updated.Enabled {
		t.Fatalf("adoption replaced user settings: %+v", updated)
	}
	for _, entry := range loaded.ToModelCatalogModels() {
		if entry.Name == model.Name && entry.Speed != "ultrafast" {
			t.Fatalf("runtime projection lost speed: %+v", entry)
		}
	}
}
