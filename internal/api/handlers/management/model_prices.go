package management

import (
	"net/http"
	"sync"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/modelcatalog"
	internalUsage "github.com/Pyrokine/CLIProxyAPI/v6/internal/usage"
	"github.com/gin-gonic/gin"
	log "github.com/sirupsen/logrus"
)

const modelPricesFileName = "model-prices.json"

type modelPriceEntry struct {
	Prompt     float64 `json:"prompt"`
	Completion float64 `json:"completion"`
	Cache      float64 `json:"cache"`
}

var modelPricesMu sync.RWMutex

func (h *Handler) GetModelPrices(c *gin.Context) {
	doc, err := modelcatalog.LoadOrInit(h.configFilePath, h.cfg)
	if err != nil {
		log.Warnf("model-prices: failed to read models.json: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to read model prices"})
		return
	}
	prices := make(map[string]modelcatalog.Price)
	for _, model := range doc.Models {
		if model.Price != nil {
			prices[model.Name] = *model.Price
		}
	}
	out := make(map[string]modelPriceEntry, len(prices))
	for name, price := range prices {
		out[name] = modelPriceEntry{Prompt: price.Prompt, Completion: price.Completion, Cache: price.Cache}
	}
	c.JSON(http.StatusOK, gin.H{"prices": out})
}

func (h *Handler) PutModelPrices(c *gin.Context) {
	if h.configFilePath == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "config file path not set"})
		return
	}
	var body struct {
		Prices map[string]modelPriceEntry `json:"prices"`
	}
	if err := c.ShouldBindJSON(&body); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid json"})
		return
	}
	if body.Prices == nil {
		body.Prices = make(map[string]modelPriceEntry)
	}
	prices := make(map[string]modelcatalog.Price, len(body.Prices))
	for name, price := range body.Prices {
		prices[name] = modelcatalog.Price{Prompt: price.Prompt, Completion: price.Completion, Cache: price.Cache}
	}
	modelPricesMu.Lock()
	doc, err := modelcatalog.ReplacePrices(h.configFilePath, prices)
	modelPricesMu.Unlock()
	if err != nil {
		log.Warnf("model-prices: failed to save models.json prices: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to save model prices"})
		return
	}
	h.applyModelCatalogRuntime(doc)
	response := gin.H{"status": "ok", "count": len(body.Prices), "recalculation": false}
	if h.usagePersister == nil {
		c.JSON(http.StatusOK, response)
		return
	}
	priceFn := internalUsage.BuildModelPriceFunc(h.configFilePath)
	h.usagePersister.SetPriceFunc(priceFn)
	if !h.usagePersister.HasPricing() {
		response["recalculation_error"] = "no model prices configured"
		c.JSON(http.StatusOK, response)
		return
	}
	result, err := h.usagePersister.RecalculateCosts()
	response["recalculation"] = true
	if err != nil {
		log.Errorf("model-prices: recalculate failed: %v", err)
		response["recalculation_error"] = err.Error()
		c.JSON(http.StatusOK, response)
		return
	}
	response["recalculated_days"] = result.RecalculatedDays
	response["total_cost"] = result.TotalCost
	if result.AlreadyRunning {
		response["status"] = "busy"
		response["recalculation_error"] = "cost recalculation already running"
	}
	c.JSON(http.StatusOK, response)
}
