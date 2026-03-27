// Last compiled: 2026-03-23
// Author: pyro

package management

import (
	"math"
	"net/http"
	"time"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/usage"
	"github.com/gin-gonic/gin"
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
type summaryCredentialStats struct {
	Success int64 `json:"success"`
	Failure int64 `json:"failure"`
}

// summaryTimePoint represents a single time bucket.
type summaryTimePoint struct {
	Time     string  `json:"time"`
	Requests int64   `json:"requests"`
	Tokens   int64   `json:"tokens"`
	Cost     float64 `json:"cost"`
	HasCost  bool    `json:"has_cost"`
}

// summaryResponse is the full response for GET /usage/summary.
type summaryResponse struct {
	Period            summaryPeriod                      `json:"period"`
	Totals            summaryTotals                      `json:"totals"`
	ByModel           map[string]*summaryModelStats      `json:"by_model"`
	ByCredential      map[string]*summaryCredentialStats `json:"by_credential"`
	TimeSeries        []summaryTimePoint                 `json:"time_series"`
	TimeSeriesByModel map[string][]summaryTimePoint      `json:"time_series_by_model"`
}

type summaryPeriod struct {
	From string `json:"from"`
	To   string `json:"to"`
}

// GetUsageSummary returns aggregated usage data for dashboard rendering.
//
//	GET /v0/management/usage/summary?from=...&to=...&granularity=hourly|daily
func (h *Handler) GetUsageSummary(c *gin.Context) {
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

	if h.usagePersister == nil {
		c.JSON(http.StatusOK, emptySummaryResponse(from, to))
		return
	}

	summary := h.usagePersister.Summary()
	if summary == nil {
		c.JSON(http.StatusOK, emptySummaryResponse(from, to))
		return
	}

	var qr usage.QueryResult
	if granularity == "hourly" {
		qr = summary.QueryHourly(from, to)
	} else {
		qr = summary.Query(from, to)
	}

	c.JSON(http.StatusOK, buildSummaryResponse(from, to, qr, h.usagePersister.HasPricing()))
}

func emptySummaryResponse(from, to time.Time) summaryResponse {
	return summaryResponse{
		Period: summaryPeriod{
			From: from.Format(time.RFC3339),
			To:   to.Format(time.RFC3339),
		},
		ByModel:           make(map[string]*summaryModelStats),
		ByCredential:      make(map[string]*summaryCredentialStats),
		TimeSeriesByModel: make(map[string][]summaryTimePoint),
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
		ByModel:           make(map[string]*summaryModelStats, len(qr.ByModel)),
		ByCredential:      make(map[string]*summaryCredentialStats, len(qr.ByCredential)),
		TimeSeriesByModel: make(map[string][]summaryTimePoint, len(qr.TimeSeriesByModel)),
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
			Success: cs.Success,
			Failure: cs.Failure,
		}
	}

	resp.TimeSeries = make([]summaryTimePoint, len(qr.TimeSeries))
	for i, tp := range qr.TimeSeries {
		resp.TimeSeries[i] = summaryTimePoint{
			Time:     tp.Time,
			Requests: tp.Requests,
			Tokens:   tp.Tokens,
			Cost:     roundCost(tp.Cost),
			HasCost:  tp.Cost > 0 || (hasPricing && tp.Requests > 0),
		}
	}

	for model, points := range qr.TimeSeriesByModel {
		converted := make([]summaryTimePoint, len(points))
		for i, tp := range points {
			converted[i] = summaryTimePoint{
				Time:     tp.Time,
				Requests: tp.Requests,
				Tokens:   tp.Tokens,
			}
		}
		resp.TimeSeriesByModel[model] = converted
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
