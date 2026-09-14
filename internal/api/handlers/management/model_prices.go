package management

import (
	"maps"
	"net/http"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/modelcatalog"
	internalUsage "github.com/Pyrokine/CLIProxyAPI/v6/internal/usage"
	"github.com/gin-gonic/gin"
	log "github.com/sirupsen/logrus"
)

type modelPriceEntry = modelcatalog.Price

func (h *Handler) GetModelPrices(c *gin.Context) {
	doc, err := modelcatalog.LoadOrInit(h.configFilePath, h.configSnapshot())
	if err != nil {
		log.Warnf("model-prices: failed to read models.json: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to read model prices"})
		return
	}
	prices := make(map[string]modelPriceEntry, len(doc.Prices))
	maps.Copy(prices, doc.Prices)
	c.JSON(http.StatusOK, gin.H{"prices": prices})
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
	maps.Copy(prices, body.Prices)
	result, ok := h.mutateModelCatalog(
		c,
		modelCatalogMutationOptions{},
		func(doc *modelcatalog.File) error {
			modelcatalog.SetPrices(doc, prices)
			return nil
		},
	)
	if result.config != nil && h.usagePersister != nil {
		priceProvider := internalUsage.BuildModelPriceProvider(h.configFilePath)
		h.usagePersister.SetPriceProvider(priceProvider)
	}
	if !ok {
		return
	}
	response := gin.H{"status": "ok", "count": len(body.Prices), "recalculation": false}
	if h.usagePersister == nil {
		c.JSON(http.StatusOK, response)
		return
	}
	if !h.usagePersister.HasPricing() {
		response["recalculation_error"] = "no model prices configured"
		c.JSON(http.StatusOK, response)
		return
	}
	response["recalculation"] = true
	response["recalculation_pending"] = true
	if !h.usagePersister.StartRecalculateCosts() {
		response["status"] = "busy"
		response["already_running"] = true
	}
	c.JSON(http.StatusOK, response)
}
