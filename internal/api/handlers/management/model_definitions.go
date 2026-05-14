package management

import (
	"net/http"
	"strings"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/registry"
	"github.com/gin-gonic/gin"
)

// GetStaticModelDefinitions returns static model metadata for a given channel.
// Channel is provided via path param (:channel) or query param (?channel=...).
func (h *Handler) GetStaticModelDefinitions(c *gin.Context) {
	channel := strings.TrimSpace(c.Param("channel"))
	if channel == "" {
		channel = strings.TrimSpace(c.Query("channel"))
	}
	if channel == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "channel is required"})
		return
	}

	models := registry.GetStaticModelDefinitionsByChannel(channel)
	if models == nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "unknown channel", "channel": channel})
		return
	}

	c.JSON(
		http.StatusOK, gin.H{
			"channel": strings.ToLower(strings.TrimSpace(channel)),
			"models":  models,
		},
	)
}

// GetModelsCatalogMeta returns the current state of the remote model catalog
// so the UI can show the source URL and refresh timing.
func (h *Handler) GetModelsCatalogMeta(c *gin.Context) {
	c.JSON(http.StatusOK, registry.GetCatalogMeta())
}

// PostModelsCatalogRefresh triggers an immediate model catalog refresh.
func (h *Handler) PostModelsCatalogRefresh(c *gin.Context) {
	meta, err := registry.TriggerCatalogRefresh(c.Request.Context())
	if err != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": err.Error(), "meta": meta})
		return
	}
	c.JSON(http.StatusOK, meta)
}
