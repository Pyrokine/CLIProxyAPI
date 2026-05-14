package management

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"sync"

	internalUsage "github.com/Pyrokine/CLIProxyAPI/v6/internal/usage"
	"github.com/gin-gonic/gin"
	log "github.com/sirupsen/logrus"
)

const modelPricesFileName = "model-prices.json"

// modelPriceEntry represents pricing for a single model (in $/1M tokens).
type modelPriceEntry struct {
	Prompt     float64 `json:"prompt"`
	Completion float64 `json:"completion"`
	Cache      float64 `json:"cache"`
}

var modelPricesMu sync.RWMutex

func (h *Handler) modelPricesFilePath() string {
	if h.configFilePath == "" {
		return ""
	}
	return filepath.Join(filepath.Dir(h.configFilePath), modelPricesFileName)
}

// GetModelPrices returns the stored model pricing configuration.
func (h *Handler) GetModelPrices(c *gin.Context) {
	filePath := h.modelPricesFilePath()
	if filePath == "" {
		c.JSON(http.StatusOK, gin.H{"prices": map[string]modelPriceEntry{}})
		return
	}

	modelPricesMu.RLock()
	defer modelPricesMu.RUnlock()

	data, err := os.ReadFile(filePath)
	if err != nil {
		if os.IsNotExist(err) {
			c.JSON(http.StatusOK, gin.H{"prices": map[string]modelPriceEntry{}})
			return
		}
		log.Warnf("model-prices: failed to read %s: %v", filePath, err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to read model prices"})
		return
	}

	var prices map[string]modelPriceEntry
	if err := json.Unmarshal(data, &prices); err != nil {
		log.Warnf("model-prices: failed to parse %s: %v", filePath, err)
		c.JSON(http.StatusOK, gin.H{"prices": map[string]modelPriceEntry{}})
		return
	}

	c.JSON(http.StatusOK, gin.H{"prices": prices})
}

// PutModelPrices replaces the stored model pricing configuration.
func (h *Handler) PutModelPrices(c *gin.Context) {
	filePath := h.modelPricesFilePath()
	if filePath == "" {
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

	data, err := json.MarshalIndent(body.Prices, "", "  ")
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to serialize prices"})
		return
	}

	modelPricesMu.Lock()
	defer modelPricesMu.Unlock()

	dir := filepath.Dir(filePath)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to create directory"})
		return
	}

	tmpPath := filePath + ".tmp"
	if err := os.WriteFile(tmpPath, data, 0o600); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to write model prices"})
		return
	}
	if err := os.Rename(tmpPath, filePath); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to save model prices"})
		return
	}

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
