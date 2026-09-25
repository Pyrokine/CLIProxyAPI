package auth

import (
	"context"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/clienterror"
	cliproxyexecutor "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
)

type homePreparedAttempt struct {
	ctx         context.Context
	auth        *Auth
	request     cliproxyexecutor.Request
	options     cliproxyexecutor.Options
	aliasResult OAuthModelAliasResult
}

func (m *Manager) prepareHomeAttempt(
	ctx context.Context,
	selection *HomeDispatchSelection,
	auth *Auth,
	req cliproxyexecutor.Request,
	opts cliproxyexecutor.Options,
	routeModel string,
) (homePreparedAttempt, error) {
	if selection == nil || selection.Executor == nil || auth == nil {
		return homePreparedAttempt{}, &Error{Code: "executor_not_found", Message: "executor not registered"}
	}
	publishAuthRouteTraceEvent(
		opts, auth, cliproxyexecutor.RouteTraceEvent{
			Provider: selection.Provider,
			Model:    routeModel,
			Action:   "attempt.prepare_started",
		},
	)
	attemptCtx := contextWithRequestedModelAlias(ctx, opts, routeModel)
	prepareStarted := time.Now()
	preparedAuth, errPrepare := m.prepareRequestAuth(attemptCtx, selection.Executor, auth)
	warnLogUpstreamFailure(attemptCtx, auth, selection.Provider, routeModel, prepareStarted, errPrepare, cliproxyexecutor.DispatchPhaseNotSent)
	if errPrepare != nil {
		resultCode, httpStatus, retryAfterMS := routeTraceError(opts.Metadata, errPrepare, "prepare_failed")
		publishAuthRouteTraceEvent(
			opts, auth, cliproxyexecutor.RouteTraceEvent{
				Provider:     selection.Provider,
				Model:        routeModel,
				Action:       "attempt.prepare_failed",
				ResultCode:   resultCode,
				HTTPStatus:   httpStatus,
				RetryAfterMS: retryAfterMS,
			},
		)
		return homePreparedAttempt{}, errPrepare
	}
	if preparedAuth == nil {
		preparedAuth = auth.Clone()
	}
	preserveHomeRoutingAttributes(preparedAuth, auth)
	if rt := m.roundTripperFor(preparedAuth); rt != nil {
		attemptCtx = context.WithValue(attemptCtx, roundTripperContextKey{}, rt)
	}

	execReq := req
	aliasResult := OAuthModelAliasResult{}
	if preparedAuth.Attributes != nil && strings.TrimSpace(preparedAuth.Attributes[homeUpstreamModelAttributeKey]) != "" {
		execReq.Model = strings.TrimSpace(preparedAuth.Attributes[homeUpstreamModelAttributeKey])
		aliasResult = homeForceMappingAliasResult(preparedAuth, execReq.Model)
	} else {
		execReq.Model = rewriteModelForAuth(routeModel, preparedAuth)
		aliasResult = m.resolveExecutionAliasResultForRequested(preparedAuth, execReq.Model)
		execReq.Model = aliasResult.UpstreamModel
	}
	execOpts := opts
	execOpts.ExecutionLifecycle = selection
	var errIntercept error
	execReq, execOpts, errIntercept = applyRequestAfterAuthInterceptor(
		attemptCtx, selection.Executor, selection.Provider, preparedAuth, execReq, execOpts, routeModel,
	)
	if errIntercept != nil {
		resultCode, httpStatus, retryAfterMS := routeTraceError(opts.Metadata, errIntercept, "request_intercepted")
		publishAuthRouteTraceEvent(
			opts, preparedAuth, cliproxyexecutor.RouteTraceEvent{
				Provider:     selection.Provider,
				Model:        routeModel,
				TargetModel:  execReq.Model,
				Action:       "attempt.intercept_failed",
				ResultCode:   resultCode,
				HTTPStatus:   httpStatus,
				RetryAfterMS: retryAfterMS,
			},
		)
		return homePreparedAttempt{}, errIntercept
	}
	execReq, errIntercept = attachResolvedAPIKeyModelInfo(
		execReq, preparedAuth, routeModel, aliasResult.UpstreamModel,
	)
	if errIntercept != nil {
		resultCode, httpStatus, retryAfterMS := routeTraceError(opts.Metadata, errIntercept, "model_not_supported")
		publishAuthRouteTraceEvent(
			opts, preparedAuth, cliproxyexecutor.RouteTraceEvent{
				Provider:     selection.Provider,
				Model:        routeModel,
				TargetModel:  execReq.Model,
				Action:       "attempt.model_rejected",
				ResultCode:   resultCode,
				HTTPStatus:   httpStatus,
				RetryAfterMS: retryAfterMS,
			},
		)
		return homePreparedAttempt{}, errIntercept
	}
	execReq = sanitizeDownstreamWebsocketFallbackRequest(attemptCtx, preparedAuth, execReq)
	publishSelectedAuthMetadata(opts.Metadata, preparedAuth)
	if opts.Stream {
		attemptCtx = contextWithRouteTraceStream(attemptCtx, opts, execReq.Model)
	}
	publishAuthRouteTraceEvent(
		opts, preparedAuth, cliproxyexecutor.RouteTraceEvent{
			Provider:    selection.Provider,
			Model:       routeModel,
			TargetModel: execReq.Model,
			Action:      "attempt.prepare_completed",
			ResultCode:  "success",
		},
	)
	return homePreparedAttempt{
		ctx:         attemptCtx,
		auth:        preparedAuth,
		request:     execReq,
		options:     execOpts,
		aliasResult: aliasResult,
	}, nil
}

