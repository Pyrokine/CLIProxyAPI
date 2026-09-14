package management

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/modelcatalog"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/usage"
	coreusage "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/usage"
	"github.com/gin-gonic/gin"
)

func TestGetUsageSummaryReturnsCostCoverage(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.yaml")
	if err := modelcatalog.Save(
		configPath, modelcatalog.File{
			Prices: map[string]modelcatalog.Price{
				"complete-model": {
					Prompt: 1000,
					Status: modelcatalog.PriceStatusComplete,
				},
				"partial-model": {
					Prompt:    2000,
					Status:    modelcatalog.PriceStatusPartial,
					AutoApply: true,
				},
			},
		},
	); err != nil {
		t.Fatalf("save model catalog: %v", err)
	}

	persister := usage.NewPersister(dir, config.UsageRetention{Days: -1})
	persister.SetPriceProvider(usage.BuildModelPriceProvider(configPath))
	persister.Start(context.Background())
	if !persister.IsReady() {
		t.Fatal("usage persister did not start")
	}
	defer persister.Stop()

	from := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	events := []struct {
		model  string
		tokens int64
	}{
		{model: "complete-model", tokens: 10},
		{model: "partial-model", tokens: 20},
		{model: "unpriced-model", tokens: 30},
	}
	for index, event := range events {
		detail := usage.FlatDetail{
			Timestamp:         from.Add(time.Duration(index+1) * time.Minute),
			RequestID:         fmt.Sprintf("coverage-%d", index),
			Model:             event.model,
			Completed:         true,
			AccountingVersion: coreusage.TokenAccountingSchemaVersion,
			TokenBreakdown: coreusage.NewSubsetTokenBreakdown(
				event.tokens,
				0,
				0,
				0,
				0,
				event.tokens,
			),
		}
		detail.Tokens.InputTokens = event.tokens
		detail.Tokens.TotalTokens = event.tokens
		persister.Record(detail)
	}

	handler := NewHandler(
		&config.Config{},
		configPath,
		nil,
		withEnvSecret(""),
		withUsagePersister(persister),
	)
	path := fmt.Sprintf(
		"/usage/summary?from=%s&to=%s&granularity=hourly&groups=none",
		from.Format(time.RFC3339),
		from.Add(time.Hour).Format(time.RFC3339),
	)
	requestSummary := func(summaryPath string) summaryResponse {
		t.Helper()
		recorder := httptest.NewRecorder()
		ctx, _ := gin.CreateTestContext(recorder)
		ctx.Request = httptest.NewRequest(http.MethodGet, summaryPath, nil)
		handler.GetUsageSummary(ctx)
		if recorder.Code != http.StatusOK {
			t.Fatalf("GetUsageSummary status = %d, want %d: %s", recorder.Code, http.StatusOK, recorder.Body.String())
		}
		var response summaryResponse
		if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
			t.Fatalf("decode GetUsageSummary response: %v", err)
		}
		return response
	}

	var response summaryResponse
	deadline := time.Now().Add(5 * time.Second)
	for {
		response = requestSummary(path)
		if response.Totals.Requests == int64(len(events)) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("usage records were not persisted before deadline: %+v", response.Totals)
		}
		time.Sleep(25 * time.Millisecond)
	}

	coverage := response.Totals.CostCoverage
	if coverage.Complete.Requests != 1 || coverage.Complete.Tokens != 10 ||
		coverage.Partial.Requests != 1 || coverage.Partial.Tokens != 20 ||
		coverage.Unpriced.Requests != 1 || coverage.Unpriced.Tokens != 30 {
		t.Fatalf("GetUsageSummary cost coverage = %+v", coverage)
	}
	if response.Totals.Cost != 0.05 {
		t.Fatalf("GetUsageSummary cost = %v, want 0.05", response.Totals.Cost)
	}

	unpricedResponse := requestSummary(path + "&model=unpriced-model")
	if len(unpricedResponse.TimeSeries) == 0 {
		t.Fatal("GetUsageSummary unpriced time series is empty")
	}
	for _, point := range unpricedResponse.TimeSeries {
		if point.Cost != 0 || point.HasCost {
			t.Fatalf("GetUsageSummary unpriced time point = %+v", point)
		}
	}
}
