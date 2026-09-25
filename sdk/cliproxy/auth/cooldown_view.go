package auth

import (
	"sort"
	"strings"
	"time"

	cliproxyexecutor "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
)

// CooldownView describes one active local retry restriction without exposing raw errors or credentials.
type CooldownView struct {
	Scope            string    `json:"scope"`
	ModelKey         string    `json:"model_key,omitempty"`
	Reason           string    `json:"reason"`
	RetryAt          time.Time `json:"retry_at"`
	RemainingSeconds int64     `json:"remaining_seconds"`
	BackoffLevel     *int      `json:"backoff_level,omitempty"`
	HTTPStatus       int       `json:"http_status,omitempty"`
}

// CooldownSnapshotForAuth projects active cooldown timers without mutating auth state.
func CooldownSnapshotForAuth(auth *Auth, now time.Time) []CooldownView {
	views := make([]CooldownView, 0)
	if auth == nil {
		return views
	}
	credentialScoped := auth.StateEffectScope == cliproxyexecutor.StateEffectScopeCredential ||
		auth.Quota.Reason == "credential_quota"
	if !credentialScoped && auth.LastError != nil {
		credentialScoped = auth.LastError.EffectScope == cliproxyexecutor.StateEffectScopeCredential
	}
	if credentialScoped {
		if blocked, _, next := cooldownViewBlock(
			auth.Unavailable,
			auth.Quota.Exceeded,
			auth.NextRetryAfter,
			auth.Quota.NextRecoverAt,
			now,
		); blocked && next.After(now) {
			views = append(
				views,
				newCooldownView("credential", "", next, now, auth.Quota, auth.StatusMessage, auth.LastError),
			)
		}
	} else if len(auth.ModelStates) == 0 {
		if blocked, _, next := cooldownViewBlock(
			auth.Unavailable,
			auth.Quota.Exceeded,
			auth.NextRetryAfter,
			auth.Quota.NextRecoverAt,
			now,
		); blocked && next.After(now) {
			views = append(
				views,
				newCooldownView("credential", "", next, now, auth.Quota, auth.StatusMessage, auth.LastError),
			)
		}
	}

	keys := make([]string, 0, len(auth.ModelStates))
	for key := range auth.ModelStates {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	type modelCooldown struct {
		view   CooldownView
		reason blockReason
	}
	byModel := make(map[string]modelCooldown)
	for _, key := range keys {
		state := auth.ModelStates[key]
		model := canonicalModelKey(key)
		if state == nil || model == "" {
			continue
		}
		blocked, reason, next := cooldownViewBlock(
			state.Unavailable,
			state.Quota.Exceeded,
			state.NextRetryAfter,
			state.Quota.NextRecoverAt,
			now,
		)
		if !blocked || !next.After(now) {
			continue
		}
		if previous, ok := byModel[model]; ok {
			preferQuotaTie := next.Equal(previous.view.RetryAt) &&
				reason == blockReasonCooldown && previous.reason != blockReasonCooldown
			if !next.After(previous.view.RetryAt) && !preferQuotaTie {
				continue
			}
		}
		byModel[model] = modelCooldown{
			view:   newCooldownView("model", model, next, now, state.Quota, state.StatusMessage, state.LastError),
			reason: reason,
		}
	}
	models := make([]string, 0, len(byModel))
	for model := range byModel {
		models = append(models, model)
	}
	sort.Strings(models)
	for _, model := range models {
		views = append(views, byModel[model].view)
	}
	return views
}

func cooldownViewBlock(
	unavailable bool,
	quotaExceeded bool,
	nextRetryAfter,
	nextRecoverAt,
	now time.Time,
) (bool, blockReason, time.Time) {
	if !unavailable && !quotaExceeded {
		return false, blockReasonNone, time.Time{}
	}
	hasRecoveryTime := !nextRetryAfter.IsZero() || !nextRecoverAt.IsZero()
	var next time.Time
	for _, candidate := range []time.Time{nextRetryAfter, nextRecoverAt} {
		if candidate.After(now) && (next.IsZero() || candidate.After(next)) {
			next = candidate
		}
	}
	if !next.IsZero() {
		if quotaExceeded {
			return true, blockReasonCooldown, next
		}
		return true, blockReasonOther, next
	}
	if hasRecoveryTime {
		return false, blockReasonNone, time.Time{}
	}
	return true, blockReasonOther, time.Time{}
}

func newCooldownView(
	scope,
	model string,
	next,
	now time.Time,
	quota QuotaState,
	statusMessage string,
	lastErr *Error,
) CooldownView {
	remaining := next.Sub(now)
	seconds := int64(remaining / time.Second)
	if remaining%time.Second != 0 {
		seconds++
	}
	view := CooldownView{
		Scope:            scope,
		ModelKey:         model,
		Reason:           "unknown",
		RetryAt:          next.UTC(),
		RemainingSeconds: seconds,
	}
	if quota.Exceeded && (quota.NextRecoverAt.IsZero() || !quota.NextRecoverAt.Before(next)) {
		switch quota.Reason {
		case "credential_quota", "quota":
			view.Reason = quota.Reason
		case "cloudflare challenge":
			view.Reason = "cloudflare_challenge"
		}
	}
	propagatedCredentialQuota := quota.Exceeded && quota.Reason == "credential_quota"
	if view.Reason == "credential_quota" {
		return view
	}
	if (view.Reason == "quota" || view.Reason == "cloudflare_challenge") && quota.BackoffLevel >= 0 {
		level := quota.BackoffLevel
		view.BackoffLevel = &level
	}
	errorReason := cooldownErrorReason(lastErr)
	if view.Reason == "unknown" {
		view.Reason = errorReason
	}
	if view.Reason == "unknown" {
		view.Reason = cooldownStatusReason(statusMessage)
	}
	if !propagatedCredentialQuota && lastErr != nil &&
		lastErr.HTTPStatus >= 400 && lastErr.HTTPStatus <= 599 && errorReason == view.Reason {
		view.HTTPStatus = lastErr.HTTPStatus
	}
	return view
}

func cooldownErrorReason(err *Error) string {
	switch {
	case err != nil && (isModelSupportError(err) || isExplicitModelNotFoundError(err, "")):
		return "model_not_supported"
	case isCloudflareChallengeResultError(err):
		return "cloudflare_challenge"
	case isInvalidGrantResultError(err):
		return "invalid_grant"
	}
	switch statusCodeFromResult(err) {
	case 401:
		return "unauthorized"
	case 402, 403:
		return "payment_required"
	case 404:
		return "not_found"
	case 429:
		return "quota"
	case 408, 500, 502, 503, 504, 520, 521, 522, 523, 524, 525, 526:
		return "transient_error"
	}
	if err != nil {
		return cooldownStatusReason(err.Code)
	}
	return "unknown"
}

func cooldownStatusReason(message string) string {
	switch strings.TrimSpace(message) {
	case "quota", "quota exhausted":
		return "quota"
	case "cloudflare challenge":
		return "cloudflare_challenge"
	case "invalid_grant", "unauthorized", "payment_required", "not_found":
		return strings.TrimSpace(message)
	case "model_not_supported":
		return "model_not_supported"
	case "transient upstream error":
		return "transient_error"
	default:
		return "unknown"
	}
}
