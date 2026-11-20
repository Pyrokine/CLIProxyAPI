package management

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/modelcatalog"
	"github.com/gin-gonic/gin"
)

func TestSpeedPricingManagementRoundtripAndValidation(t *testing.T) {
	h := &Handler{cfg: &config.Config{}, configFilePath: filepath.Join(t.TempDir(), "config.yaml")}
	price := modelPriceEntry{
		Prompt: 1, Completion: 2, SpeedRates: []modelcatalog.SpeedRate{
			{
				Speed: "fast", Provider: "openai", Multiplier: 2.5,
				Multipliers:   map[string]float64{"cache_read": 0, "cache_write_5m": 3},
				EffectiveFrom: "2026-02-01T00:00:00Z", EffectiveUntil: "2026-02-17T00:00:00Z", Source: "price-source",
			},
		},
	}
	putModelPrices(t, h, map[string]modelPriceEntry{"speed-model": price})
	if got := getModelPrices(t, h)["speed-model"]; !reflect.DeepEqual(got, price) {
		t.Fatalf("PUT/GET price = %+v", got)
	}
	for _, test := range []struct{ name, rule string }{
		{"zero multiplier", `{"speed":"fast","multiplier":0}`},
		{"negative multiplier", `{"speed":"fast","multiplier":-1}`},
		{"negative override", `{"speed":"fast","multiplier":2,"multipliers":{"completion":-1}}`},
		{"null override", `{"speed":"fast","multiplier":2,"multipliers":{"prompt":null}}`},
		{"null overrides object", `{"speed":"fast","multiplier":2,"multipliers":null}`},
		{"non-number override", `{"speed":"fast","multiplier":2,"multipliers":{"prompt":"0"}}`},
		{"invalid speed", `{"speed":"unknown","multiplier":2}`},
		{"overlap", `{"speed":"fast","multiplier":2},{"speed":"fast","multiplier":3}`},
	} {
		t.Run(
			test.name, func(t *testing.T) {
				response := httptest.NewRecorder()
				ctx, _ := gin.CreateTestContext(response)
				ctx.Request = httptest.NewRequest(
					http.MethodPut, "/model-prices",
					bytes.NewBufferString(`{"prices":{"speed-model":{"prompt":1,"speed_rates":[`+test.rule+`]}}}`),
				)
				ctx.Request.Header.Set("Content-Type", "application/json")
				h.PutModelPrices(ctx)
				if response.Code != http.StatusBadRequest {
					t.Fatalf("invalid rule status = %d body=%s", response.Code, response.Body.String())
				}
				for _, entry := range []struct {
					method, body string
					handler      func(*gin.Context)
				}{
					{
						http.MethodPatch,
						`{"name":"speed-model","price":{"prompt":1,"speed_rates":[` + test.rule + `]}}`,
						h.PatchModelCatalogModel,
					},
					{
						http.MethodPut,
						`{"models":[{"name":"speed-model","provider":"OpenAI","group":"GPT","price":{"prompt":1,"speed_rates":[` + test.rule + `]}}]}`,
						h.PutModelCatalog,
					},
				} {
					response := httptest.NewRecorder()
					ctx, _ := gin.CreateTestContext(response)
					ctx.Request = httptest.NewRequest(entry.method, "/model-catalog", bytes.NewBufferString(entry.body))
					ctx.Request.Header.Set("Content-Type", "application/json")
					entry.handler(ctx)
					if response.Code != http.StatusBadRequest {
						t.Fatalf("invalid catalog price status = %d body=%s", response.Code, response.Body.String())
					}
				}
				if got := getModelPrices(t, h)["speed-model"]; !reflect.DeepEqual(got, price) {
					t.Fatalf("invalid price changed saved catalog: %+v", got)
				}
			},
		)
	}
	price.SpeedRates[0].Multiplier = 3
	payload, err := json.Marshal(map[string]any{"name": "speed-model", "price": price})
	if err != nil {
		t.Fatal(err)
	}
	response := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(response)
	ctx.Request = httptest.NewRequest(http.MethodPatch, "/model-catalog/model", bytes.NewReader(payload))
	ctx.Request.Header.Set("Content-Type", "application/json")
	h.PatchModelCatalogModel(ctx)
	if response.Code != http.StatusOK {
		t.Fatalf("catalog PATCH = %d body=%s", response.Code, response.Body.String())
	}
	var catalog modelCatalogResponse
	if err := json.Unmarshal(response.Body.Bytes(), &catalog); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, row := range catalog.Models {
		if row.Name == "speed-model" {
			found = true
			if row.Price == nil || !reflect.DeepEqual(*row.Price, price) {
				t.Fatalf("catalog response price = %+v", row.Price)
			}
		}
	}
	if !found || !reflect.DeepEqual(getModelPrices(t, h)["speed-model"], price) {
		t.Fatal("catalog patch did not preserve speed rules")
	}
}
