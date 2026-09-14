package management

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	"github.com/gin-gonic/gin"
	log "github.com/sirupsen/logrus"
)

var (
	errOAuthTimeout          = errors.New("timeout waiting for OAuth callback")
	errOAuthSessionCancelled = errors.New("OAuth session cancelled")
)

// waitForOAuthCallback polls authDir for an OAuth callback file and returns the parsed result.
// Returns (nil, error) on timeout or when the session is no longer pending.
func waitForOAuthCallback(
	authDir string,
	provider string,
	state string,
	timeout time.Duration,
) (map[string]string, error) {
	waitFile := filepath.Join(authDir, fmt.Sprintf(".oauth-%s-%s.oauth", provider, state))
	deadline := time.Now().Add(timeout)
	for {
		if !isOAuthSessionPending(state, provider) {
			return nil, errOAuthSessionCancelled
		}
		data, errRead := os.ReadFile(waitFile)
		if errRead == nil {
			var payload oauthCallbackFilePayload
			if errUnmarshal := json.Unmarshal(data, &payload); errUnmarshal != nil {
				setOAuthSessionError(state, "Invalid OAuth callback payload")
				return nil, fmt.Errorf("decode oauth callback payload: %w", errUnmarshal)
			}
			payload.Code = strings.TrimSpace(payload.Code)
			payload.State = strings.TrimSpace(payload.State)
			payload.Error = strings.TrimSpace(payload.Error)
			if payload.State != strings.TrimSpace(state) {
				setOAuthSessionError(state, "Invalid OAuth callback state")
				return nil, fmt.Errorf("oauth callback state mismatch")
			}
			if payload.Code == "" && payload.Error == "" {
				setOAuthSessionError(state, "Invalid OAuth callback payload")
				return nil, fmt.Errorf("oauth callback contains neither code nor error")
			}
			if errRemove := os.Remove(waitFile); errRemove != nil {
				setOAuthSessionError(state, "Failed to finalize OAuth callback")
				return nil, fmt.Errorf("remove oauth callback file: %w", errRemove)
			}
			return map[string]string{
				"code":  payload.Code,
				"state": payload.State,
				"error": payload.Error,
			}, nil
		}
		if !errors.Is(errRead, os.ErrNotExist) {
			setOAuthSessionError(state, "Failed to read OAuth callback")
			return nil, fmt.Errorf("read oauth callback file: %w", errRead)
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
	cfg *config.Config,
	provider string,
	port int,
	callbackPath string,
) (*callbackForwarder, bool) {
	if !isWebUIRequest(c) {
		return nil, true
	}
	targetURL, err := managementCallbackURL(cfg, callbackPath)
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
