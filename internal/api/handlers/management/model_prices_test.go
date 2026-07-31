package management

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	"github.com/gin-gonic/gin"
)

func TestPutModelPricesReplacesEditedAndDeletedEntries(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	h := &Handler{cfg: &config.Config{}, configFilePath: filepath.Join(dir, "config.yaml")}

	putModelPrices(
		t, h, map[string]modelPriceEntry{
			"original-model": {Prompt: 1, Completion: 2, Cache: 0.5},
			"deleted-model":  {Prompt: 3, Completion: 4, Cache: 1},
		},
	)
	putModelPrices(
		t, h, map[string]modelPriceEntry{
			"edited-model": {Prompt: 1.25, Completion: 2.5, Cache: 0.75},
		},
	)

	prices := getModelPrices(t, h)
	if _, ok := prices["original-model"]; ok {
		t.Fatalf("original-model still present after edit replacement: %#v", prices)
	}
	if _, ok := prices["deleted-model"]; ok {
		t.Fatalf("deleted-model still present after delete replacement: %#v", prices)
	}
	edited, ok := prices["edited-model"]
	if !ok {
		t.Fatalf("edited-model missing after save: %#v", prices)
	}
	if edited.Prompt != 1.25 || edited.Completion != 2.5 || edited.Cache != 0.75 {
		t.Fatalf("edited-model price = %#v, want prompt=1.25 completion=2.5 cache=0.75", edited)
	}
}

func TestGetModelPricesReturnsErrorWhenCatalogInvalid(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(filepath.Join(dir, "models.json"), []byte(`{invalid`), 0o600); err != nil {
		t.Fatalf("write invalid models.json: %v", err)
	}
	h := &Handler{cfg: &config.Config{}, configFilePath: configPath}
	w := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(w)
	ctx.Request = httptest.NewRequest(http.MethodGet, "/model-prices", nil)

	h.GetModelPrices(ctx)

	if w.Code != http.StatusInternalServerError {
		t.Fatalf("GET status = %d, want %d; body=%s", w.Code, http.StatusInternalServerError, w.Body.String())
	}
}

func putModelPrices(t *testing.T, h *Handler, prices map[string]modelPriceEntry) {
	t.Helper()

	body, err := json.Marshal(map[string]map[string]modelPriceEntry{"prices": prices})
	if err != nil {
		t.Fatalf("marshal model prices: %v", err)
	}
	w := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(w)
	ctx.Request = httptest.NewRequest(http.MethodPut, "/model-prices", bytes.NewReader(body))
	ctx.Request.Header.Set("Content-Type", "application/json")

	h.PutModelPrices(ctx)

	if w.Code != http.StatusOK {
		t.Fatalf("PUT status = %d, want %d; body=%s", w.Code, http.StatusOK, w.Body.String())
	}
}

func getModelPrices(t *testing.T, h *Handler) map[string]modelPriceEntry {
	t.Helper()

	w := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(w)
	ctx.Request = httptest.NewRequest(http.MethodGet, "/model-prices", nil)

	h.GetModelPrices(ctx)

	if w.Code != http.StatusOK {
		t.Fatalf("GET status = %d, want %d; body=%s", w.Code, http.StatusOK, w.Body.String())
	}
	var payload struct {
		Prices map[string]modelPriceEntry `json:"prices"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode model prices response: %v", err)
	}
	return payload.Prices
}
