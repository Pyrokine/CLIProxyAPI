// Last compiled: 2026-04-27
// Author: pyro

package management

import (
	"math"
	"net/http"
	"time"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/usage"
	"github.com/gin-gonic/gin"
	log "github.com/sirupsen/logrus"
)

// summaryTokens holds a token breakdown.
type summaryTokens struct {
	Input     int64 `json:"input"`
	Output    int64 `json:"output"`
	Cached    int64 `json:"cached"`
	Reasoning int64 `json:"reasoning"`
	Total     int64 `json:"total"`
}

// summaryTotals holds the global aggregation.
type summaryTotals struct {
	Requests int64         `json:"requests"`
	Success  int64         `json:"success"`
	Failure  int64         `json:"failure"`
	Tokens   summaryTokens `json:"tokens"`
	Cost     float64       `json:"cost"`
}

// summaryModelStats holds per-model aggregation.
type summaryModelStats struct {
	Requests int64         `json:"requests"`
	Success  int64         `json:"success"`
	Failure  int64         `json:"failure"`
	Tokens   summaryTokens `json:"tokens"`
	Cost     float64       `json:"cost"`
}

// summaryCredentialStats holds per-credential aggregation.
// Provider/Source are surfaced so the frontend can render the
// "[Claude] alias" tags in dropdowns and stat cards without re-parsing
// the map key — v2 by_credential keys are "provider:source" tuples for
// rows that carry a provider, but legacy rows (provider blank) keep the
// bare source as their key, so the components must travel separately.
type summaryCredentialStats struct {
	Success  int64  `json:"success"`
	Failure  int64  `json:"failure"`
	Provider string `json:"provider,omitempty"`
	Source   string `json:"source,omitempty"`
}

// summaryTimePoint represents a single time bucket.
type summaryTimePoint struct {
	Time     string        `json:"time"`
	Requests int64         `json:"requests"`
	Success  int64         `json:"success"`
	Failure  int64         `json:"failure"`
	Tokens   summaryTokens `json:"tokens"`
	Cost     float64       `json:"cost"`
	HasCost  bool          `json:"has_cost"`
}

// summaryAPIKeyStats holds per-API-key aggregation.
type summaryAPIKeyStats struct {
	Requests int64         `json:"requests"`
	Success  int64         `json:"success"`
	Failure  int64         `json:"failure"`
	Tokens   summaryTokens `json:"tokens"`
	Cost     float64       `json:"cost"`
}

// summaryResponse is the full response for GET /usage/summary.
type summaryResponse struct {
	Period                 summaryPeriod                      `json:"period"`
	Totals                 summaryTotals                      `json:"totals"`
	ByModel                map[string]*summaryModelStats      `json:"by_model"`
	ByCredential           map[string]*summaryCredentialStats `json:"by_credential"`
	ByAPIKey               map[string]*summaryAPIKeyStats     `json:"by_api_key"`
	TimeSeries             []summaryTimePoint                 `json:"time_series"`
	TimeSeriesByModel      map[string][]summaryTimePoint      `json:"time_series_by_model"`
	TimeSeriesByCredential map[string][]summaryTimePoint      `json:"time_series_by_credential"`
	TimeSeriesByAPIKey     map[string][]summaryTimePoint      `json:"time_series_by_api_key"`
}

type summaryPeriod struct {
	From string `json:"from"`
	To   string `json:"to"`
}

// GetUsageSummary returns aggregated usage data for dashboard rendering.
//
//	GET /v0/management/usage/summary?from=...&to=...&granularity=hourly|daily
func (h *Handler) GetUsageSummary(c *gin.Context) {
	start := time.Now()
	now := time.Now().UTC()

	from, errFrom := parseTimeParam(c.Query("from"), now.AddDate(0, 0, -30))
	to, errTo := parseTimeParam(c.Query("to"), now)
	if errFrom != nil || errTo != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid from/to parameter"})
		return
	}
	if from.After(to) {
		from, to = to, from
	}

	granularity := c.DefaultQuery("granularity", "hourly")
	if granularity != "hourly" && granularity != "daily" {
		granularity = "hourly"
	}

	modelFilter := c.Query("model")
	apiKeyFilter := c.Query("api_key")
	credentialFilter := c.Query("credential")
	includeGroups := c.DefaultQuery("groups", "all") != "none"

	if h.usagePersister == nil {
		c.JSON(http.StatusOK, emptySummaryResponse(from, to))
		return
	}

	qr, err := h.usagePersister.Query(
		from, to, usage.EventFilters{
			Model:  modelFilter,
			Source: credentialFilter,
			APIKey: apiKeyFilter,
		}, granularity, includeGroups,
	)
	if err != nil {
		log.Errorf("usage: GetUsageSummary query failed: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to query summary"})
		return
	}

	resp := buildSummaryResponse(from, to, qr, h.usagePersister.HasPricing())

	log.Debugf(
		"usage: GetUsageSummary took %v (from=%s, to=%s)",
		time.Since(start), from.Format(time.RFC3339), to.Format(time.RFC3339),
	)
	c.JSON(http.StatusOK, resp)
}

