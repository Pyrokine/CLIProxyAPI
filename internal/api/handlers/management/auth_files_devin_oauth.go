package management

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	devinauth "github.com/Pyrokine/CLIProxyAPI/v6/internal/auth/devin"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/misc"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/util"
	coreauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
	"github.com/gin-gonic/gin"
	log "github.com/sirupsen/logrus"
)

const devinManagementOAuthTimeout = 5 * time.Minute

type devinOAuthService interface {
	BuildAuthorizationURL(redirectURI, codeChallenge, state string) string
	ExchangeCodeForToken(ctx context.Context, code, codeVerifier string) (string, error)
	CreateAuthRecord(ctx context.Context, token string) (*coreauth.Auth, error)
}

var newDevinOAuthService = func(cfg *config.Config) devinOAuthService {
	client := util.SetProxy(&cfg.SDKConfig, &http.Client{Timeout: 30 * time.Second})
	return devinauth.NewDevinAuthService(client)
}

// RequestDevinToken starts a bounded management OAuth session for Devin.
func (h *Handler) RequestDevinToken(c *gin.Context) {
	flowCfg, ok := h.authFlowConfig(c)
	if !ok {
		return
	}
	pkce, err := devinauth.GeneratePKCECodes()
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to generate PKCE codes"})
		return
	}
	randomState, err := misc.GenerateRandomState()
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to generate state parameter"})
		return
	}
	state := "devin-" + randomState

	redirectURI, forwarder, err := devinManagementCallback(flowCfg)
	if err != nil {
		log.WithError(err).Error("failed to prepare Devin OAuth callback")
		c.JSON(http.StatusInternalServerError, gin.H{"error": "callback server unavailable"})
		return
	}
	service := newDevinOAuthService(flowCfg)
	authURL := service.BuildAuthorizationURL(redirectURI, pkce.CodeChallenge, state)
	registerOAuthSessionForAuthDir(state, "devin", strings.TrimSpace(c.Query("nonce")), flowCfg.AuthDir)

	flowContext := populateAuthContext(context.Background(), c)
	go h.completeDevinOAuth(
		flowContext,
		flowCfg.AuthDir,
		state,
		pkce.CodeVerifier,
		service,
		forwarder,
	)

	response := gin.H{"status": "ok", "url": authURL, "state": state}
	if forwarder != nil {
		response["callback_port"] = forwarder.port
	}
	c.JSON(http.StatusOK, response)
}

func devinManagementCallback(cfg *config.Config) (string, *callbackForwarder, error) {
	targetURL, err := managementCallbackURL(cfg, "/callback")
	if err != nil {
		return "", nil, err
	}
	if cfg != nil && !cfg.TLS.Enable {
		return targetURL, nil, nil
	}
	forwarder, err := startCallbackForwarder(0, "devin", targetURL)
	if err != nil {
		return "", nil, err
	}
	return fmt.Sprintf("http://127.0.0.1:%d/callback", forwarder.port), forwarder, nil
}

func (h *Handler) completeDevinOAuth(
	ctx context.Context,
	authDir string,
	state string,
	codeVerifier string,
	service devinOAuthService,
	forwarder *callbackForwarder,
) {
	flowContext, cancelFlow := context.WithTimeout(ctx, devinManagementOAuthTimeout)
	defer cancelFlow()
	pollContext, cancelPoll := context.WithCancel(flowContext)
	defer cancelPoll()
	if forwarder != nil {
		defer stopCallbackForwarderInstance(forwarder.port, forwarder)
	}
	if !setOAuthSessionCancel(state, "devin", cancelPoll) {
		return
	}

	result, err := waitForOAuthCallback(authDir, "devin", state, devinManagementOAuthTimeout)
	if err != nil {
		if isOAuthSessionPending(state, "devin") {
			setOAuthSessionError(state, "Failed to receive Devin authorization callback")
		}
		return
	}
	if !isOAuthSessionPending(state, "devin") {
		return
	}
	if result["error"] != "" {
		setOAuthSessionError(state, "Devin authorization denied")
		return
	}
	code := strings.TrimSpace(result["code"])
	if code == "" {
		setOAuthSessionError(state, "Missing authorization code")
		return
	}

	token, err := service.ExchangeCodeForToken(pollContext, code, codeVerifier)
	if err != nil || strings.TrimSpace(token) == "" {
		if isOAuthSessionPending(state, "devin") {
			setOAuthSessionError(state, "Failed to exchange authorization code")
		}
		return
	}
	if !isOAuthSessionPending(state, "devin") {
		return
	}
	record, err := service.CreateAuthRecord(pollContext, token)
	if err != nil || record == nil {
		if isOAuthSessionPending(state, "devin") {
			setOAuthSessionError(state, "Failed to create Devin credential")
		}
		return
	}

	savedPath, err := h.saveOAuthTokenRecord(flowContext, authDir, state, "devin", record)
	if err != nil {
		if !errors.Is(err, errOAuthSessionNotPending) {
			log.WithError(err).Error("failed to save Devin credential")
			setOAuthSessionError(state, "Failed to save credential")
		}
		return
	}
	completeOAuthSession(state)
	log.WithField("file", savedPath).Info("Devin authentication successful")
}
