package management

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/modelcatalog"
	"github.com/gin-gonic/gin"
)

func TestModelCatalogSpeedPatchPersistsAndPublishes(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := modelcatalog.Save(path, modelcatalog.DefaultFile()); err != nil {
		t.Fatal(err)
	}
	h := &Handler{cfg: &config.Config{}, configFilePath: path}
	t.Cleanup(func() { config.SetRuntimeModelCatalog(nil) })
	for _, mode := range []string{"ultrafast", "standard", "inherit", "ultrafast"} {
		response := performManagementJSONRequest(
			t, h.PatchModelCatalogModel, http.MethodPatch,
			"/model-catalog/model", gin.H{"name": "gpt-6.2-astra", "speed": mode},
		)
		if response.Code != http.StatusOK {
			t.Fatalf("patch %s returned %d: %s", mode, response.Code, response.Body.String())
		}
		var catalog modelCatalogResponse
		if err := json.Unmarshal(response.Body.Bytes(), &catalog); err != nil {
			t.Fatal(err)
		}
		entry, exists := findModelCatalogResponseRow(catalog.Models, "gpt-6.2-astra")
		if !exists || entry.Speed != mode || !slices.Contains(entry.SpeedOptions, "ultrafast") {
			t.Fatalf("saved speed response = %+v", entry)
		}
		persisted, err := modelcatalog.Load(path)
		if err != nil {
			t.Fatal(err)
		}
		model, _ := persisted.Find("gpt-6.2-astra")
		if model.Speed != mode || h.configSnapshot().ModelCatalogSpeed("codex", "gpt-6.2-astra") != mode {
			t.Fatalf("persisted/runtime speed differs for %s", mode)
		}
	}
	response := performManagementJSONRequest(
		t, h.PatchModelCatalogModel, http.MethodPatch,
		"/model-catalog/model", gin.H{"name": "claude-haiku-5-5", "speed": "fast"},
	)
	if response.Code != http.StatusBadRequest {
		t.Fatalf("unsupported speed returned %d: %s", response.Code, response.Body.String())
	}
	response = performManagementJSONRequest(
		t, h.PatchModelCatalogModel, http.MethodPatch,
		"/model-catalog/model", gin.H{"name": "gpt-6.2-astra", "channel": "meta"},
	)
	if response.Code != http.StatusBadRequest {
		t.Fatalf("channel change retained an unsupported speed: %d %s", response.Code, response.Body.String())
	}
	for _, test := range []struct {
		model gin.H
		want  string
	}{
		{gin.H{"name": "gpt-6.2-astra", "channel": "codex", "enabled": true}, "ultrafast"},
		{gin.H{"name": "gpt-6.2-astra", "channel": "codex", "enabled": true, "speed": "inherit"}, "inherit"},
	} {
		response = performManagementJSONRequest(
			t, h.PutModelCatalog, http.MethodPut,
			"/model-catalog", gin.H{"models": []gin.H{test.model}},
		)
		if response.Code != http.StatusOK {
			t.Fatalf("catalog PUT returned %d: %s", response.Code, response.Body.String())
		}
		persisted, err := modelcatalog.Load(path)
		if err != nil {
			t.Fatal(err)
		}
		model, exists := persisted.Find("gpt-6.2-astra")
		if !exists || model.Speed != test.want || model.Channel != "codex" {
			t.Fatalf("PUT lost the speed or persisted a rejected channel: %+v", model)
		}
	}
}
