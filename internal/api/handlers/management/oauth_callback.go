package management

import (
	"crypto/sha256"
	"errors"
	"net/http"
	"net/url"
	"strings"

	"github.com/gin-gonic/gin"
	log "github.com/sirupsen/logrus"
)

type oauthCallbackRequest struct {
	Provider    string `json:"provider"`
	RedirectURL string `json:"redirect_url"`
	Code        string `json:"code"`
	State       string `json:"state"`
	Error       string `json:"error"`
}

type oauthCallbackInitiator struct {
	sessionID [sha256.Size]byte
	nonce     string
}

func oauthRequestNonce(c *gin.Context) string {
	if header := c.GetHeader(oauthNonceHeader); header != "" || ManagementSessionID(c.Request.Context()) != ([sha256.Size]byte{}) {
		return header
	}
	return c.Query("nonce")
}

func (session oauthSession) acceptsManualCallback(initiator oauthCallbackInitiator) bool {
	return session.Initiator == ([sha256.Size]byte{}) ||
		(session.Initiator == initiator.sessionID && session.verifyNonce(initiator.nonce))
}

func (session oauthSession) authorizeManagementRequest(c *gin.Context) (bool, int) {
	initiator := oauthCallbackInitiator{
		sessionID: ManagementSessionID(c.Request.Context()), nonce: oauthRequestNonce(c),
	}
	if session.Initiator != ([sha256.Size]byte{}) {
		return session.acceptsManualCallback(initiator), http.StatusForbidden
	}
	if initiator.sessionID != ([sha256.Size]byte{}) {
		return session.verifyNonce(initiator.nonce), http.StatusForbidden
	}
	return session.verifyNonce(initiator.nonce), http.StatusUnauthorized
}

func (h *Handler) GetOAuthCallback(c *gin.Context) {
	req := oauthCallbackRequest{
		Provider: strings.TrimSpace(c.Query("provider")),
		Code:     strings.TrimSpace(c.Query("code")),
		State:    strings.TrimSpace(c.Query("state")),
		Error:    strings.TrimSpace(c.Query("error")),
	}
	if req.Error == "" {
		req.Error = strings.TrimSpace(c.Query("error_description"))
	}
	if req.Provider == "" {
		if session, ok := oauthSessions.get(req.State); ok {
			req.Provider = session.Provider
		}
	}
	h.handleOAuthCallback(c, req, nil)
}

func (h *Handler) PostOAuthCallback(c *gin.Context) {
	c.Header("Cache-Control", "no-store")
	var req oauthCallbackRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		rejectMutation(c.Request.Context())
		c.JSON(http.StatusBadRequest, gin.H{"status": "error", "error": "invalid body"})
		return
	}
	initiator := oauthCallbackInitiator{
		sessionID: ManagementSessionID(c.Request.Context()), nonce: oauthRequestNonce(c),
	}
	h.handleOAuthCallback(c, req, &initiator)
}

func (h *Handler) handleOAuthCallback(c *gin.Context, req oauthCallbackRequest, manual *oauthCallbackInitiator) {
	c.Header("Cache-Control", "no-store")
	if h == nil || h.configSnapshot() == nil {
		c.JSON(http.StatusInternalServerError, gin.H{"status": "error", "error": "handler not initialized"})
		return
	}

	state := strings.TrimSpace(req.State)
	code := strings.TrimSpace(req.Code)
	errMsg := strings.TrimSpace(req.Error)

	if rawRedirect := strings.TrimSpace(req.RedirectURL); rawRedirect != "" {
		u, errParse := url.Parse(rawRedirect)
		if errParse != nil {
			rejectMutation(c.Request.Context())
			c.JSON(http.StatusBadRequest, gin.H{"status": "error", "error": "invalid redirect_url"})
			return
		}
		q := u.Query()
		if state == "" {
			state = strings.TrimSpace(q.Get("state"))
		}
		if code == "" {
			code = strings.TrimSpace(q.Get("code"))
		}
		if errMsg == "" {
			errMsg = strings.TrimSpace(q.Get("error"))
			if errMsg == "" {
				errMsg = strings.TrimSpace(q.Get("error_description"))
			}
		}
	}

	if state == "" {
		rejectMutation(c.Request.Context())
		c.JSON(http.StatusBadRequest, gin.H{"status": "error", "error": "state is required"})
		return
	}
	if err := validateOAuthState(state); err != nil {
		rejectMutation(c.Request.Context())
		c.JSON(http.StatusBadRequest, gin.H{"status": "error", "error": "invalid state"})
		return
	}
	if code == "" && errMsg == "" {
		rejectMutation(c.Request.Context())
		c.JSON(http.StatusBadRequest, gin.H{"status": "error", "error": "code or error is required"})
		return
	}

	session, ok := oauthSessions.get(state)
	if !ok || session.CallbackAccepted || session.Completed || session.Saving {
		rejectMutation(c.Request.Context())
		c.JSON(http.StatusNotFound, gin.H{"status": "error", "error": "unknown or expired state"})
		return
	}
	if manual != nil && !session.acceptsManualCallback(*manual) {
		rejectMutation(c.Request.Context())
		c.JSON(http.StatusForbidden, gin.H{"status": "error", "error": "oauth initiator does not match"})
		return
	}
	if session.Status != "" {
		rejectMutation(c.Request.Context())
		c.JSON(http.StatusConflict, gin.H{"status": "error", "error": "oauth flow is not pending"})
		return
	}
	canonicalProvider, err := normalizeOAuthCallbackProviderForPendingSession(req.Provider, state)
	if err != nil {
		rejectMutation(c.Request.Context())
		c.JSON(http.StatusBadRequest, gin.H{"status": "error", "error": "unsupported provider"})
		return
	}
	if !strings.EqualFold(session.Provider, canonicalProvider) {
		rejectMutation(c.Request.Context())
		c.JSON(http.StatusBadRequest, gin.H{"status": "error", "error": "provider does not match state"})
		return
	}
	if _, errWrite := writePendingOAuthCallback(
		canonicalProvider, state, code, errMsg, manual,
	); errWrite != nil {
		if errors.Is(errWrite, errOAuthSessionNotPending) {
			rejectMutation(c.Request.Context())
			c.JSON(http.StatusConflict, gin.H{"status": "error", "error": "oauth flow is not pending"})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"status": "error", "error": "failed to persist oauth callback"})
		return
	}
	RecordMutationEffect(c.Request.Context(), "task.persisted", "task")
	CompleteMutation(c.Request.Context())
	log.WithField("provider", canonicalProvider).Info("OAuth callback accepted")

	c.JSON(http.StatusOK, gin.H{"status": "ok", "phase": "accepted"})
}
