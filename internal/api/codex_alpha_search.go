package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"maps"
	"net/http"
	"strings"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/clienterror"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/logging"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/runtime/executor"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/util"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/api/handlers"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
	coreexecutor "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
	cliproxysession "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/session"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/pluginapi"
	"github.com/gin-gonic/gin"
	log "github.com/sirupsen/logrus"
)

const (
	codexAlphaSearchSourceFormat  = "codex-alpha-search"
	codexAlphaSearchOAuthURL      = "https://chatgpt.com/backend-api/codex/alpha/search"
	codexAlphaSearchRequestLimit  = 16 << 20
	codexAlphaSearchResponseLimit = 32 << 20
)

type codexAlphaSearchRouting struct {
	ID    string `json:"id"`
	Model string `json:"model"`
}

func sanitizeCodexAlphaSearchBody(body []byte) []byte {
	var payload map[string]json.RawMessage
	if errUnmarshal := json.Unmarshal(body, &payload); errUnmarshal != nil || payload == nil {
		return body
	}

	removed := false
	for _, field := range []string{"prompt_cache_key", "prompt_cache_retention"} {
		if _, exists := payload[field]; exists {
			delete(payload, field)
			removed = true
		}
	}
	if !removed {
		return body
	}

	sanitizedBody, errMarshal := json.Marshal(payload)
	if errMarshal != nil {
		return body
	}
	return sanitizedBody
}

func rewriteCodexAlphaSearchModel(body []byte, upstreamModel string) []byte {
	upstreamModel = strings.TrimSpace(upstreamModel)
	if upstreamModel == "" {
		return body
	}

	var payload map[string]json.RawMessage
	if errUnmarshal := json.Unmarshal(body, &payload); errUnmarshal != nil || payload == nil {
		return body
	}
	if _, exists := payload["model"]; !exists {
		return body
	}

	modelJSON, errMarshalModel := json.Marshal(upstreamModel)
	if errMarshalModel != nil || string(payload["model"]) == string(modelJSON) {
		return body
	}
	payload["model"] = modelJSON
	rewrittenBody, errMarshal := json.Marshal(payload)
	if errMarshal != nil {
		return body
	}
	return rewrittenBody
}

func restoreCodexAlphaSearchModel(body, originalBody []byte) []byte {
	var payload map[string]json.RawMessage
	if errUnmarshal := json.Unmarshal(body, &payload); errUnmarshal != nil || payload == nil {
		return body
	}
	var originalPayload map[string]json.RawMessage
	if errUnmarshal := json.Unmarshal(originalBody, &originalPayload); errUnmarshal != nil ||
		originalPayload == nil {
		return body
	}
	originalModel, hadOriginalModel := originalPayload["model"]
	currentModel, hasCurrentModel := payload["model"]
	if hadOriginalModel {
		if hasCurrentModel && string(currentModel) == string(originalModel) {
			return body
		}
		payload["model"] = originalModel
	} else {
		if !hasCurrentModel {
			return body
		}
		delete(payload, "model")
	}
	if maps.EqualFunc(
		payload, originalPayload, func(left, right json.RawMessage) bool {
			return string(left) == string(right)
		},
	) {
		return originalBody
	}
	restoredBody, errMarshal := json.Marshal(payload)
	if errMarshal != nil {
		return body
	}
	return restoredBody
}

func codexAlphaSearchStatus(err error, fallback int) int {
	return clienterror.HTTPStatusFromErrorOr(err, fallback)
}

func codexAlphaSearchTerminationError(
	response pluginapi.RequestInterceptResponse,
) *coreexecutor.RequestTerminatedError {
	if !response.Terminate {
		return nil
	}
	status := response.StatusCode
	if status < http.StatusOK || status > 599 {
		status = http.StatusForbidden
	}
	return &coreexecutor.RequestTerminatedError{
		HTTPStatus: status,
		Header:     response.ResponseHeaders.Clone(),
		Body:       append([]byte(nil), response.ResponseBody...),
	}
}

