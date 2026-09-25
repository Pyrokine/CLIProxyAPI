package auth

import (
	"context"
	"errors"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/home"
	cliproxyexecutor "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
)

type retryRoundContextKey struct{}

// retryRoundState belongs to one synchronous selection round, not an executor session.
type retryRoundState struct {
	round, defaultRetry, maxCredentials int
	attempted                           map[string]struct{}
	homeLimit                           int
	homeAuthority                       bool
	homeWait                            time.Duration
	hasHomeWait                         bool
	homeStopped                         bool
}

func retryRoundFromContext(ctx context.Context) *retryRoundState {
	if ctx == nil {
		return nil
	}
	state, _ := ctx.Value(retryRoundContextKey{}).(*retryRoundState)
	return state
}

func effectiveRequestRetryLimit(auth *Auth, fallback int) int {
	if value, present := auth.RequestRetryOverride(); present {
		return value
	}
	return max(fallback, 0)
}

func (s *retryRoundState) observeHomeLimit(value *int, auth *Auth) {
	if s == nil {
		return
	}
	if value != nil {
		s.homeLimit, s.homeAuthority = max(*value, 0), true
	} else if !s.homeAuthority && auth != nil {
		limit := effectiveRequestRetryLimit(auth, s.defaultRetry)
		if s.homeLimit < 0 || limit > s.homeLimit {
			s.homeLimit = limit
		}
	}
}

const ExcludedAuthIDsMetadataKey = "excluded_auth_ids"

func excludedAuthIDs(opts cliproxyexecutor.Options) map[string]struct{} {
	ids := make(map[string]struct{})
	add := func(id string) {
		if id = strings.TrimSpace(id); id != "" {
			ids[id] = struct{}{}
		}
	}
	switch value := opts.Metadata[ExcludedAuthIDsMetadataKey].(type) {
	case []string:
		for _, id := range value {
			add(id)
		}
	case []any:
		for _, id := range value {
			if text, ok := id.(string); ok {
				add(text)
			}
		}
	case map[string]struct{}:
		for id := range value {
			add(id)
		}
	case map[string]bool:
		for id, enabled := range value {
			if enabled {
				add(id)
			}
		}
	}
	return ids
}

func homeDispatchConstraints(ctx context.Context, opts cliproxyexecutor.Options) home.AuthDispatchConstraints {
	excluded := excludedAuthIDs(opts)
	constraints := home.AuthDispatchConstraints{PinnedAuthID: pinnedAuthIDFromMetadata(opts.Metadata), CredentialPolicy: credentialPolicyFromContext(ctx)}
	if state := retryRoundFromContext(ctx); state != nil {
		constraints.RetryRound = state.round
		for id := range state.attempted {
			excluded[id] = struct{}{}
		}
	}
	for id := range excluded {
		constraints.ExcludedAuthIDs = append(constraints.ExcludedAuthIDs, id)
	}
	sort.Strings(constraints.ExcludedAuthIDs)
	return constraints
}

type homeConstraintsDispatcher interface {
	RPopAuthWithConstraints(context.Context, string, string, http.Header, int, home.AuthDispatchConstraints) ([]byte, error)
}

type homeDispatchRetryAfterError struct {
	cause        error
	requestRetry *int
	retryAfter   time.Duration
}

func (e *homeDispatchRetryAfterError) Error() string              { return e.cause.Error() }
func (e *homeDispatchRetryAfterError) Unwrap() error              { return e.cause }
func (e *homeDispatchRetryAfterError) RetryAfter() *time.Duration { return &e.retryAfter }

func (s *retryRoundState) observeHomeError(err error) {
	if s == nil {
		return
	}
	var remote *homeDispatchRetryAfterError
	if errors.As(err, &remote) {
		s.observeHomeLimit(remote.requestRetry, nil)
		if isHomeCooldownError(err) {
			s.homeWait, s.hasHomeWait = remote.retryAfter, true
		}
	}
	var authErr *Error
	if errors.As(err, &authErr) && authErr.Code == "auth_unavailable" {
		s.homeWait, s.hasHomeWait = 0, true
	}
}

func retryRoundStateEligible(auth *Auth, model string, now time.Time) (time.Time, bool) {
	blocked, reason, next := isAuthBlockedForModel(auth, model, now)
	if !blocked {
		return time.Time{}, true
	}
	if reason == blockReasonDisabled || next.IsZero() {
		return time.Time{}, false
	}
	eligible := func(err *Error, quota bool) bool {
		if err == nil {
			return quota
		}
		return retryDirectiveAllowsCooldownRetry(executionDispositionForError(err, cliproxyexecutor.DispatchPhaseMaybeSent))
	}
	if auth.Unavailable && auth.NextRetryAfter.After(now) {
		return next, eligible(auth.LastError, auth.Quota.Exceeded)
	}
	state := auth.ModelStates[model]
	if state == nil {
		state = auth.ModelStates[canonicalModelKey(model)]
	}
	if state != nil && state.Unavailable && state.NextRetryAfter.After(now) {
		return next, eligible(state.LastError, state.Quota.Exceeded)
	}
	return next, false
}

func isHomeCooldownError(err error) bool {
	var remote *homeDispatchRetryAfterError
	var cause *Error
	return errors.As(err, &remote) && errors.As(remote.cause, &cause) && cause.Code == "model_cooldown"
}

func (s *retryRoundState) observeHomeAttempt(err error) {
	if s == nil || err == nil {
		return
	}
	wait := time.Duration(0)
	if retryAfter := retryAfterFromError(err); retryAfter != nil {
		wait = *retryAfter
	}
	if !s.hasHomeWait || wait < s.homeWait {
		s.homeWait, s.hasHomeWait = wait, true
	}
}
