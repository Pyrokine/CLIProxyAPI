package management

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/modelcatalog"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/usage"
	"github.com/gin-gonic/gin"
)

func TestConditionalPriceOnlyPatchPreservesCatalogAndAbsentRates(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	doc := modelcatalog.File{
		SchemaVersion: modelcatalog.SchemaVersion, DefaultsVersion: modelcatalog.DefaultsVersion,
		Models: []modelcatalog.Model{
			{
				Name: "kept-model", Provider: "GPT", Channel: "codex", Group: "kept-group", DisplayName: "kept-name",
				Enabled: false,
			},
		},
		OAuthModelAlias: map[string][]config.OAuthModelAlias{
			"codex": {
				{
					Name: "kept-model", Alias: "kept-alias", Fork: true,
				},
			},
		},
		Prices: map[string]modelcatalog.Price{
			"custom-price": {
				Prompt: 9, Completion: 10, Status: modelcatalog.PriceStatusPartial,
			},
		},
	}
	if err := modelcatalog.Save(path, doc); err != nil {
		t.Fatal(err)
	}
	before, err := modelcatalog.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	h := &Handler{cfg: &config.Config{}, configFilePath: path}
	for _, raw := range []string{
		`{"prompt":1,"request_scoped":true}`,
		`{"prompt":0,"completion":0,"cache_read":0,"request_scoped":true}`,
		`{"prompt":1,"completion":2,"cache_write_30m":2.5,"cache_write_12h":5,"request_scoped":true,"conditional_rates":[{"id":"long","min_input_tokens":272001,"prompt":4,"completion":15}],"modality_rates":[{"modality":"text","prompt":1,"completion":2}],"charge_rates":[{"kind":"web_fetch","unit":"request","price":0,"per_units":1,"speed":"batch"}],"coverage_gaps":[{"dimension":"tools","reason":"missing tool usage"}]}`,
	} {
		response := httptest.NewRecorder()
		ctx, _ := gin.CreateTestContext(response)
		ctx.Request = httptest.NewRequest(
			http.MethodPatch, "/model-catalog",
			bytes.NewBufferString(`{"name":"price-only","recalculate_costs":false,"price":`+raw+`}`),
		)
		ctx.Request.Header.Set("Content-Type", "application/json")
		h.PatchModelCatalogModel(ctx)
		if response.Code != http.StatusOK {
			t.Fatalf("PATCH status=%d body=%s", response.Code, response.Body.String())
		}
		loaded, err := modelcatalog.Load(path)
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(loaded.Models, before.Models) || !reflect.DeepEqual(
			loaded.OAuthModelAlias, before.OAuthModelAlias,
		) || !reflect.DeepEqual(loaded.Prices["custom-price"], before.Prices["custom-price"]) {
			t.Fatal("price-only maintenance changed model state, aliases or a custom price")
		}
		var expected, actual map[string]any
		if err = json.Unmarshal([]byte(raw), &expected); err != nil {
			t.Fatal(err)
		}
		getResponse := httptest.NewRecorder()
		getCtx, _ := gin.CreateTestContext(getResponse)
		getCtx.Request = httptest.NewRequest(http.MethodGet, "/model-prices", nil)
		h.GetModelPrices(getCtx)
		var result struct {
			Prices map[string]json.RawMessage `json:"prices"`
		}
		if err = json.Unmarshal(getResponse.Body.Bytes(), &result); err != nil || getResponse.Code != http.StatusOK {
			t.Fatalf("GET=%d/%v", getResponse.Code, err)
		}
		if err = json.Unmarshal(result.Prices["price-only"], &actual); err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(actual, expected) {
			t.Fatalf("price readback %s -> %s", raw, result.Prices["price-only"])
		}
	}
}

func TestConditionalPricePatchCanSkipHistoricalRecalculation(t *testing.T) {
	ctx := t.Context()
	persister := usage.NewPersister(t.TempDir(), config.UsageRetention{Days: -1})
	persister.Start(ctx)
	defer persister.Stop()
	if !persister.IsReady() {
		t.Fatal("temporary usage persister is not ready")
	}
	importCtx, cancelImport := context.WithTimeout(ctx, 10*time.Second)
	defer cancelImport()
	if result, err := persister.ImportContext(
		importCtx,
		[]byte(`[{"timestamp":"2026-03-01T03:04:05Z","model":"price-only","tokens":{"input_tokens":10,"output_tokens":2,"total_tokens":12}}]`),
	); err != nil || result.Added != 1 {
		t.Fatalf("import=%+v/%v", result, err)
	}
	h := &Handler{
		cfg: &config.Config{}, configFilePath: filepath.Join(t.TempDir(), "config.yaml"), usagePersister: persister,
	}
	response := httptest.NewRecorder()
	requestCtx, _ := gin.CreateTestContext(response)
	requestCtx.Request = httptest.NewRequest(
		http.MethodPatch, "/model-catalog",
		bytes.NewBufferString(`{"name":"price-only","price":{"prompt":1,"completion":2,"cache_read":0.5,"request_scoped":true},"recalculate_costs":false}`),
	)
	requestCtx.Request.Header.Set("Content-Type", "application/json")
	h.PatchModelCatalogModel(requestCtx)
	if response.Code != http.StatusOK {
		t.Fatalf("PATCH=%d %s", response.Code, response.Body.String())
	}
	status := getRecalculateCostsStatus(t, h)
	if status.Running || status.ProcessedEvents != 0 {
		t.Fatalf("maintenance started historical repricing: %+v", status)
	}
	start := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)
	queryCtx, cancelQuery := context.WithTimeout(ctx, 10*time.Second)
	defer cancelQuery()
	events, count, err := persister.QueryEventsContext(
		queryCtx, start, start.Add(24*time.Hour), usage.EventFilters{}, 1, 10, "timestamp", false,
	)
	if err != nil || count != 1 || len(events) != 1 || events[0].Cost != 0 || events[0].PricingReceipt != nil {
		t.Fatalf("historical event changed: %+v/%d/%v", events, count, err)
	}
	if !persister.HasPricing() {
		t.Fatal("maintenance did not refresh the price provider for future requests")
	}
}