func codexAlphaSearchErrorResponse(err error) *handlers.ErrorMessage {
	response := &handlers.ErrorMessage{
		StatusCode: codexAlphaSearchStatus(err, http.StatusBadGateway),
		Error:      err,
	}
	if terminated, ok := errors.AsType[*coreexecutor.RequestTerminatedError](err); ok {
		response.DirectResponse = true
		response.Body = terminated.ResponseBody()
		response.Headers = terminated.ResponseHeaders()
	}
	return response
}

type codexAlphaSearchRequestRouter interface {
	HasRequestInterceptors() bool
	InterceptRequestBeforeAuth(
		context.Context,
		pluginapi.RequestInterceptRequest,
	) pluginapi.RequestInterceptResponse
	InterceptRequestAfterAuth(
		context.Context,
		pluginapi.RequestInterceptRequest,
	) pluginapi.RequestInterceptResponse
}

func routeCodexAlphaSearchRequest(
	router codexAlphaSearchRequestRouter,
	ctx context.Context,
	c *gin.Context,
	body []byte,
	routing codexAlphaSearchRouting,
) ([]byte, http.Header, codexAlphaSearchRouting, *coreexecutor.RequestTerminatedError) {
	headers := c.Request.Header.Clone()
	if router == nil || !router.HasRequestInterceptors() {
		return body, headers, routing, nil
	}
	metadata := map[string]any{
		coreexecutor.RequestedModelMetadataKey: strings.TrimSpace(routing.Model),
	}
	if c.Request.URL != nil {
		metadata[coreexecutor.RequestPathMetadataKey] = strings.TrimSpace(c.Request.URL.Path)
	}
	response := router.InterceptRequestBeforeAuth(
		ctx, pluginapi.RequestInterceptRequest{
			SourceFormat:   codexAlphaSearchSourceFormat,
			Model:          strings.TrimSpace(routing.Model),
			RequestedModel: strings.TrimSpace(routing.Model),
			Headers:        headers,
			Body:           body,
			Metadata:       metadata,
		},
	)
	if errTerminate := codexAlphaSearchTerminationError(response); errTerminate != nil {
		return body, headers, routing, errTerminate
	}
	if response.Headers != nil {
		headers = response.Headers.Clone()
	}
	if len(response.Body) > 0 {
		body = response.Body
		var routed codexAlphaSearchRouting
		if errUnmarshal := json.Unmarshal(body, &routed); errUnmarshal == nil {
			if strings.TrimSpace(routed.ID) == "" {
				routed.ID = routing.ID
			}
			if strings.TrimSpace(routed.Model) == "" {
				routed.Model = routing.Model
			}
			routing = routed
		}
	}
	return body, headers, routing, nil
}

func codexAlphaSearchHeaderValuesEqual(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}

func codexAlphaSearchUpstreamHeaders(original, routed http.Header) http.Header {
	headers := make(http.Header)
	headers.Set("Content-Type", "application/json")
	headers.Set("Accept", "application/json")
	headers.Set("Originator", "codex_cli_rs")
	for _, name := range []string{"Version", "User-Agent", "Session_id", "X-Client-Request-Id"} {
		for _, value := range routed.Values(name) {
			headers.Add(name, value)
		}
	}
	for name, values := range routed {
		if codexAlphaSearchHeaderValuesEqual(original.Values(name), values) {
			continue
		}
		headers.Del(name)
		for _, value := range values {
			headers.Add(name, value)
		}
	}
	for name := range original {
		if len(routed.Values(name)) == 0 {
			headers.Del(name)
		}
	}
	for _, name := range []string{"Authorization", "Content-Encoding", "Content-Length", "Host", "X-Session-ID"} {
		headers.Del(name)
	}
	return headers
}