// RefreshHomeSelectionAfterUnauthorized 只读取 Home 已安装的新 token 快照，不刷新或修改凭证
func (m *Manager) RefreshHomeSelectionAfterUnauthorized(
	_ context.Context,
	selection *HomeDispatchSelection,
	failedAuth *Auth,
) (*Auth, bool, error) {
	if m == nil || selection == nil {
		return nil, false, nil
	}
	current := selection.CloneAuth()
	if current != nil && failedAuth != nil && current.ID == failedAuth.ID &&
		!current.Disabled && current.Status != StatusDisabled {
		currentHash, failedHash := AccessTokenSHA256(current), AccessTokenSHA256(failedAuth)
		if currentHash != "" && failedHash != "" && currentHash != failedHash {
			return current, true, nil
		}
	}
	return current, false, nil
}

func (m *Manager) executeHome(
	ctx context.Context,
	req cliproxyexecutor.Request,
	opts cliproxyexecutor.Options,
	countTokens bool,
) (cliproxyexecutor.Response, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if unlockSession := m.lockHomeWebsocketSession(ctx, opts); unlockSession != nil {
		defer unlockSession()
	}
	routeModel := req.Model
	opts = ensureRequestedModelMetadata(opts, routeModel)
	opts = ensureRouteTraceState(opts)
	tried := make(map[string]struct{})
	if state := retryRoundFromContext(ctx); state != nil {
		tried = state.attempted
	}
	var lastErr error
	for count := 1; ; count++ {
		if err := ctx.Err(); err != nil {
			return cliproxyexecutor.Response{}, err
		}
		attemptOrdinal, previousAttempt := nextRouteTraceAttempt(opts)
		attemptOpts := withRouteTraceAttempt(opts, attemptOrdinal, previousAttempt)
		selection, errSelection := m.pickHomeDispatchSelection(
			ctx, routeModel, withHomeAuthCount(attemptOpts, count),
		)
		if errSelection != nil {
			returnedErr := errSelection
			if shouldReturnLastErrorOnPickFailure(true, lastErr, errSelection) || (lastErr != nil && isHomeCooldownError(errSelection)) {
				returnedErr = lastErr
				if state := retryRoundFromContext(ctx); state != nil && isHomeCooldownError(errSelection) {
					returnedErr = &homeDispatchRetryAfterError{cause: lastErr, retryAfter: state.homeWait}
				}
			}
			resultCode, httpStatus, retryAfterMS := routeTraceError(opts.Metadata, returnedErr, "home_dispatch_failed")
			publishRouteTraceEvent(
				opts.Metadata, cliproxyexecutor.RouteTraceEvent{
					Attempt:         attemptOrdinal,
					PreviousAttempt: previousAttempt,
					Provider:        "home",
					Model:           routeModel,
					Action:          "route.selection_failed",
					Reason:          "home_dispatch_failed",
					ResultCode:      resultCode,
					HTTPStatus:      httpStatus,
					RetryAfterMS:    retryAfterMS,
				},
			)
			return cliproxyexecutor.Response{}, returnedErr
		}
		auth := selection.CloneAuthForRoute(routeModel)
		if auth == nil || selection.Executor == nil {
			selection.End("missing_execution_target")
			publishRouteTraceEvent(
				opts.Metadata, cliproxyexecutor.RouteTraceEvent{
					Attempt:         attemptOrdinal,
					PreviousAttempt: previousAttempt,
					Provider:        selection.Provider,
					Model:           routeModel,
					Action:          "route.selection_failed",
					Reason:          "missing_execution_target",
					ResultCode:      "executor_not_found",
				},
			)
			return cliproxyexecutor.Response{}, &Error{Code: "executor_not_found", Message: "executor not registered"}
		}
		if _, exists := tried[auth.ID]; exists {
			selection.End("repeated_auth")
			traceCandidateExcluded(attemptOpts, auth, routeModel, "already_tried")
			if lastErr != nil {
				return cliproxyexecutor.Response{}, lastErr
			}
			return cliproxyexecutor.Response{}, repeatedHomeAuthError()
		}
		m.recordRouteDecisionCandidate(attemptOpts, auth, routeModel)
		publishSelectedOnlyRouteDecision(
			attemptOpts,
			[]string{selection.Provider},
			routeModel,
			schedulerStrategyCustom,
			auth,
			"home_dispatch",
			"",
		)
		publishAuthRouteTraceEvent(
			attemptOpts, auth, cliproxyexecutor.RouteTraceEvent{
				Provider: selection.Provider,
				Model:    routeModel,
				Action:   "route.selected",
				Reason:   "home_dispatch",
			},
		)
		tried[auth.ID] = struct{}{}
		if errBind := m.bindHomeSelectionRuntimeAuth(ctx, opts, selection); errBind != nil {
			selection.End("runtime_auth_bind_failed")
			return cliproxyexecutor.Response{}, errBind
		}

		attemptCtx, releaseAttempt, errAttempt := selection.AttemptContext(ctx)
		if errAttempt != nil {
			selection.End("attempt_bind_failed")
			return cliproxyexecutor.Response{}, errAttempt
		}
		attempt, errPrepare := m.prepareHomeAttempt(attemptCtx, selection, auth, req, attemptOpts, routeModel)
		if errPrepare != nil {
			releaseAttempt()
			if isRequestScopedError(errPrepare) {
				selection.End("request_intercepted")
				return cliproxyexecutor.Response{}, errPrepare
			}
			result := executionResult(
				auth, selection.Provider, routeModel, errPrepare, cliproxyexecutor.DispatchPhaseNotSent,
			)
			m.recordHomeExecutionResult(attemptCtx, result, countTokens)
			resultCode, httpStatus, retryAfterMS := routeTraceError(opts.Metadata, errPrepare, "prepare_failed")
			recordedEvent := cliproxyexecutor.RouteTraceEvent{
				Provider:     selection.Provider,
				Model:        routeModel,
				Action:       "result.recorded",
				ResultCode:   resultCode,
				HTTPStatus:   httpStatus,
				RetryAfterMS: retryAfterMS,
			}
			applyDispositionToRouteTrace(&recordedEvent, result.Disposition, result.StateGeneration)
			publishAuthRouteTraceEvent(attemptOpts, auth, recordedEvent)
			lastErr = errPrepare
			retryRoundFromContext(ctx).observeHomeAttempt(errPrepare)
			if !retryDirectiveAllowsRedispatch(result.Disposition) {
				selection.End("retry_forbidden")
				return cliproxyexecutor.Response{}, errPrepare
			}
			publishAuthRouteTraceEvent(
				attemptOpts, auth, cliproxyexecutor.RouteTraceEvent{
					Provider: selection.Provider,
					Model:    routeModel,
					Action:   "attempt.retry",
					Reason:   string(result.Disposition.RetryDirective),
				},
			)
			if errEnd := m.endHomeSelectionBeforeRedispatch(ctx, selection, "prepare_failed"); errEnd != nil {
				return cliproxyexecutor.Response{}, errEnd
			}
			continue
		}

		effectiveAuth := attempt.auth.Clone()
		var effectiveAuthMu sync.RWMutex
		setEffectiveAuth := func(observed *Auth) {
			if observed == nil || AccessTokenSHA256(observed) == "" {
				return
			}
			effectiveAuthMu.Lock()
			effectiveAuth = observed.Clone()
			effectiveAuthMu.Unlock()
		}
		getEffectiveAuth := func() *Auth {
			effectiveAuthMu.RLock()
			defer effectiveAuthMu.RUnlock()
			return effectiveAuth.Clone()
		}
		if countTokens {
			attempt.ctx = withAccessTokenFingerprintObserver(attempt.ctx, setEffectiveAuth)
		}
		execute := func(current homePreparedAttempt) (cliproxyexecutor.Response, error) {
			if countTokens {
				return selection.Executor.CountTokens(
					current.ctx, current.auth, current.request, externalRouteTraceOptions(current.options),
				)
			}
			return selection.Executor.Execute(
				current.ctx, current.auth, current.request, externalRouteTraceOptions(current.options),
			)
		}

		publishAuthRouteTraceEvent(
			attemptOpts, attempt.auth, cliproxyexecutor.RouteTraceEvent{
				Provider:    selection.Provider,
				Model:       routeModel,
				TargetModel: attempt.request.Model,
				Action:      "attempt.execute_started",
			},
		)
		started := time.Now()
		response, errExecute := execute(attempt)
		warnLogUpstreamFailure(attempt.ctx, attempt.auth, selection.Provider, attempt.request.Model, started, errExecute, cliproxyexecutor.DispatchPhaseMaybeSent)
		if countTokens && isUnauthorizedError(errExecute) {
			failedAuth := getEffectiveAuth()
			failureBody := errExecute.Error()
			if body, direct := clienterror.ResponseBodyFromError(errExecute); direct {
				failureBody = string(body)
			}
			m.reportHomeUnauthorized(
				attempt.ctx, failedAuth, selection.Provider, routeModel, AccessTokenSHA256(failedAuth), failureBody,
			)
		}
		phase := cliproxyexecutor.DispatchPhaseResponseStarted
		if errExecute != nil {
			phase = cliproxyexecutor.DispatchPhaseMaybeSent
		}
		result := executionResult(attempt.auth, selection.Provider, routeModel, errExecute, phase)
		decision := requestScopedActionDecision{err: errExecute}
		compactRequestFault := false
		if errExecute != nil {
			result.RetryAfter = retryAfterFromError(errExecute)
			decision = m.applyRequestScopedErrorRule(attempt.auth, errExecute, &result)
			errExecute = decision.err
			if !decision.matched {
				compactRequestFault = applyResponsesCompactErrorDisposition(opts, errExecute, &result)
			}
		}
		if !decision.matched && isRequestScopedError(errExecute) {
			resultCode, httpStatus, retryAfterMS := routeTraceError(opts.Metadata, errExecute, "request_scoped")
			publishResultRouteTraceEvent(
				attemptOpts, attempt.auth, cliproxyexecutor.RouteTraceEvent{
					Provider:     selection.Provider,
					Model:        routeModel,
					TargetModel:  attempt.request.Model,
					Action:       "attempt.failed",
					Reason:       "request_scoped",
					ResultCode:   resultCode,
					HTTPStatus:   httpStatus,
					RetryAfterMS: retryAfterMS,
					FinalTarget:  true,
				}, result,
			)
			releaseAttempt()
			selection.End("request_invalid")
			return cliproxyexecutor.Response{}, errExecute
		}
		resultCode, httpStatus, retryAfterMS := routeTraceError(opts.Metadata, errExecute, "execution_failed")
		if errExecute == nil {
			httpStatus = http.StatusOK
			publishResultRouteTraceEvent(
				attemptOpts, attempt.auth, cliproxyexecutor.RouteTraceEvent{
					Provider:    selection.Provider,
					Model:       routeModel,
					TargetModel: attempt.request.Model,
					Action:      "attempt.completed",
					ResultCode:  resultCode,
					HTTPStatus:  http.StatusOK,
					FinalTarget: true,
				}, result,
			)
		} else {
			publishResultRouteTraceEvent(
				attemptOpts, attempt.auth, cliproxyexecutor.RouteTraceEvent{
					Provider:     selection.Provider,
					Model:        routeModel,
					TargetModel:  attempt.request.Model,
					Action:       "attempt.failed",
					Reason:       "upstream_failure",
					ResultCode:   resultCode,
					HTTPStatus:   httpStatus,
					RetryAfterMS: retryAfterMS,
					FinalTarget:  !retryDirectiveAllowsRedispatch(result.Disposition),
				}, result,
			)
		}
		m.recordHomeExecutionResult(attempt.ctx, result, countTokens)
		publishResultRouteTraceEvent(
			attemptOpts, attempt.auth, cliproxyexecutor.RouteTraceEvent{
				Provider:     selection.Provider,
				Model:        routeModel,
				TargetModel:  attempt.request.Model,
				Action:       "result.recorded",
				ResultCode:   resultCode,
				HTTPStatus:   httpStatus,
				RetryAfterMS: retryAfterMS,
				FinalTarget:  errExecute == nil,
			}, result,
		)
		releaseAttempt()
		if errExecute == nil {
			rewriteForceMappedResponse(&response, attempt.aliasResult)
			if !m.retainHomeWebsocketSelection(ctx, opts, routeModel, selection) {
				selection.End("completed")
			}
			return response, nil
		}
		lastErr = errExecute
		retryRoundFromContext(ctx).observeHomeAttempt(errExecute)
		requestInvalid := !decision.matched && (compactRequestFault || isRequestInvalidError(errExecute))
		if decision.stop || requestInvalid || !retryDirectiveAllowsRedispatch(result.Disposition) {
			reason := "retry_forbidden"
			if decision.stop {
				reason = "request_scoped_rule"
			} else if requestInvalid {
				reason = "request_invalid"
			} else if result.Disposition.Outcome == cliproxyexecutor.ExecutionOutcomeUnknown {
				reason = "outcome_unknown"
			}
			selection.End(reason)
			if decision.stop {
				return cliproxyexecutor.Response{}, wrapRequestStopError(errExecute)
			}
			return cliproxyexecutor.Response{}, errExecute
		}
		publishAuthRouteTraceEvent(
			attemptOpts, attempt.auth, cliproxyexecutor.RouteTraceEvent{
				Provider: selection.Provider,
				Model:    routeModel,
				Action:   "attempt.retry",
				Reason:   string(result.Disposition.RetryDirective),
			},
		)
		if errEnd := m.endHomeSelectionBeforeRedispatch(ctx, selection, "execution_failed"); errEnd != nil {
			return cliproxyexecutor.Response{}, errEnd
		}
		if errContext := ctx.Err(); errContext != nil {
			return cliproxyexecutor.Response{}, errContext
		}
	}
}

