package management

import (
	"bytes"
	"context"
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/access"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executionregistry"
	"github.com/gin-gonic/gin"
)

func (h *Handler) authenticateUpdateGuard(c *gin.Context) (*updateGuardSession, *updateHealthTokenState, bool) {
	if h == nil || h.stateStore == nil || c == nil || c.Request == nil {
		if c != nil {
			c.AbortWithStatus(http.StatusServiceUnavailable)
		}
		return nil, nil, false
	}
	c.Header("Cache-Control", "no-store")
	if !access.ResolveClientAddress(c.Request, nil).DirectLoopback {
		c.AbortWithStatus(http.StatusForbidden)
		return nil, nil, false
	}
	values := c.Request.Header.Values(updateHealthTokenHeader)
	if len(values) != 1 {
		c.AbortWithStatus(http.StatusUnauthorized)
		return nil, nil, false
	}
	provided := strings.TrimSpace(values[0])
	ctx, cancel := context.WithTimeout(c.Request.Context(), updateGuardHTTPTimeout)
	defer cancel()
	token, err := h.stateStore.loadUpdateHealthToken(ctx)
	if err != nil || token == nil || !isUpdateHealthToken(token.Token) ||
		token.ExpiresUnix != token.ExpiresAt.Unix() || !time.Now().Before(token.ExpiresAt) ||
		len(provided) != len(token.Token) || subtle.ConstantTimeCompare([]byte(provided), []byte(token.Token)) != 1 {
		c.AbortWithStatus(http.StatusUnauthorized)
		return nil, nil, false
	}
	session, err := h.stateStore.loadUpdateSession(ctx)
	if err != nil || session == nil || session.AttemptID != token.AttemptID || !isActiveUpdatePhase(session.Phase) {
		c.AbortWithStatus(http.StatusUnauthorized)
		return nil, nil, false
	}
	return session, token, true
}

func (h *Handler) PostUpdateDrain(c *gin.Context) {
	session, _, ok := h.authenticateUpdateGuard(c)
	if !ok {
		return
	}
	if session.SchemaVersion != updateGuardStateSchemaVersion || session.OriginInstance == "" ||
		h.inferenceAdmission == nil {
		c.AbortWithStatus(http.StatusConflict)
		return
	}
	var request struct {
		AttemptID string `json:"attempt_id"`
		Action    string `json:"action"`
	}
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, 1024)
	if err := c.ShouldBindJSON(&request); err != nil || request.AttemptID != session.AttemptID {
		c.AbortWithStatus(http.StatusBadRequest)
		return
	}
	var confirmation executionregistry.DrainConfirmation
	switch request.Action {
	case "wait":
		if session.Phase != updatePhaseDraining || h.inferenceAdmission.InstanceID() != session.OriginInstance ||
			!time.Now().Before(session.DrainDeadline) {
			c.AbortWithStatus(http.StatusConflict)
			return
		}
		ctx, cancel := context.WithDeadline(c.Request.Context(), session.DrainDeadline)
		defer cancel()
		var err error
		confirmation, err = h.inferenceAdmission.Drain(ctx, session.AttemptID, session.Deadline)
		if err != nil {
			c.AbortWithStatus(http.StatusConflict)
			return
		}
	case "confirm":
		if session.Phase != updatePhaseInstalling || h.inferenceAdmission.InstanceID() != session.OriginInstance ||
			session.DrainConfirmation == nil || !h.inferenceAdmission.Commit(*session.DrainConfirmation) {
			c.AbortWithStatus(http.StatusConflict)
			return
		}
		confirmation = *session.DrainConfirmation
	case "resume":
		h.inferenceAdmission.Resume(session.AttemptID)
		c.JSON(http.StatusOK, gin.H{"status": "ok"})
		return
	default:
		c.AbortWithStatus(http.StatusBadRequest)
		return
	}
	c.JSON(http.StatusOK, confirmation)
}

func requestUpdateDrain(
	ctx context.Context,
	session updateGuardSession,
	token, action string,
) (executionregistry.DrainConfirmation, error) {
	client, err := newUpdateGuardHTTPClient(session)
	if err != nil {
		return executionregistry.DrainConfirmation{}, err
	}
	if action == "wait" {
		client.Timeout = updateGuardDrainTimeout + updateGuardHTTPTimeout
	}
	body, err := json.Marshal(map[string]string{"attempt_id": session.AttemptID, "action": action})
	if err != nil {
		return executionregistry.DrainConfirmation{}, err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, session.HealthBaseURL+updateDrainPath, bytes.NewReader(body))
	if err != nil {
		return executionregistry.DrainConfirmation{}, err
	}
	request.Header.Set(updateHealthTokenHeader, token)
	request.Header.Set("Content-Type", "application/json")
	response, err := client.Do(request)
	if err != nil {
		return executionregistry.DrainConfirmation{}, err
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, maxUpdateHealthResponseBytes))
		return executionregistry.DrainConfirmation{}, fmt.Errorf("inference drain %s returned status %d", action, response.StatusCode)
	}
	if action == "resume" {
		return executionregistry.DrainConfirmation{}, nil
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, maxUpdateHealthResponseBytes+1))
	if err != nil || len(data) > maxUpdateHealthResponseBytes {
		return executionregistry.DrainConfirmation{}, fmt.Errorf("invalid inference drain response")
	}
	expectedDeadline := session.Deadline
	if action == "confirm" && session.DrainConfirmation != nil {
		expectedDeadline = session.DrainConfirmation.Deadline
	}
	var confirmation executionregistry.DrainConfirmation
	if err = json.Unmarshal(data, &confirmation); err != nil || confirmation.AttemptID != session.AttemptID ||
		confirmation.InstanceID != session.OriginInstance || !confirmation.Deadline.Equal(expectedDeadline) ||
		!time.Now().Before(confirmation.Deadline) {
		return executionregistry.DrainConfirmation{}, fmt.Errorf("inference drain identity mismatch")
	}
	return confirmation, nil
}
