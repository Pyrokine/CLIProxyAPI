package auth

import (
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/redisqueue"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/util"
	log "github.com/sirupsen/logrus"
)

type errorEvent struct {
	Timestamp  time.Time            `json:"timestamp"`
	Provider   string               `json:"provider,omitempty"`
	Model      string               `json:"model,omitempty"`
	AuthID     string               `json:"auth_id,omitempty"`
	AuthIndex  string               `json:"auth_index"`
	StatusCode int                  `json:"status_code"`
	Body       string               `json:"body"`
	Code       string               `json:"code,omitempty"`
	Retryable  bool                 `json:"retryable,omitempty"`
	AuthStatus errorEventAuthStatus `json:"auth_status"`
}

type errorEventAuthStatus struct {
	Status         Status                 `json:"status"`
	StatusMessage  string                 `json:"status_message,omitempty"`
	Disabled       bool                   `json:"disabled"`
	Unavailable    bool                   `json:"unavailable"`
	NextRetryAfter *time.Time             `json:"next_retry_after,omitempty"`
	Quota          *errorEventQuotaStatus `json:"quota,omitempty"`
	Model          *errorEventModelStatus `json:"model,omitempty"`
}

type errorEventQuotaStatus struct {
	Exceeded      bool       `json:"exceeded"`
	Reason        string     `json:"reason,omitempty"`
	NextRecoverAt *time.Time `json:"next_recover_at,omitempty"`
	BackoffLevel  int        `json:"backoff_level,omitempty"`
}

type errorEventModelStatus struct {
	Name           string                 `json:"name"`
	Status         Status                 `json:"status"`
	StatusMessage  string                 `json:"status_message,omitempty"`
	Unavailable    bool                   `json:"unavailable"`
	NextRetryAfter *time.Time             `json:"next_retry_after,omitempty"`
	Quota          *errorEventQuotaStatus `json:"quota,omitempty"`
}

// SetErrorQueue 绑定认证错误事件队列，nil 关闭事件发布
func (m *Manager) SetErrorQueue(queue *redisqueue.Queue) {
	if m != nil {
		m.errorQueue.Store(queue)
	}
}

func (m *Manager) publishErrorEvent(result Result, authSnapshot *Auth) {
	if m == nil || result.Success || authSnapshot == nil || m.HomeEnabled() {
		return
	}
	queue := m.errorQueue.Load()
	if !queue.Enabled() {
		return
	}
	payload, errMarshal := buildErrorEventPayload(result, authSnapshot)
	if errMarshal != nil {
		log.WithError(errMarshal).Warn("failed to encode auth error event")
		return
	}
	queue.EnqueueError(payload)
}

func buildErrorEventPayload(result Result, authSnapshot *Auth) ([]byte, error) {
	authSnapshot.EnsureIndex()
	event := errorEvent{
		Timestamp:  time.Now(),
		Provider:   strings.TrimSpace(result.Provider),
		Model:      strings.TrimSpace(result.Model),
		AuthID:     strings.TrimSpace(result.AuthID),
		AuthIndex:  authSnapshot.Index,
		StatusCode: http.StatusInternalServerError,
		Body:       "request failed",
		AuthStatus: errorEventAuthStatus{
			Status:         authSnapshot.Status,
			StatusMessage:  strings.TrimSpace(authSnapshot.StatusMessage),
			Disabled:       authSnapshot.Disabled,
			Unavailable:    authSnapshot.Unavailable,
			NextRetryAfter: timePtrIfSet(authSnapshot.NextRetryAfter),
			Quota:          errorEventQuotaStatusFrom(authSnapshot.Quota),
		},
	}
	if result.Error != nil {
		if result.Error.HTTPStatus > 0 {
			event.StatusCode = result.Error.HTTPStatus
		}
		if body := strings.TrimSpace(result.Error.Message); body != "" {
			event.Body = body
		} else if body := strings.TrimSpace(result.Error.Error()); body != "" {
			event.Body = body
		}
		event.Code = strings.TrimSpace(result.Error.Code)
		event.Retryable = result.Error.Retryable
	}
	if state, model := findModelState(authSnapshot, result.Model); state != nil {
		event.AuthStatus.Model = &errorEventModelStatus{
			Name:           model,
			Status:         state.Status,
			StatusMessage:  strings.TrimSpace(state.StatusMessage),
			Unavailable:    state.Unavailable,
			NextRetryAfter: timePtrIfSet(state.NextRetryAfter),
			Quota:          errorEventQuotaStatusFrom(state.Quota),
		}
	}
	payload, errMarshal := json.Marshal(event)
	if errMarshal != nil {
		return nil, errMarshal
	}
	return util.MaskSensitivePayload(payload), nil
}

func errorEventQuotaStatusFrom(quota QuotaState) *errorEventQuotaStatus {
	if !quota.Exceeded && strings.TrimSpace(quota.Reason) == "" && quota.NextRecoverAt.IsZero() &&
		quota.BackoffLevel == 0 {
		return nil
	}
	return &errorEventQuotaStatus{
		Exceeded:      quota.Exceeded,
		Reason:        strings.TrimSpace(quota.Reason),
		NextRecoverAt: timePtrIfSet(quota.NextRecoverAt),
		BackoffLevel:  quota.BackoffLevel,
	}
}

func timePtrIfSet(value time.Time) *time.Time {
	if value.IsZero() {
		return nil
	}
	return &value
}