func (m *Manager) recordHomeExecutionResult(ctx context.Context, result Result, countTokens bool) {
	if result.AuthID == "" {
		return
	}
	if countTokens && result.Error != nil && !isForceCooldownResult(result) &&
		isCountTokensEndpointNotFoundError(result.Error, result.Model) {
		m.recordAvailabilityNeutralResult(ctx, result)
		return
	}
	m.MarkResult(ctx, result)
}

func validateStreamResult(
	result *cliproxyexecutor.StreamResult,
	err error,
) (*cliproxyexecutor.StreamResult, error) {
	if err != nil {
		return result, err
	}
	if result == nil || result.Chunks == nil {
		return result, &Error{Code: "empty_stream", Message: "upstream stream has no source", Retryable: true}
	}
	return result, nil
}

func (m *Manager) startHomeStreamAttempt(
	attempt homePreparedAttempt,
	selection *HomeDispatchSelection,
	routeModel string,
) (*cliproxyexecutor.StreamResult, error) {
	streamResult, errStream := selection.Executor.ExecuteStream(
		attempt.ctx, attempt.auth, attempt.request, externalRouteTraceOptions(attempt.options),
	)
	if errStream != nil {
		return streamResult, errStream
	}
	streamResult, errStream = validateStreamResult(streamResult, nil)
	if errStream != nil {
		return streamResult, newStreamBootstrapError(errStream, nil)
	}
	buffered, closed, bootstrapErr := readStreamBootstrap(attempt.ctx, streamResult.Chunks)
	if bootstrapErr != nil {
		discardStreamChunks(streamResult.Chunks)
		return streamResult, newStreamBootstrapError(bootstrapErr, streamResult.Headers)
	}
	if closed && len(buffered) == 0 {
		return streamResult, newStreamBootstrapError(
			&Error{Code: "empty_stream", Message: "upstream stream closed before first payload", Retryable: true},
			streamResult.Headers,
		)
	}
	remaining := streamResult.Chunks
	if closed {
		closedCh := make(chan cliproxyexecutor.StreamChunk)
		close(closedCh)
		remaining = closedCh
	}
	return m.wrapStreamResult(
		attempt.ctx,
		attempt.auth.Clone(),
		selection.Provider,
		routeModel,
		streamResult.Headers,
		buffered,
		remaining,
		attempt.aliasResult,
	), nil
}

