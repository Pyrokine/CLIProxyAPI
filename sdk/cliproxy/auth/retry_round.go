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
	upstreamErr                         error
}

func retryRoundFromContext(ctx context.Context) *retryRoundState {
	if ctx == nil {
		return nil
	}
	state, _ := ctx.Value(retryRoundContextKey{}).(*retryRoundState)
	return state
}

func (s *retryRoundState) observeUpstreamError(err error, disposition cliproxyexecutor.ExecutionDisposition) {
	if s != nil && err != nil && disposition.DispatchPhase != cliproxyexecutor.DispatchPhaseNotSent {
		s.upstreamErr = err
	}
}

func (s *retryRoundState) preferredError(err error) error {
	if s == nil || err == nil || s.upstreamErr == nil || errors.Is(err, s.upstreamErr) ||
		errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) ||
		isRequestStopError(err) || isRequestScopedError(err) || isRequestInvalidError(err) {
		return err
	}
	disposition := executionDispositionForError(err, cliproxyexecutor.DispatchPhaseNotSent)
	_, typed := errors.AsType[cliproxyexecutor.ExecutionDispositionError](err)
	current, _ := errors.AsType[*Error](err)
	if typed && disposition.DispatchPhase != cliproxyexecutor.DispatchPhaseNotSent &&
		(current == nil || current.executionState().DispatchPhase != "") {
		return err
	}
	// 返回上游原因，重放许可和当前等待时间仍由最后一次失败决定
	disposition.DispatchPhase = cliproxyexecutor.DispatchPhaseNotSent
	var preferred error
	if statusCodeFromError(s.upstreamErr) == 0 && current != nil {
		preferred = &errorWithCause{
			base: errorForResultWithDisposition(err, disposition, 0), cause: s.upstreamErr,
		}
	} else {
		upstream := errorForResultWithDisposition(s.upstreamErr, disposition, 0)
		upstream.executionState().cause = s.upstreamErr
		preferred = upstream
	}
	if IsTerminalAuthError(err) {
		preferred = NewTerminalAuthError(errorForResultWithDisposition(err, disposition, 0), preferred)
	}
	if remote, ok := errors.AsType[*homeDispatchRetryAfterError](err); ok {
		copyRemote := *remote
		copyRemote.cause = preferred
		return &copyRemote
	}
	if retryAfter := retryAfterFromError(err); retryAfter != nil {
		return &homeDispatchRetryAfterError{cause: preferred, retryAfter: *retryAfter}
	}
	return preferred
}

func effectiveRequestRetryLimit(auth *Auth, fallback int) int {
	if value, present := auth.RequestRetryOverride(); present {
		return value
	}
	return max(fallback, 0)
}

func (s *retryRoundState) observeHomeLimit(value *int, auth *Auth, pinned bool) {
	if s == nil {
		return
	}
	if pinned && auth != nil {
		if override, present := auth.RequestRetryOverride(); present {
			if value != nil {
				override = min(override, max(*value, 0))
			}
			value = &override
		}
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

//goland:noinspection GoUnnecessarilyExportedIdentifiers
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
	constraints := home.AuthDispatchConstraints{
		PinnedAuthID: pinnedAuthIDFromMetadata(opts.Metadata), CredentialPolicy: credentialPolicyFromContext(ctx),
	}
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
	RPopAuthWithConstraints(context.Context, string, string, http.Header, int, home.AuthDispatchConstraints) (
		[]byte,
		error,
	)
}

type homeDispatchRetryAfterError struct {
	cause        error
	requestRetry *int
	retryAfter   time.Duration
}

func (e *homeDispatchRetryAfterError) Error() string { return e.cause.Error() }

//goland:noinspection GoUnnecessarilyExportedIdentifiers
func (e *homeDispatchRetryAfterError) Unwrap() error { return e.cause }

//goland:noinspection GoUnnecessarilyExportedIdentifiers
func (e *homeDispatchRetryAfterError) RetryAfter() *time.Duration { return &e.retryAfter }

func (s *retryRoundState) observeHomeError(err error) {
	if s == nil {
		return
	}
	if remote, ok := errors.AsType[*homeDispatchRetryAfterError](err); ok {
		s.observeHomeLimit(remote.requestRetry, nil, false)
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
		if err.executionState().DispatchPhase == "" && err.executionState().Outcome == "" &&
			err.executionState().RetryDirective == "" &&
			err.executionState().EffectScope == "" {
			switch err.HTTPStatus {
			case http.StatusBadRequest, http.StatusUnauthorized, http.StatusPaymentRequired, http.StatusNotFound:
				return false
			}
		}
		return retryDirectiveAllowsCooldownRetry(
			executionDispositionForError(
				err, cliproxyexecutor.DispatchPhaseMaybeSent,
			),
		)
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
