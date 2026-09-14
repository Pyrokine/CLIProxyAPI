package auth

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/home"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/logging"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executionregistry"
	cliproxyexecutor "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
)

type homeAmbiguousDispatcher interface {
	AbortAmbiguousDispatch()
}

func abortAmbiguousHomeDispatch(client homeAuthDispatcher) {
	if client == nil {
		return
	}
	if aborter, ok := client.(homeAmbiguousDispatcher); ok {
		aborter.AbortAmbiguousDispatch()
	}
}

func (m *Manager) pickHomeDispatchSelection(
	ctx context.Context,
	model string,
	opts cliproxyexecutor.Options,
) (*HomeDispatchSelection, error) {
	if m == nil {
		return nil, &Error{Code: "auth_not_found", Message: "no auth available"}
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if retained, okRetained, errRetained := m.retainedHomeSessionSelection(ctx, opts, model); errRetained != nil {
		return nil, errRetained
	} else if okRetained {
		return retained, nil
	}

	requestedModel := requestedModelFromMetadata(opts.Metadata, model)
	bundle := m.HomeDispatchBundle()
	if bundle == nil {
		client := m.homeDispatcher()
		if client != nil {
			bundle = &HomeDispatchBundle{client: client, registry: executionregistry.New()}
		}
	}
	if bundle == nil || bundle.client == nil || bundle.registry == nil {
		return nil, &Error{
			Code: "home_unavailable", Message: "home dispatch bundle unavailable",
			HTTPStatus: http.StatusServiceUnavailable,
		}
	}
	client := bundle.client
	registry := bundle.registry
	if !client.HeartbeatOK() {
		return nil, &Error{
			Code: "home_unavailable", Message: "home control center unavailable",
			HTTPStatus: http.StatusServiceUnavailable,
		}
	}
	pending, errBegin := registry.BeginDispatch()
	if errBegin != nil {
		return nil, &Error{
			Code: "home_unavailable", Message: "home execution registry unavailable", Retryable: true,
			HTTPStatus: http.StatusServiceUnavailable,
		}
	}

	sessionID := m.homeDispatchSessionID(opts)
	dispatchHeaders := homeDispatchHeaders(ctx, opts.Headers)
	credentialPolicy := credentialPolicyFromContext(ctx)
	var raw []byte
	var errRPop error
	if credentialPolicy == "" {
		raw, errRPop = client.RPopAuth(
			ctx, requestedModel, sessionID, dispatchHeaders, homeAuthCountFromMetadata(opts.Metadata),
		)
	} else if policyClient, okPolicy := client.(homeCredentialPolicyDispatcher); okPolicy {
		raw, errRPop = policyClient.RPopAuthWithPolicy(
			ctx,
			requestedModel,
			sessionID,
			dispatchHeaders,
			homeAuthCountFromMetadata(opts.Metadata),
			credentialPolicy,
		)
	} else {
		pending.End()
		return nil, &Error{
			Code:       "home_unavailable",
			Message:    "home dispatcher does not support credential policies",
			HTTPStatus: http.StatusServiceUnavailable,
		}
	}
	if errRPop != nil {
		if home.IsAmbiguousDispatchError(errRPop) {
			abortAmbiguousHomeDispatch(client)
		}
		pending.End()
		if errors.Is(errRPop, home.ErrAuthNotFound) {
			return nil, &Error{
				Code: "auth_not_found", Message: errRPop.Error(), HTTPStatus: http.StatusServiceUnavailable,
			}
		}
		return nil, &Error{
			Code: "home_unavailable", Message: errRPop.Error(), Retryable: true,
			HTTPStatus: http.StatusServiceUnavailable,
		}
	}

	concurrencyTuple, concurrencyPresent, errEnvelope := decodeHomeDispatchConcurrencyEnvelope(raw)
	if errEnvelope != nil {
		if concurrencyPresent {
			abortAmbiguousHomeDispatch(client)
		}
		pending.End()
		if concurrencyPresent {
			return nil, invalidHomeConcurrencyResponse("Home returned malformed concurrency tuple")
		}
		return nil, &Error{
			Code: "invalid_auth", Message: "home returned invalid auth payload", HTTPStatus: http.StatusBadGateway,
		}
	}

	kind := "http"
	if cliproxyexecutor.DownstreamWebsocket(ctx) {
		kind = "websocket"
	} else if opts.Stream {
		kind = "stream"
	}
	baseScope := executionregistry.ScopeSpec{
		RequestID: logging.GetRequestID(ctx),
		Model:     requestedModel,
		Kind:      kind,
		StartedAt: time.Now(),
	}
	var scope *executionregistry.Scope
	if concurrencyPresent {
		var errInstall error
		scope, errInstall = installHomeConcurrencyScope(registry, pending, concurrencyTuple, baseScope)
		if errInstall != nil {
			abortAmbiguousHomeDispatch(client)
			pending.End()
			return nil, homeConcurrencyInstallError(errInstall)
		}
	}
	endScope := func() {
		if scope != nil {
			scope.End("local_validation_failed")
			return
		}
		pending.End()
	}
	if errHome := decodeHomeDispatchError(raw); errHome != nil {
		if concurrencyPresent {
			abortAmbiguousHomeDispatch(client)
			endScope()
			return nil, invalidHomeConcurrencyResponse("Home returned both accounted concurrency and an error")
		}
		pending.End()
		return nil, errHome
	}

	var dispatch homeAuthDispatchResponse
	if errUnmarshal := json.Unmarshal(raw, &dispatch); errUnmarshal != nil {
		endScope()
		return nil, &Error{
			Code: "invalid_auth", Message: "home returned invalid auth payload", HTTPStatus: http.StatusBadGateway,
		}
	}
	auth := dispatch.Auth
	if strings.TrimSpace(auth.ID) == "" {
		if errUnmarshal := json.Unmarshal(raw, &auth); errUnmarshal != nil {
			endScope()
			return nil, &Error{
				Code: "invalid_auth", Message: "home returned invalid auth payload", HTTPStatus: http.StatusBadGateway,
			}
		}
	}
	observedModel := canonicalHomeDispatchModel(dispatch.Model, requestedModel)
	if concurrencyPresent {
		observedConcurrencyModel, validModel := validCanonicalHomeConcurrencyModelKey(observedModel)
		if !validModel || concurrencyTuple.Model != observedConcurrencyModel {
			abortAmbiguousHomeDispatch(client)
			endScope()
			return nil, invalidHomeConcurrencyResponse("Home concurrency model does not match dispatched model")
		}
	}
	if !concurrencyPresent {
		baseScope.Model = observedModel
	}

	setHomeUserAPIKeyOnGinContext(ctx, dispatch.UserAPIKey)
	if upstreamModel := strings.TrimSpace(dispatch.Model); upstreamModel != "" {
		if auth.Attributes == nil {
			auth.Attributes = make(map[string]string, 3)
		}
		auth.Attributes[homeUpstreamModelAttributeKey] = upstreamModel
	}
	if originalAlias := strings.TrimSpace(dispatch.OriginalAlias); dispatch.ForceMapping && originalAlias != "" {
		if auth.Attributes == nil {
			auth.Attributes = make(map[string]string, 2)
		}
		auth.Attributes[homeForceMappingAttributeKey] = "true"
		auth.Attributes[homeOriginalAliasAttributeKey] = originalAlias
	}
	if strings.TrimSpace(auth.ID) == "" {
		endScope()
		return nil, &Error{
			Code: "invalid_auth", Message: "home returned auth without id", HTTPStatus: http.StatusBadGateway,
		}
	}
	if errIdentity := verifyAccountedHomeConcurrencyIdentity(
		concurrencyTuple, &auth, dispatch.AuthIndex,
	); errIdentity != nil {
		endScope()
		return nil, errIdentity
	}
	logicalProvider := strings.ToLower(strings.TrimSpace(auth.Provider))
	executorKey := executorKeyFromAuth(&auth)
	if logicalProvider == "" || executorKey == "" {
		endScope()
		return nil, &Error{
			Code: "invalid_auth", Message: "home returned auth without provider", HTTPStatus: http.StatusBadGateway,
		}
	}
	if homeAuthIndex := strings.TrimSpace(dispatch.AuthIndex); homeAuthIndex != "" {
		auth.Index = homeAuthIndex
		auth.indexAssigned = true
	} else {
		auth.EnsureIndex()
	}
	executor, okExecutor := m.Executor(executorKey)
	if !okExecutor && auth.Attributes != nil && strings.TrimSpace(auth.Attributes["base_url"]) != "" {
		executorKey = "openai-compatibility"
		executor, okExecutor = m.Executor(executorKey)
	}
	if !okExecutor {
		endScope()
		return nil, &Error{
			Code: "executor_not_found", Message: "executor not registered", HTTPStatus: http.StatusBadGateway,
		}
	}
	if scope == nil {
		var errInstall error
		scope, errInstall = installHomeConcurrencyScope(
			registry, pending, homeConcurrencyTuple{}, executionregistry.ScopeSpec{
				RequestID:    baseScope.RequestID,
				CredentialID: strings.TrimSpace(auth.ID),
				Model:        baseScope.Model,
				Kind:         baseScope.Kind,
				StartedAt:    baseScope.StartedAt,
			},
		)
		if errInstall != nil {
			abortAmbiguousHomeDispatch(client)
			pending.End()
			return nil, homeConcurrencyInstallError(errInstall)
		}
	}
	authCopy := auth.Clone()
	authCopy.executorKey = executorKey
	selection, errSelection := newHomeDispatchSelection(authCopy, executor, logicalProvider, scope)
	if errSelection != nil {
		endScope()
		return nil, &Error{
			Code: "home_unavailable", Message: "home execution registry unavailable", Retryable: true,
			HTTPStatus: http.StatusServiceUnavailable,
		}
	}
	if concurrencyPresent {
		selection.accountedModel = concurrencyTuple.Model
	}
	return selection, nil
}