func (m *Manager) executeHomeStream(
	ctx context.Context,
	req cliproxyexecutor.Request,
	opts cliproxyexecutor.Options,
) (*cliproxyexecutor.StreamResult, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if unlockSession := m.lockHomeWebsocketSession(ctx, opts); unlockSession != nil {
		defer unlockSession()
	}
	routeModel := req.Model
	opts = ensureRequestedModelMetadata(opts, routeModel)
	opts = ensureRouteTraceState(opts)
	tried := make(map[string]struct{})
	if state := retryRoundFromContext(ctx); state != nil {
		tried = state.attempted
	}
	var lastErr error
	for count := 1; ; count++ {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		attemptOrdinal, previousAttempt := nextRouteTraceAttempt(opts)
		attemptOpts := withRouteTraceAttempt(opts, attemptOrdinal, previousAttempt)
		selection, errSelection := m.pickHomeDispatchSelection(
			ctx, routeModel, withHomeAuthCount(attemptOpts, count),
		)
		if errSelection != nil {
			returnedErr := errSelection
			if shouldReturnLastErrorOnPickFailure(true, lastErr, errSelection) || (lastErr != nil && isHomeCooldownError(errSelection)) {
				returnedErr = lastErr
				if state := retryRoundFromContext(ctx); state != nil && isHomeCooldownError(errSelection) {
					returnedErr = &homeDispatchRetryAfterError{cause: lastErr, retryAfter: state.homeWait}
				}
			}
			resultCode, httpStatus, retryAfterMS := routeTraceError(opts.Metadata, returnedErr, "home_dispatch_failed")
			publishRouteTraceEvent(
				opts.Metadata, cliproxyexecutor.RouteTraceEvent{
					Attempt:         attemptOrdinal,
					PreviousAttempt: previousAttempt,
					Provider:        "home",
					Model:           routeModel,
					Action:          "route.selection_failed",
					Reason:          "home_dispatch_failed",
					ResultCode:      resultCode,
					HTTPStatus:      httpStatus,
					RetryAfterMS:    retryAfterMS,
				},
			)
			return nil, returnedErr
		}
		auth := selection.CloneAuthForRoute(routeModel)
		if auth == nil || selection.Executor == nil {
			selection.End("missing_execution_target")
			publishRouteTraceEvent(
				opts.Metadata, cliproxyexecutor.RouteTraceEvent{
					Attempt:         attemptOrdinal,
					PreviousAttempt: previousAttempt,
					Provider:        selection.Provider,
					Model:           routeModel,
					Action:          "route.selection_failed",
					Reason:          "missing_execution_target",
					ResultCode:      "executor_not_found",
				},
			)
			return nil, &Error{Code: "executor_not_found", Message: "executor not registered"}
		}
		if _, exists := tried[auth.ID]; exists {
			selection.End("repeated_auth")
			traceCandidateExcluded(attemptOpts, auth, routeModel, "already_tried")
			if lastErr != nil {
				return nil, lastErr
			}
			return nil, repeatedHomeAuthError()
		}
		m.recordRouteDecisionCandidate(attemptOpts, auth, routeModel)
		publishSelectedOnlyRouteDecision(
			attemptOpts,
			[]string{selection.Provider},
			routeModel,
			schedulerStrategyCustom,
			auth,
			"home_dispatch",
			"",
		)
		publishAuthRouteTraceEvent(
			attemptOpts, auth, cliproxyexecutor.RouteTraceEvent{
				Provider: selection.Provider,
				Model:    routeModel,
				Action:   "route.selected",
				Reason:   "home_dispatch",
			},
		)
		tried[auth.ID] = struct{}{}
		if errBind := m.bindHomeSelectionRuntimeAuth(ctx, opts, selection); errBind != nil {
			selection.End("runtime_auth_bind_failed")
			return nil, errBind
		}

		attemptCtx, releaseAttempt, errAttempt := selection.AttemptContext(ctx)
		if errAttempt != nil {
			selection.End("attempt_bind_failed")
			return nil, errAttempt
		}
		attempt, errPrepare := m.prepareHomeAttempt(attemptCtx, selection, auth, req, attemptOpts, routeModel)
		if errPrepare != nil {
			releaseAttempt()
			if isRequestScopedError(errPrepare) {
				selection.End("request_intercepted")
				return nil, errPrepare
			}
			result := executionResult(
				auth, selection.Provider, routeModel, errPrepare, cliproxyexecutor.DispatchPhaseNotSent,
			)
			m.recordHomeExecutionResult(attemptCtx, result, false)
			resultCode, httpStatus, retryAfterMS := routeTraceError(opts.Metadata, errPrepare, "prepare_failed")
			recordedEvent := cliproxyexecutor.RouteTraceEvent{
				Provider:     selection.Provider,
				Model:        routeModel,
				Action:       "result.recorded",
				ResultCode:   resultCode,
				HTTPStatus:   httpStatus,
				RetryAfterMS: retryAfterMS,
			}
			applyDispositionToRouteTrace(&recordedEvent, result.Disposition, result.StateGeneration)
			publishAuthRouteTraceEvent(attemptOpts, auth, recordedEvent)
			lastErr = errPrepare
			retryRoundFromContext(ctx).observeHomeAttempt(errPrepare)
			if !retryDirectiveAllowsRedispatch(result.Disposition) {
				selection.End("retry_forbidden")
				return nil, errPrepare
			}
			publishAuthRouteTraceEvent(
				attemptOpts, auth, cliproxyexecutor.RouteTraceEvent{
					Provider: selection.Provider,
					Model:    routeModel,
					Action:   "attempt.retry",
					Reason:   string(result.Disposition.RetryDirective),
				},
			)
			if errEnd := m.endHomeSelectionBeforeRedispatch(ctx, selection, "prepare_failed"); errEnd != nil {
				return nil, errEnd
			}
			continue
		}

		publishAuthRouteTraceEvent(
			attemptOpts, attempt.auth, cliproxyexecutor.RouteTraceEvent{
				Provider:    selection.Provider,
				Model:       routeModel,
				TargetModel: attempt.request.Model,
				Action:      "attempt.execute_started",
			},
		)
		failurePhase := cliproxyexecutor.DispatchPhaseMaybeSent
		started := time.Now()
		streamResult, errStream := m.startHomeStreamAttempt(attempt, selection, routeModel)
		warnLogUpstreamFailure(attempt.ctx, attempt.auth, selection.Provider, attempt.request.Model, started, errStream, failurePhase)
		if errStream == nil {
			publishAuthRouteTraceEvent(
				attemptOpts, attempt.auth, cliproxyexecutor.RouteTraceEvent{
					Provider:    selection.Provider,
					Model:       routeModel,
					TargetModel: attempt.request.Model,
					Action:      "attempt.stream_started",
					ResultCode:  "stream_started",
					HTTPStatus:  http.StatusOK,
				},
			)
			retained := m.retainHomeWebsocketSelection(ctx, opts, routeModel, selection)
			return wrapHomeOwnedStream(attempt.ctx, streamResult, selection, releaseAttempt, !retained), nil
		}
		result := executionResult(attempt.auth, selection.Provider, routeModel, errStream, failurePhase)
		result.RetryAfter = retryAfterFromError(errStream)
		decision := m.applyRequestScopedErrorRule(attempt.auth, errStream, &result)
		errStream = decision.err
		resultCode, httpStatus, retryAfterMS := routeTraceError(opts.Metadata, errStream, "stream_bootstrap_failed")
		publishResultRouteTraceEvent(
			attemptOpts, attempt.auth, cliproxyexecutor.RouteTraceEvent{
				Provider:     selection.Provider,
				Model:        routeModel,
				TargetModel:  attempt.request.Model,
				Action:       "attempt.failed",
				Reason:       "stream_bootstrap_failed",
				ResultCode:   resultCode,
				HTTPStatus:   httpStatus,
				RetryAfterMS: retryAfterMS,
				FinalTarget:  !retryDirectiveAllowsRedispatch(result.Disposition),
			}, result,
		)
		releaseAttempt()
		if !decision.matched && isRequestScopedError(errStream) {
			selection.End("request_invalid")
			return nil, errStream
		}
		m.recordHomeExecutionResult(attempt.ctx, result, false)
		publishResultRouteTraceEvent(
			attemptOpts, attempt.auth, cliproxyexecutor.RouteTraceEvent{
				Provider:     selection.Provider,
				Model:        routeModel,
				TargetModel:  attempt.request.Model,
				Action:       "result.recorded",
				ResultCode:   resultCode,
				HTTPStatus:   httpStatus,
				RetryAfterMS: retryAfterMS,
			}, result,
		)
		lastErr = errStream
		retryRoundFromContext(ctx).observeHomeAttempt(errStream)
		requestInvalid := !decision.matched && isRequestInvalidError(errStream)
		if decision.stop || requestInvalid || !retryDirectiveAllowsRedispatch(result.Disposition) {
			reason := "retry_forbidden"
			if decision.stop {
				reason = "request_scoped_rule"
			} else if requestInvalid {
				reason = "request_invalid"
			} else if result.Disposition.Outcome == cliproxyexecutor.ExecutionOutcomeUnknown {
				reason = "outcome_unknown"
			}
			selection.End(reason)
			if decision.stop {
				return nil, wrapRequestStopError(errStream)
			}
			return nil, errStream
		}
		publishAuthRouteTraceEvent(
			attemptOpts, attempt.auth, cliproxyexecutor.RouteTraceEvent{
				Provider: selection.Provider,
				Model:    routeModel,
				Action:   "attempt.retry",
				Reason:   string(result.Disposition.RetryDirective),
			},
		)
		if errEnd := m.endHomeSelectionBeforeRedispatch(ctx, selection, "stream_start_failed"); errEnd != nil {
			return nil, errEnd
		}
		if errContext := ctx.Err(); errContext != nil {
			return nil, errContext
		}
	}
}

func wrapHomeOwnedStream(
	ctx context.Context,
	result *cliproxyexecutor.StreamResult,
	selection *HomeDispatchSelection,
	releaseAttempt func(),
	endSelection bool,
) *cliproxyexecutor.StreamResult {
	if result == nil || result.Chunks == nil {
		if releaseAttempt != nil {
			releaseAttempt()
		}
		if selection != nil && endSelection {
			selection.End("stream_missing")
		}
		return result
	}
	out := make(chan cliproxyexecutor.StreamChunk)
	go func() {
		defer close(out)
		if releaseAttempt != nil {
			defer releaseAttempt()
		}
		if selection != nil && endSelection {
			defer selection.End("stream_closed")
		}
		for {
			select {
			case <-ctx.Done():
				discardStreamChunks(result.Chunks)
				return
			case chunk, ok := <-result.Chunks:
				if !ok {
					return
				}
				select {
				case <-ctx.Done():
					discardStreamChunks(result.Chunks)
					return
				case out <- chunk:
				}
			}
		}
	}()
	return &cliproxyexecutor.StreamResult{Headers: cloneHTTPHeader(result.Headers), Chunks: out}
}