func emptySummaryResponse(from, to time.Time) summaryResponse {
	return summaryResponse{
		Period: summaryPeriod{
			From: from.Format(time.RFC3339),
			To:   to.Format(time.RFC3339),
		},
		ByModel:                make(map[string]*summaryModelStats),
		ByCredential:           make(map[string]*summaryCredentialStats),
		ByAPIKey:               make(map[string]*summaryAPIKeyStats),
		TimeSeriesByModel:      make(map[string][]summaryTimePoint),
		TimeSeriesByCredential: make(map[string][]summaryTimePoint),
		TimeSeriesByAPIKey:     make(map[string][]summaryTimePoint),
	}
}

func buildSummaryResponse(from, to time.Time, qr usage.QueryResult, hasPricing bool) summaryResponse {
	resp := summaryResponse{
		Period: summaryPeriod{
			From: from.Format(time.RFC3339),
			To:   to.Format(time.RFC3339),
		},
		Totals: summaryTotals{
			Requests: qr.Totals.Requests,
			Success:  qr.Totals.Success,
			Failure:  qr.Totals.Failure,
			Tokens: summaryTokens{
				Input:     qr.Totals.Tokens.InputTokens,
				Output:    qr.Totals.Tokens.OutputTokens,
				Cached:    qr.Totals.Tokens.CachedTokens,
				Reasoning: qr.Totals.Tokens.ReasoningTokens,
				Total:     qr.Totals.Tokens.TotalTokens,
			},
			Cost: roundCost(qr.Totals.Cost),
		},
		ByModel:                make(map[string]*summaryModelStats, len(qr.ByModel)),
		ByCredential:           make(map[string]*summaryCredentialStats, len(qr.ByCredential)),
		ByAPIKey:               make(map[string]*summaryAPIKeyStats, len(qr.ByAPIKey)),
		TimeSeriesByModel:      make(map[string][]summaryTimePoint, len(qr.TimeSeriesByModel)),
		TimeSeriesByCredential: make(map[string][]summaryTimePoint, len(qr.TimeSeriesByCredential)),
		TimeSeriesByAPIKey:     make(map[string][]summaryTimePoint, len(qr.TimeSeriesByAPIKey)),
	}

	for model, ms := range qr.ByModel {
		resp.ByModel[model] = &summaryModelStats{
			Requests: ms.Requests,
			Success:  ms.Success,
			Failure:  ms.Failure,
			Tokens: summaryTokens{
				Input:     ms.Tokens.InputTokens,
				Output:    ms.Tokens.OutputTokens,
				Cached:    ms.Tokens.CachedTokens,
				Reasoning: ms.Tokens.ReasoningTokens,
				Total:     ms.Tokens.TotalTokens,
			},
			Cost: roundCost(ms.Cost),
		}
	}

	for cred, cs := range qr.ByCredential {
		resp.ByCredential[cred] = &summaryCredentialStats{
			Success:  cs.Success,
			Failure:  cs.Failure,
			Provider: cs.Provider,
			Source:   cs.Source,
		}
	}

	for key, aks := range qr.ByAPIKey {
		resp.ByAPIKey[key] = &summaryAPIKeyStats{
			Requests: aks.Requests,
			Success:  aks.Success,
			Failure:  aks.Failure,
			Tokens: summaryTokens{
				Input:     aks.Tokens.InputTokens,
				Output:    aks.Tokens.OutputTokens,
				Cached:    aks.Tokens.CachedTokens,
				Reasoning: aks.Tokens.ReasoningTokens,
				Total:     aks.Tokens.TotalTokens,
			},
			Cost: roundCost(aks.Cost),
		}
	}

	resp.TimeSeries = make([]summaryTimePoint, len(qr.TimeSeries))
	for i, tp := range qr.TimeSeries {
		resp.TimeSeries[i] = summaryTimePoint{
			Time:     tp.Time,
			Requests: tp.Requests,
			Success:  tp.Success,
			Failure:  tp.Failure,
			Tokens: summaryTokens{
				Input:     tp.Tokens.InputTokens,
				Output:    tp.Tokens.OutputTokens,
				Cached:    tp.Tokens.CachedTokens,
				Reasoning: tp.Tokens.ReasoningTokens,
				Total:     tp.Tokens.TotalTokens,
			},
			Cost:    roundCost(tp.Cost),
			HasCost: tp.Cost > 0 || (hasPricing && tp.Requests > 0),
		}
	}

	for model, points := range qr.TimeSeriesByModel {
		converted := make([]summaryTimePoint, len(points))
		for i, tp := range points {
			converted[i] = summaryTimePoint{
				Time:     tp.Time,
				Requests: tp.Requests,
				Success:  tp.Success,
				Failure:  tp.Failure,
				Tokens: summaryTokens{
					Input:     tp.Tokens.InputTokens,
					Output:    tp.Tokens.OutputTokens,
					Cached:    tp.Tokens.CachedTokens,
					Reasoning: tp.Tokens.ReasoningTokens,
					Total:     tp.Tokens.TotalTokens,
				},
				Cost:    roundCost(tp.Cost),
				HasCost: tp.Cost > 0 || (hasPricing && tp.Requests > 0),
			}
		}
		resp.TimeSeriesByModel[model] = converted
	}

	for cred, points := range qr.TimeSeriesByCredential {
		converted := make([]summaryTimePoint, len(points))
		for i, tp := range points {
			converted[i] = summaryTimePoint{
				Time:     tp.Time,
				Requests: tp.Requests,
				Success:  tp.Success,
				Failure:  tp.Failure,
				Tokens: summaryTokens{
					Input:     tp.Tokens.InputTokens,
					Output:    tp.Tokens.OutputTokens,
					Cached:    tp.Tokens.CachedTokens,
					Reasoning: tp.Tokens.ReasoningTokens,
					Total:     tp.Tokens.TotalTokens,
				},
				Cost:    roundCost(tp.Cost),
				HasCost: tp.Cost > 0 || (hasPricing && tp.Requests > 0),
			}
		}
		resp.TimeSeriesByCredential[cred] = converted
	}

	for key, points := range qr.TimeSeriesByAPIKey {
		converted := make([]summaryTimePoint, len(points))
		for i, tp := range points {
			converted[i] = summaryTimePoint{
				Time:     tp.Time,
				Requests: tp.Requests,
				Success:  tp.Success,
				Failure:  tp.Failure,
				Tokens: summaryTokens{
					Input:     tp.Tokens.InputTokens,
					Output:    tp.Tokens.OutputTokens,
					Cached:    tp.Tokens.CachedTokens,
					Reasoning: tp.Tokens.ReasoningTokens,
					Total:     tp.Tokens.TotalTokens,
				},
				Cost:    roundCost(tp.Cost),
				HasCost: tp.Cost > 0 || (hasPricing && tp.Requests > 0),
			}
		}
		resp.TimeSeriesByAPIKey[key] = converted
	}

	return resp
}

func parseTimeParam(value string, fallback time.Time) (time.Time, error) {
	if value == "" {
		return fallback, nil
	}
	// RFC3339: 2026-03-10T12:00:00Z
	t, err := time.Parse(time.RFC3339, value)
	if err != nil {
		// datetime-local format from HTML input: 2026-03-10T12:00
		t, err = time.Parse("2006-01-02T15:04", value)
		if err != nil {
			// Date-only: 2026-03-10
			t, err = time.Parse("2006-01-02", value)
			if err != nil {
				return time.Time{}, err
			}
		}
	}
	return t.UTC(), nil
}

func roundCost(v float64) float64 {
	return math.Round(v*100) / 100
}
