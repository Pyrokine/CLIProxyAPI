package management

import (
	"context"
	"net/http"
	"strings"
	"time"

	metaauth "github.com/Pyrokine/CLIProxyAPI/v6/internal/auth/meta"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/misc"
	sdkAuth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/auth"
	"github.com/gin-gonic/gin"
	log "github.com/sirupsen/logrus"
)

func (h *Handler) RequestMetaToken(c *gin.Context) {
	ctx := populateAuthContext(context.Background(), c)
	flowCfg, ok := h.authFlowConfig(c)
	if !ok {
		return
	}
	state, err := misc.GenerateRandomState()
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to create authentication session"})
		return
	}
	authSvc := metaauth.NewMetaAuth(flowCfg)
	device, err := authSvc.StartDeviceFlow(ctx)
	if err != nil {
		log.WithError(err).Warn("Meta device authorization could not start")
		c.JSON(http.StatusBadGateway, gin.H{"error": "Meta device authorization could not start"})
		return
	}
	url := strings.TrimSpace(device.VerificationURIComplete)
	if url == "" {
		url = strings.TrimSpace(device.VerificationURI)
	}
	state = "meta-" + state
	registerOAuthSessionForAuthDir(state, "meta", strings.TrimSpace(c.Query("nonce")), flowCfg.AuthDir)
	go func() {
		pollCtx, cancel := context.WithCancel(ctx)
		defer cancel()
		if !setOAuthSessionCancel(state, "meta", cancel) {
			return
		}
		bundle, err := authSvc.WaitForAuthorization(pollCtx, device)
		if err != nil {
			if isOAuthSessionPending(state, "meta") {
				log.WithError(err).Warn("Meta device authorization did not complete")
				setOAuthSessionError(state, oauthSessionErrorWithCause("Meta authentication failed", err))
			}
			return
		}
		record, err := sdkAuth.MetaRecordFromBundle(authSvc, bundle)
		if err != nil {
			log.WithError(err).Warn("Meta device authorization did not yield a usable API key")
			setOAuthSessionError(state, "Meta authorization did not yield a usable API key")
			return
		}
		if _, err := h.saveOAuthTokenRecord(pollCtx, flowCfg.AuthDir, state, "meta", record); err != nil {
			log.WithError(err).Warn("Meta credential could not be saved")
			setOAuthSessionError(state, "Meta credential could not be saved")
			return
		}
		completeOAuthSession(state)
	}()
	expiresIn := device.ExpiresIn
	if expiresIn <= 0 {
		expiresIn = int(metaauth.MaxPollDuration / time.Second)
	}
	c.JSON(http.StatusOK, gin.H{
		"status": "ok", "url": url, "state": state, "flow": "device",
		"user_code": strings.TrimSpace(device.UserCode), "expires_in": expiresIn,
	})
}
