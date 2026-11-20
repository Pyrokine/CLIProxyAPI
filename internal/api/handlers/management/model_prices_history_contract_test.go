package management

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/usage"
	"github.com/gin-gonic/gin"
)

func TestPutModelPricesRecalculatesHistoricalSummaryWithUnpricedCoverage(t *testing.T) {
	persister := usage.NewPersister(t.TempDir(), config.UsageRetention{Days: -1})
	persister.Start(t.Context())
	if !persister.IsReady() {
		t.Fatal("temporary persister did not start")
	}
	defer persister.Stop()
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	if result, err := persister.ImportContext(
		ctx, []byte("[\n\t\t{\"timestamp\":\"2026-03-01T03:04:05Z\",\"model\":\"synthetic-p"+
			`riced-history","tokens":{"input_tokens":10,"output_tokens":2,"total_to`+
			"kens\":12}},\n\t\t{\"timestamp\":\"2026-03-01T03:04:06Z\",\"model\":\""+
			`synthetic-unpriced-history","tokens":{"input_tokens":10,"output_tokens`+
			"\":2,\"total_tokens\":12}}\n\t]"),
	); err != nil || result.Added != 2 {
		t.Fatalf("import historical usage: %+v %v", result, err)
	}
	h := &Handler{
		cfg:            &config.Config{},
		configFilePath: filepath.Join(t.TempDir(), "config.yaml"),
		usagePersister: persister,
	}
	putModelPrices(
		t, h, map[string]modelPriceEntry{
			"synthetic-priced-history": {Prompt: 1, Completion: 2, Status: "complete"},
		},
	)
	if price := getModelPrices(t, h)["synthetic-priced-history"]; price.Prompt != 1 || price.Completion != 2 {
		t.Fatalf("saved price = %+v", price)
	}
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		status := getRecalculateCostsStatus(t, h)
		if !status.Running {
			if status.Status != "ok" || status.Error != "" || status.ProcessedEvents != 2 {
				t.Fatalf("price-save recalculation = %+v", status)
			}
			break
		}
		select {
		case <-ctx.Done():
			t.Fatalf("price-save recalculation did not finish: %+v", status)
		case <-ticker.C:
		}
	}
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(
		http.MethodGet,
		"/usage/summary?from=2026-03-01T00:00:00Z&to=2026-03-02T00:00:00Z&granularity=daily", nil,
	)
	h.GetUsageSummary(c)
	var summary summaryResponse
	if err := json.Unmarshal(recorder.Body.Bytes(), &summary); err != nil || recorder.Code != http.StatusOK {
		t.Fatalf("historical summary status=%d error=%v body=%s", recorder.Code, err, recorder.Body.String())
	}
	if summary.Totals.Requests != 2 || summary.Totals.Tokens.Total != 24 || summary.Totals.Cost != 0.000014 ||
		summary.Totals.CostCoverage.Complete.Requests != 1 || summary.Totals.CostCoverage.Unpriced.Requests != 1 {
		t.Fatalf("price-save summary = %+v", summary.Totals)
	}
	priced := summary.ByModel["synthetic-priced-history"]
	if priced == nil || priced.Cost != 0.000014 || priced.Requests != 1 {
		t.Fatalf("priced model summary = %+v", priced)
	}
	var seriesCost float64
	for _, point := range summary.TimeSeries {
		seriesCost += point.Cost
		if point.Requests > 0 && !point.HasCost {
			t.Fatalf("positive subcent point lost cost presence: %+v", point)
		}
	}
	if seriesCost != summary.Totals.Cost {
		t.Fatalf("series cost = %g, total = %g", seriesCost, summary.Totals.Cost)
	}
	events, total, err := persister.QueryEventsContext(
		ctx,
		time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC), time.Date(2026, 3, 2, 0, 0, 0, 0, time.UTC),
		usage.EventFilters{Model: "synthetic-unpriced-history"}, 1, 10, "timestamp", false,
	)
	if err != nil || total != 1 || len(events) != 1 {
		t.Fatalf("unpriced history query count=%d error=%v", total, err)
	}
	if events[0].CostStatus != usage.CostStatusUnpriced || events[0].CostReason == "" {
		t.Fatalf("missing price lost status or reason: %+v", events[0])
	}
	t.Log(
		"price save completed historical recalculation: total/model/series=0.00" +
			"0014 USD; missing-price event retained unpriced status and reason",
	)
}
