package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
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
	if errUnmarshal := json.Unmarshal(originalBody, &originalPayload); errUnmarshal != nil || originalPayload == nil {
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
	restoredBody, errMarshal := json.Marshal(payload)
	if errMarshal != nil {
		return body
	}
	return restoredBody
}

func codexAlphaSearchStatus(err error, fallback int) int {
	return clienterror.HTTPStatusFromErrorOr(err, fallback)
}

type codexAlphaSearchRequestRouter interface {
	HasRequestInterceptors() bool
	InterceptRequestBeforeAuth(context.Context, pluginapi.RequestInterceptRequest) pluginapi.RequestInterceptResponse
	InterceptRequestAfterAuth(context.Context, pluginapi.RequestInterceptRequest) pluginapi.RequestInterceptResponse
}

func routeCodexAlphaSearchRequest(
	router codexAlphaSearchRequestRouter,
	ctx context.Context,
	c *gin.Context,
	body []byte,
	routing codexAlphaSearchRouting,
) ([]byte, http.Header, codexAlphaSearchRouting) {
	headers := c.Request.Header.Clone()
	if router == nil || !router.HasRequestInterceptors() {
		return body, headers, routing
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
	return body, headers, routing
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
) ([]byte, http.Header) {
	if router == nil || !router.HasRequestInterceptors() {
		return request.Body, request.Headers
	}
	response := router.InterceptRequestAfterAuth(ctx, request)
	if response.Headers != nil {
		request.Headers = response.Headers.Clone()
	}
	if len(response.Body) > 0 {
		request.Body = response.Body
	}
	return request.Body, request.Headers
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
		c.JSON(codexAlphaSearchStatus(errRead, http.StatusBadRequest), gin.H{"error": "Failed to read search request"})
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
	body, selectionHeaders, routing := routeCodexAlphaSearchRequest(router, ctx, c, body, routing)
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
		for _, value := range auth.SafeResponseHeaders(errSelection).Values("Retry-After") {
			c.Writer.Header().Add("Retry-After", value)
		}
		c.JSON(
			codexAlphaSearchStatus(errSelection, http.StatusServiceUnavailable), gin.H{"error": errSelection.Error()},
		)
		return
	}
	if selected == nil {
		if selection != nil {
			selection.End("missing_auth")
		}
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "Codex auth unavailable"})
		return
	}
	logging.SetGinCPATraceID(c, selected.EnsureIndex())

	if selection != nil {
		attemptCtx, releaseAttempt, errAttempt := homeSelectionAttemptContext(ctx, selection)
		if errAttempt != nil {
			selection.End("attempt_bind_failed")
			c.JSON(http.StatusServiceUnavailable, gin.H{"error": errAttempt.Error()})
			return
		}
		ctx = attemptCtx
		defer releaseAttempt()
	}

	upstreamRequestBody := sanitizeCodexAlphaSearchBody(body)
	baseHeaders := codexAlphaSearchUpstreamHeaders(c.Request.Header, selectionHeaders)

	errMissingBaseURL := errors.New("codex Alpha Search API key base URL unavailable")
	performRequest := func(current *auth.Auth) (*http.Response, error) {
		headers := baseHeaders.Clone()
		if accountID, ok := current.Metadata["account_id"].(string); ok && strings.TrimSpace(accountID) != "" {
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
		requestBody, headers = interceptCodexAlphaSearchAfterAuth(
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
		return s.handlers.AuthManager.HttpRequest(ctx, current, req)
	}

	if errContext := ctx.Err(); errContext != nil {
		if selection != nil {
			selection.End("attempt_canceled")
		}
		c.JSON(codexAlphaSearchStatus(errContext, http.StatusRequestTimeout), gin.H{"error": errContext.Error()})
		return
	}
	response, errRequest := performRequest(selected)
	if errRequest != nil {
		if selection != nil {
			if errors.Is(errRequest, errMissingBaseURL) {
				selection.End("missing_base_url")
			} else {
				selection.End("request_failed")
			}
		}
		executor.RecordAPIResponseError(ctx, s.configSnapshot(), errRequest)
		c.JSON(codexAlphaSearchStatus(errRequest, http.StatusBadGateway), gin.H{"error": errRequest.Error()})
		return
	}

	if selection != nil && response.StatusCode == http.StatusUnauthorized {
		s.handlers.AuthManager.ReportHomeUnauthorized(ctx, selected, "codex", selectionModel)
		executor.RecordAPIResponseMetadata(ctx, s.configSnapshot(), response.StatusCode, response.Header.Clone())
		_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 1<<20))
		if errClose := response.Body.Close(); errClose != nil {
			log.WithError(errClose).Error("codex alpha search: close unauthorized response body")
		}
		refreshed, didRefresh, errRefresh := s.handlers.AuthManager.RefreshHomeSelectionAfterUnauthorized(
			ctx,
			selection,
			selected,
		)
		if errRefresh != nil {
			selection.End("refresh_failed")
			c.JSON(
				codexAlphaSearchStatus(errRefresh, http.StatusServiceUnavailable), gin.H{"error": errRefresh.Error()},
			)
			return
		}
		if !didRefresh || refreshed == nil {
			selection.End("refresh_unavailable")
			c.JSON(http.StatusUnauthorized, gin.H{"error": "Codex credential unauthorized"})
			return
		}
		selected = refreshed
		logging.SetGinCPATraceID(c, selected.EnsureIndex())
		response, errRequest = performRequest(selected)
		if errRequest != nil {
			selection.End("retry_failed")
			executor.RecordAPIResponseError(ctx, s.configSnapshot(), errRequest)
			c.JSON(codexAlphaSearchStatus(errRequest, http.StatusBadGateway), gin.H{"error": errRequest.Error()})
			return
		}
		if response.StatusCode == http.StatusUnauthorized {
			s.handlers.AuthManager.ReportHomeUnauthorized(ctx, selected, "codex", selectionModel)
		}
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
			selection.End("response_bind_failed")
			c.JSON(http.StatusServiceUnavailable, gin.H{"error": errBind.Error()})
			return
		}
		defer selection.End("response_closed")
	} else {
		defer func() { _ = closeResponseBody() }()
	}

	executor.RecordAPIResponseMetadata(ctx, s.configSnapshot(), response.StatusCode, response.Header.Clone())
	upstreamBody, errResponse := io.ReadAll(io.LimitReader(response.Body, codexAlphaSearchResponseLimit+1))
	if errResponse != nil {
		executor.RecordAPIResponseError(ctx, s.configSnapshot(), errResponse)
		c.JSON(
			codexAlphaSearchStatus(errResponse, http.StatusBadGateway),
			gin.H{"error": "Failed to read Codex search response"},
		)
		return
	}
	if len(upstreamBody) > codexAlphaSearchResponseLimit {
		errTooLarge := errors.New("codex search response body too large")
		executor.RecordAPIResponseError(ctx, s.configSnapshot(), errTooLarge)
		c.JSON(http.StatusBadGateway, gin.H{"error": errTooLarge.Error()})
		return
	}
	executor.AppendAPIResponseChunk(ctx, s.configSnapshot(), upstreamBody)
	if contentType := response.Header.Get("Content-Type"); contentType != "" {
		c.Header("Content-Type", contentType)
	}
	c.Status(response.StatusCode)
	_, _ = c.Writer.Write(upstreamBody)
}