func interceptCodexAlphaSearchAfterAuth(
	router codexAlphaSearchRequestRouter,
	ctx context.Context,
	request pluginapi.RequestInterceptRequest,
) ([]byte, http.Header, *coreexecutor.RequestTerminatedError) {
	if router == nil || !router.HasRequestInterceptors() {
		return request.Body, request.Headers, nil
	}
	response := router.InterceptRequestAfterAuth(ctx, request)
	if errTerminate := codexAlphaSearchTerminationError(response); errTerminate != nil {
		return request.Body, request.Headers, errTerminate
	}
	if response.Headers != nil {
		request.Headers = response.Headers.Clone()
	}
	if len(response.Body) > 0 {
		request.Body = response.Body
	}
	return request.Body, request.Headers, nil
}

func homeSelectionAttemptContext(
	ctx context.Context,
	selection *auth.HomeDispatchSelection,
) (context.Context, func(), error) {
	if selection == nil {
		return nil, func() {}, errors.New("home dispatch selection is nil")
	}
	return selection.AttemptContext(ctx)
}

// codexAlphaSearch forwards standalone Codex search requests without protocol translation.
func (s *Server) codexAlphaSearch(c *gin.Context) {
	var router codexAlphaSearchRequestRouter
	if s != nil {
		router = s.pluginHost
	}
	s.codexAlphaSearchWithRouter(c, router)
}

