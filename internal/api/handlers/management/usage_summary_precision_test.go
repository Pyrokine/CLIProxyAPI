package management

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/usage"
	"github.com/gin-gonic/gin"
)

func TestUsageSummaryAndEventsShareExactHTTPRange(t *testing.T) {
	dir := t.TempDir()
	p := usage.NewPersister(dir, config.UsageRetention{Days: -1})
	p.Start(context.Background())
	defer p.Stop()
	if !p.IsReady() {
		t.Fatal("temporary persister did not start")
	}
	if _, err := p.Import(
		[]byte((`[{"timestamp":"2026-11-01T10:05:00Z","model":"m","auth_index":"auth-a"` +
			`,"tokens":{"total_tokens":1}},{"timestamp":"2026-11-01T10:15:00Z","mod` +
			`el":"m","auth_index":"auth-a","tokens":{"total_tokens":5}},{"timestamp` +
			`":"2026-11-01T10:25:00Z","model":"m","auth_index":"auth-a","tokens":{"` +
			`total_tokens":9}},{"timestamp":"2026-11-01T10:55:00Z","model":"m","aut` +
			`h_index":"auth-a","tokens":{"total_tokens":20}}]`)),
	); err != nil {
		t.Fatal(err)
	}
	h := NewHandler(&config.Config{}, "", nil, withEnvSecret(""), withUsagePersister(p))
	values := url.Values{
		"from": {"2026-11-01T10:15:00Z"}, "to": {"2026-11-01T10:25:00Z"}, "auth_index_groups": {"true"},
		"auth_index_granularity": {"10m"},
	}
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodGet, "/usage/summary?"+values.Encode(), nil)
	h.GetUsageSummary(c)
	var summary struct {
		Totals struct {
			Requests int64 `json:"requests"`
			Tokens   struct {
				Total int64 `json:"total"`
			} `json:"tokens"`
		} `json:"totals"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &summary); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusOK || summary.Totals.Requests != 2 || summary.Totals.Tokens.Total != 14 {
		t.Fatalf("status=%d summary=%s", rec.Code, rec.Body.String())
	}
	eventRec := httptest.NewRecorder()
	eventCtx, _ := gin.CreateTestContext(eventRec)
	eventCtx.Request = httptest.NewRequest(http.MethodGet, "/usage/events?"+values.Encode(), nil)
	h.GetUsageEvents(eventCtx)
	var events struct {
		Total int64 `json:"total"`
	}
	if err := json.Unmarshal(eventRec.Body.Bytes(), &events); err != nil {
		t.Fatal(err)
	}
	if eventRec.Code != http.StatusOK || events.Total != 2 {
		t.Fatalf("status=%d events=%s", eventRec.Code, eventRec.Body.String())
	}
}
