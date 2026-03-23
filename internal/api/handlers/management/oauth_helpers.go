package management

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/gin-gonic/gin"
	log "github.com/sirupsen/logrus"
)

var (
	errOAuthTimeout          = errors.New("timeout waiting for OAuth callback")
	errOAuthSessionCancelled = errors.New("OAuth session cancelled")
)

// waitForOAuthCallback polls authDir for an OAuth callback file and returns the parsed result.
// Returns (nil, error) on timeout or when the session is no longer pending.
func (h *Handler) waitForOAuthCallback(provider, state string, timeout time.Duration) (map[string]string, error) {
	waitFile := filepath.Join(h.cfg.AuthDir, fmt.Sprintf(".oauth-%s-%s.oauth", provider, state))
	deadline := time.Now().Add(timeout)
	for {
		if !isOAuthSessionPending(state, provider) {
			return nil, errOAuthSessionCancelled
		}
		data, err := os.ReadFile(waitFile)
		if err == nil {
			_ = os.Remove(waitFile)
			var m map[string]string
			_ = json.Unmarshal(data, &m)
			return m, nil
		}
		if time.Now().After(deadline) {
			setOAuthSessionError(state, "Timeout waiting for OAuth callback")
			return nil, errOAuthTimeout
		}
		time.Sleep(500 * time.Millisecond)
	}
}

// setupOAuthForwarder starts a callback forwarder for WebUI OAuth flows.
// Returns the forwarder (for cleanup) and true on success. On failure, writes error to c and returns (nil, false).
// If not a WebUI request, returns (nil, true) — caller should skip forwarder cleanup.
func (h *Handler) setupOAuthForwarder(
	c *gin.Context,
	provider string,
	port int,
	callbackPath string,
) (*callbackForwarder, bool) {
	if !isWebUIRequest(c) {
		return nil, true
	}
	targetURL, err := h.managementCallbackURL(callbackPath)
	if err != nil {
		log.WithError(err).Errorf("failed to compute %s callback target", provider)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "callback server unavailable"})
		return nil, false
	}
	forwarder, err := startCallbackForwarder(port, provider, targetURL)
	if err != nil {
		log.WithError(err).Errorf("failed to start %s callback forwarder", provider)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to start callback server"})
		return nil, false
	}
	return forwarder, true
}