func (s *Server) codexAlphaSearchWithRouter(c *gin.Context, router codexAlphaSearchRequestRouter) {
	if s == nil || s.handlers == nil || s.handlers.AuthManager == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "Codex auth manager unavailable"})
		return
	}

	rawBody, errRead := io.ReadAll(io.LimitReader(c.Request.Body, codexAlphaSearchRequestLimit+1))
	if errRead != nil {
		c.JSON(
			codexAlphaSearchStatus(errRead, http.StatusBadRequest),
			gin.H{"error": "Failed to read search request"},
		)
		return
	}
	if len(rawBody) > codexAlphaSearchRequestLimit {
		c.JSON(http.StatusRequestEntityTooLarge, gin.H{"error": "Search request body too large"})
		return
	}
	body, errDecode := handlers.DecodeRequestBodyWithLimit(
		rawBody,
		c.GetHeader("Content-Encoding"),
		codexAlphaSearchRequestLimit,
	)
	if errDecode != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Failed to decode search request"})
		return
	}

	clientBody := append([]byte(nil), body...)
	var routing codexAlphaSearchRouting
	_ = json.Unmarshal(body, &routing)
	requestedModel := strings.TrimSpace(routing.Model)
	ctx := util.WithGinContext(c.Request.Context(), c)
	body, selectionHeaders, routing, errRoute := routeCodexAlphaSearchRequest(router, ctx, c, body, routing)
	if errRoute != nil {
		s.handlers.WriteErrorResponse(c, codexAlphaSearchErrorResponse(errRoute))
		return
	}
	if sessionID := cliproxysession.NormalizeExplicitID(routing.ID); sessionID != "" {
		selectionHeaders.Set("X-Session-ID", sessionID)
	}
	selectionModel := strings.TrimSpace(routing.Model)
	selectionOpts := coreexecutor.Options{
		Headers:         selectionHeaders,
		OriginalRequest: body,
		Metadata: map[string]any{
			coreexecutor.RequestedModelMetadataKey: selectionModel,
			coreexecutor.RequestPathMetadataKey:    c.Request.URL.Path,
		},
	}

	policyError := s.handlers.ExecuteHTTPWithAPIKeyPolicy(
		ctx, codexAlphaSearchSourceFormat, "codex", requestedModel, selectionModel,
		func(ctx context.Context, model string, dispatch func()) *handlers.ErrorMessage {
			if model != selectionModel {
				body = rewriteCodexAlphaSearchModel(body, model)
				clientBody = rewriteCodexAlphaSearchModel(clientBody, model)
			}
			selectionModel = model
			selectionOpts.Metadata[coreexecutor.RequestedModelMetadataKey] = model
			var selection *auth.HomeDispatchSelection
			var selected *auth.Auth
			var errSelection error
			if s.handlers.AuthManager.HomeEnabled() {
				selection, errSelection = s.handlers.AuthManager.SelectHomeAuthWithCredentialPolicy(
					ctx,
					"codex",
					selectionModel,
					auth.CredentialPolicyCodexAlphaSearchV1,
					selectionOpts,
				)
				if selection != nil {
					selected = selection.CloneAuth()
				}
			} else {
				selected, errSelection = s.handlers.AuthManager.SelectAuthWithCredentialPolicy(
					ctx,
					"codex",
					selectionModel,
					auth.CredentialPolicyCodexAlphaSearchV1,
					selectionOpts,
				)
			}
			if errSelection != nil {
				return &handlers.ErrorMessage{
					StatusCode: codexAlphaSearchStatus(errSelection, http.StatusServiceUnavailable),
					Error:      errSelection, Headers: auth.SafeResponseHeaders(errSelection),
				}
			}
			if selected == nil {
				if selection != nil {
					selection.End("missing_auth")
				}
				return &handlers.ErrorMessage{
					StatusCode: http.StatusServiceUnavailable, Error: errors.New("codex auth unavailable"),
				}
			}
			logging.SetGinCPATraceID(c, selected.EnsureIndex())

			if selection != nil {
				attemptCtx, releaseAttempt, errAttempt := homeSelectionAttemptContext(ctx, selection)
				if errAttempt != nil {
					selection.End("attempt_bind_failed")
					return &handlers.ErrorMessage{StatusCode: http.StatusServiceUnavailable, Error: errAttempt}
				}
				ctx = attemptCtx
				defer releaseAttempt()
			}

			upstreamRequestBody := sanitizeCodexAlphaSearchBody(body)
			baseHeaders := codexAlphaSearchUpstreamHeaders(c.Request.Header, selectionHeaders)

			errMissingBaseURL := errors.New("codex Alpha Search API key base URL unavailable")
			performRequest := func(current *auth.Auth) (*http.Response, error) {
				headers := baseHeaders.Clone()
				if accountID, ok := current.Metadata["account_id"].(string); ok &&
					strings.TrimSpace(accountID) != "" {
					headers.Set("Chatgpt-Account-Id", accountID)
				}
				upstreamURL := codexAlphaSearchOAuthURL
				upstreamModel := requestedModel
				requestBody := restoreCodexAlphaSearchModel(upstreamRequestBody, clientBody)
				if current.AuthKind() == auth.AuthKindAPIKey {
					baseURL := ""
					if current.Attributes != nil {
						baseURL = strings.TrimSpace(current.Attributes["base_url"])
					}
					if baseURL == "" {
						return nil, errMissingBaseURL
					}
					upstreamURL = strings.TrimRight(baseURL, "/") + "/alpha/search"
					upstreamModel = s.handlers.AuthManager.ResolveExecutionModel(current, selectionModel)
					requestBody = rewriteCodexAlphaSearchModel(upstreamRequestBody, upstreamModel)
				}
				requestBody, headers, errIntercept := interceptCodexAlphaSearchAfterAuth(
					router,
					ctx,
					pluginapi.RequestInterceptRequest{
						SourceFormat:   codexAlphaSearchSourceFormat,
						ToFormat:       codexAlphaSearchSourceFormat,
						Model:          upstreamModel,
						RequestedModel: requestedModel,
						Headers:        headers,
						Body:           requestBody,
						Metadata:       selectionOpts.Metadata,
					},
				)
				if errIntercept != nil {
					return nil, errIntercept
				}
				req, errRequest := s.handlers.AuthManager.NewHttpRequest(
					ctx,
					current,
					http.MethodPost,
					upstreamURL,
					requestBody,
					headers,
				)
				if errRequest != nil {
					return nil, errRequest
				}
				executor.RecordAPIRequest(
					ctx,
					s.configSnapshot(),
					upstreamURL,
					http.MethodPost,
					req.Header.Clone(),
					requestBody,
					"codex",
					current,
				)
				dispatch()
				return s.handlers.AuthManager.HttpRequest(ctx, current, req)
			}

			if errContext := ctx.Err(); errContext != nil {
				if selection != nil {
					selection.End("attempt_canceled")
				}
				return &handlers.ErrorMessage{
					StatusCode: codexAlphaSearchStatus(
						errContext,
						http.StatusRequestTimeout,
					), Error: errContext,
				}
			}
			response, errRequest := performRequest(selected)
			if errRequest != nil {
				if _, ok := errors.AsType[*coreexecutor.RequestTerminatedError](errRequest); ok {
					if selection != nil {
						selection.End("request_rejected")
					}
					return codexAlphaSearchErrorResponse(errRequest)
				}
				if selection != nil {
					if errors.Is(errRequest, errMissingBaseURL) {
						selection.End("missing_base_url")
					} else {
						selection.End("request_failed")
					}
				}
				executor.RecordAPIResponseError(ctx, s.configSnapshot(), errRequest)
				return codexAlphaSearchErrorResponse(errRequest)
			}

			closeResponseBody := func() error {
				errClose := response.Body.Close()
				if errClose != nil {
					log.WithError(errClose).Error("codex alpha search: close response body")
				}
				return errClose
			}
			if selection != nil {
				if errBind := selection.Bind(closeResponseBody); errBind != nil {
					if response.StatusCode == http.StatusUnauthorized {
						s.handlers.AuthManager.ReportHomeUnauthorized(ctx, selected, "codex", selectionModel)
					}
					selection.End("response_bind_failed")
					return &handlers.ErrorMessage{StatusCode: http.StatusServiceUnavailable, Error: errBind}
				}
				defer selection.End("response_closed")
			} else {
				defer func() { _ = closeResponseBody() }()
			}

			executor.RecordAPIResponseMetadata(
				ctx,
				s.configSnapshot(),
				response.StatusCode,
				response.Header.Clone(),
				response,
			)
			upstreamBody, errResponse := io.ReadAll(
				io.LimitReader(response.Body, codexAlphaSearchResponseLimit+1),
			)
			executor.AppendAPIResponseChunk(ctx, s.configSnapshot(), upstreamBody)
			if selection != nil && response.StatusCode == http.StatusUnauthorized {
				s.handlers.AuthManager.ReportHomeUnauthorized(
					ctx,
					selected,
					"codex",
					selectionModel,
					upstreamBody,
				)
			}
			if errResponse != nil {
				executor.RecordAPIResponseError(ctx, s.configSnapshot(), errResponse)
				return &handlers.ErrorMessage{
					StatusCode: codexAlphaSearchStatus(errResponse, http.StatusBadGateway),
					Error:      errors.New("failed to read Codex search response"),
				}
			}
			if len(upstreamBody) > codexAlphaSearchResponseLimit {
				errTooLarge := errors.New("codex search response body too large")
				executor.RecordAPIResponseError(ctx, s.configSnapshot(), errTooLarge)
				return &handlers.ErrorMessage{StatusCode: http.StatusBadGateway, Error: errTooLarge}
			}
			if selection != nil && response.StatusCode == http.StatusUnauthorized {
				log.WithField("status", response.StatusCode).Warnf(
					"codex alpha search upstream request failed: "+
						"%s", logging.SafeDiagnosticForLog(string(upstreamBody)),
				)
			}
			if contentType := response.Header.Get("Content-Type"); contentType != "" {
				c.Header("Content-Type", contentType)
			}
			c.Status(response.StatusCode)
			if _, errWrite := c.Writer.Write(upstreamBody); errWrite != nil {
				return &handlers.ErrorMessage{StatusCode: http.StatusBadGateway, Error: errWrite}
			}
			return nil
		},
	)
	if policyError != nil {
		if !c.Writer.Written() {
			s.handlers.WriteErrorResponse(c, policyError)
		} else {
			log.WithError(policyError.Error).Error("codex alpha search response delivery failed")
		}
	}
}
