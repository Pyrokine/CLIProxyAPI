package management

import (
	"net/http"
	"strings"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/managementasset"
	"github.com/gin-gonic/gin"
)

// GetPanelUpdateState compares the installed panel with the requested release without publishing files.
func (h *Handler) GetPanelUpdateState(c *gin.Context) {
	cfg := h.configSnapshot()
	if cfg != nil && cfg.RemoteManagement.DisableControlPanel {
		c.JSON(
			http.StatusBadRequest,
			gin.H{"error": "control_panel_disabled", "message": "Control panel is disabled in configuration"},
		)
		return
	}
	staticDir := managementasset.StaticDir(h.configFilePath)
	if staticDir == "" {
		c.JSON(
			http.StatusInternalServerError,
			gin.H{"error": "static_dir_unknown", "message": "Cannot resolve static directory"},
		)
		return
	}
	version := strings.TrimSpace(c.Query("version"))
	if version != "" && !validVersion.MatchString(version) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid_version", "message": "Version format is invalid"})
		return
	}
	proxyURL, panelRepo := "", ""
	if cfg != nil {
		proxyURL = strings.TrimSpace(cfg.ProxyURL)
		panelRepo = strings.TrimSpace(cfg.RemoteManagement.PanelGitHubRepository)
	}
	state, err := managementasset.ReadManagementHTMLState(c.Request.Context(), staticDir, proxyURL, panelRepo, version)
	if err != nil {
		c.JSON(
			http.StatusBadGateway,
			gin.H{"error": "panel_state_unavailable", "message": "Failed to read management panel state"},
		)
		return
	}
	c.JSON(http.StatusOK, state)
}
