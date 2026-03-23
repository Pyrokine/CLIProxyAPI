package amp

import (
	"bytes"
	"io"
	"net/http/httputil"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/thinking"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/util"
	log "github.com/sirupsen/logrus"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// routeType represents the type of routing decision made for an Amp request
type routeType string

const (
	// routeTypeLocalProvider indicates the request is handled by a local OAuth provider (free)
	routeTypeLocalProvider routeType = "LOCAL_PROVIDER"
	// routeTypeModelMapping indicates the request was remapped to another available model (free)
	routeTypeModelMapping routeType = "MODEL_MAPPING"
	// routeTypeAmpCredits indicates the request is forwarded to ampcode.com (uses Amp credits)
	routeTypeAmpCredits routeType = "AMP_CREDITS"
	// routeTypeNoProvider indicates no provider or fallback available
	routeTypeNoProvider routeType = "NO_PROVIDER"
)

// mappedModelContextKey is the Gin context key for passing mapped model names.
const mappedModelContextKey = "mapped_model"

// logAmpRouting logs the routing decision for an Amp request with structured fields
func logAmpRouting(routeType routeType, requestedModel, resolvedModel, provider, path string) {
	fields := log.Fields{
		"component":       "amp-routing",
		"route_type":      string(routeType),
		"requested_model": requestedModel,
		"path":            path,
		"timestamp":       time.Now().Format(time.RFC3339),
	}

	if resolvedModel != "" && resolvedModel != requestedModel {
		fields["resolved_model"] = resolvedModel
	}
	if provider != "" {
		fields["provider"] = provider
	}

	switch routeType {
	case routeTypeLocalProvider:
		fields["cost"] = "free"
		fields["source"] = "local_oauth"
		log.WithFields(fields).Debugf("amp using local provider for model: %s", requestedModel)

	case routeTypeModelMapping:
		fields["cost"] = "free"
		fields["source"] = "local_oauth"
		fields["mapping"] = requestedModel + " -> " + resolvedModel
		// model mapping already logged in mapper; avoid duplicate here

	case routeTypeAmpCredits:
		fields["cost"] = "amp_credits"
		fields["source"] = "ampcode.com"
		fields["model_id"] = requestedModel // Explicit model_id for easy config reference
		log.WithFields(fields).Warnf(
			"forwarding to ampcode.com (uses amp credits) - model_id: %s | To use local provider, add to config: ampcode.model-mappings: [{from: \"%s\", to: \"<your-local-model>\"}]",
			requestedModel, requestedModel,
		)

	case routeTypeNoProvider:
		fields["cost"] = "none"
		fields["source"] = "error"
		fields["model_id"] = requestedModel // Explicit model_id for easy config reference
		log.WithFields(fields).Warnf("no provider available for model_id: %s", requestedModel)
	}
}

// fallbackHandler wraps a standard handler with fallback logic to ampcode.com
// when the model's provider is not available in CLIProxyAPI
type fallbackHandler struct {
	getProxy           func() *httputil.ReverseProxy
	modelMapper        modelMapper
	forceModelMappings func() bool
}

// newFallbackHandlerWithMapper creates a new fallback handler with model mapping support
func newFallbackHandlerWithMapper(
	getProxy func() *httputil.ReverseProxy,
	mapper modelMapper,
	forceModelMappings func() bool,
) *fallbackHandler {
	if forceModelMappings == nil {
		forceModelMappings = func() bool { return false }
	}
	return &fallbackHandler{
		getProxy:           getProxy,
		modelMapper:        mapper,
		forceModelMappings: forceModelMappings,
	}
}

// wrapHandler wraps a gin.HandlerFunc with fallback logic
// If the model's provider is not configured in CLIProxyAPI, it forwards to ampcode.com
func (fh *fallbackHandler) wrapHandler(handler gin.HandlerFunc) gin.HandlerFunc {
	return func(c *gin.Context) {
		requestPath := c.Request.URL.Path

		// Read the request body to extract the model name
		bodyBytes, err := io.ReadAll(c.Request.Body)
		if err != nil {
			log.Errorf("amp fallback: failed to read request body: %v", err)
			handler(c)
			return
		}

		// Restore the body for the handler to read
		c.Request.Body = io.NopCloser(bytes.NewReader(bodyBytes))

		// Try to extract model from request body or URL path (for Gemini)
		modelName := extractModelFromRequest(bodyBytes, c)
		if modelName == "" {
			// Can't determine model, proceed with normal handler
			handler(c)
			return
		}

		// Normalize model (handles dynamic thinking suffixes)
		suffixResult := thinking.ParseSuffix(modelName)
		normalizedModel := suffixResult.ModelName
		thinkingSuffix := ""
		if suffixResult.HasSuffix {
			thinkingSuffix = "(" + suffixResult.RawSuffix + ")"
		}

		resolveMappedModel := func() (string, []string) {
			if fh.modelMapper == nil {
				return "", nil
			}

			mappedModel := fh.modelMapper.MapModel(modelName)
			if mappedModel == "" {
				mappedModel = fh.modelMapper.MapModel(normalizedModel)
			}
			mappedModel = strings.TrimSpace(mappedModel)
			if mappedModel == "" {
				return "", nil
			}

			// Preserve dynamic thinking suffix (e.g. "(xhigh)") when mapping applies, unless the target
			// already specifies its own thinking suffix.
			if thinkingSuffix != "" {
				mappedSuffixResult := thinking.ParseSuffix(mappedModel)
				if !mappedSuffixResult.HasSuffix {
					mappedModel += thinkingSuffix
				}
			}

			mappedBaseModel := thinking.ParseSuffix(mappedModel).ModelName
			mappedProviders := util.GetProviderName(mappedBaseModel)
			if len(mappedProviders) == 0 {
				return "", nil
			}

			return mappedModel, mappedProviders
		}

		// Track resolved model for logging (may change if mapping is applied)
		resolvedModel := normalizedModel
		usedMapping := false
		var providers []string

		// Check if model mappings should be forced ahead of local API keys
		forceMappings := fh.forceModelMappings != nil && fh.forceModelMappings()

		if forceMappings {
			// FORCE MODE: Check model mappings FIRST (takes precedence over local API keys)
			// This allows users to route Amp requests to their preferred OAuth providers
			if mappedModel, mappedProviders := resolveMappedModel(); mappedModel != "" {
				// Mapping found and provider available - rewrite the model in request body
				bodyBytes = rewriteModelInRequest(bodyBytes, mappedModel)
				c.Request.Body = io.NopCloser(bytes.NewReader(bodyBytes))
				// Store mapped model in context for handlers that check it (like gemini bridge)
				c.Set(mappedModelContextKey, mappedModel)
				resolvedModel = mappedModel
				usedMapping = true
				providers = mappedProviders
			}

			// If no mapping applied, check for local providers
			if !usedMapping {
				providers = util.GetProviderName(normalizedModel)
			}
		} else {
			// DEFAULT MODE: Check local providers first, then mappings as fallback
			providers = util.GetProviderName(normalizedModel)

			if len(providers) == 0 {
				// No providers configured - check if we have a model mapping
				if mappedModel, mappedProviders := resolveMappedModel(); mappedModel != "" {
					// Mapping found and provider available - rewrite the model in request body
					bodyBytes = rewriteModelInRequest(bodyBytes, mappedModel)
					c.Request.Body = io.NopCloser(bytes.NewReader(bodyBytes))
					// Store mapped model in context for handlers that check it (like gemini bridge)
					c.Set(mappedModelContextKey, mappedModel)
					resolvedModel = mappedModel
					usedMapping = true
					providers = mappedProviders
				}
			}
		}

		// If no providers available, fallback to ampcode.com
		if len(providers) == 0 {
			proxy := fh.getProxy()
			if proxy != nil {
				// Log: Forwarding to ampcode.com (uses Amp credits)
				logAmpRouting(routeTypeAmpCredits, modelName, "", "", requestPath)

				// Restore body again for the proxy
				c.Request.Body = io.NopCloser(bytes.NewReader(bodyBytes))

				// Forward to ampcode.com
				proxy.ServeHTTP(c.Writer, c.Request)
				return
			}

			// No proxy available, let the normal handler return the error
			logAmpRouting(routeTypeNoProvider, modelName, "", "", requestPath)
		}

		// Log the routing decision
		providerName := ""
		if len(providers) > 0 {
			providerName = providers[0]
		}

		if usedMapping {
			// Log: Model was mapped to another model
			log.Debugf("amp model mapping: request %s -> %s", normalizedModel, resolvedModel)
			logAmpRouting(routeTypeModelMapping, modelName, resolvedModel, providerName, requestPath)
			rewriter := newResponseRewriter(c.Writer, modelName)
			c.Writer = rewriter
			// Filter Anthropic-Beta header only for local handling paths
			filterAntropicBetaHeader(c)
			c.Request.Body = io.NopCloser(bytes.NewReader(bodyBytes))
			handler(c)
			rewriter.Flush()
			log.Debugf("amp model mapping: response %s -> %s", resolvedModel, modelName)
		} else if len(providers) > 0 {
			// Log: Using local provider (free)
			logAmpRouting(routeTypeLocalProvider, modelName, resolvedModel, providerName, requestPath)
			// Filter Anthropic-Beta header only for local handling paths
			filterAntropicBetaHeader(c)
			c.Request.Body = io.NopCloser(bytes.NewReader(bodyBytes))
			handler(c)
		} else {
			// No provider, no mapping, no proxy: fall back to the wrapped handler so it can return an error response
			c.Request.Body = io.NopCloser(bytes.NewReader(bodyBytes))
			handler(c)
		}
	}
}

// filterAntropicBetaHeader filters Anthropic-Beta header to remove features requiring special subscription
// This is needed when using local providers (bypassing the Amp proxy)
func filterAntropicBetaHeader(c *gin.Context) {
	if betaHeader := c.Request.Header.Get("Anthropic-Beta"); betaHeader != "" {
		if filtered := filterBetaFeatures(betaHeader, "context-1m-2025-08-07"); filtered != "" {
			c.Request.Header.Set("Anthropic-Beta", filtered)
		} else {
			c.Request.Header.Del("Anthropic-Beta")
		}
	}
}

// rewriteModelInRequest replaces the model name in a JSON request body
func rewriteModelInRequest(body []byte, newModel string) []byte {
	if !gjson.GetBytes(body, "model").Exists() {
		return body
	}
	result, err := sjson.SetBytes(body, "model", newModel)
	if err != nil {
		log.Warnf("amp model mapping: failed to rewrite model in request body: %v", err)
		return body
	}
	return result
}

// extractModelFromRequest attempts to extract the model name from various request formats
func extractModelFromRequest(body []byte, c *gin.Context) string {
	// First try to parse from JSON body (OpenAI, Claude, etc.)
	// Check common model field names
	if result := gjson.GetBytes(body, "model"); result.Exists() && result.Type == gjson.String {
		return result.String()
	}

	// For Gemini requests, model is in the URL path
	// Standard format: /models/{model}:generateContent -> :action parameter
	if action := c.Param("action"); action != "" {
		// Split by colon to get model name (e.g., "gemini-pro:generateContent" -> "gemini-pro")
		parts := strings.Split(action, ":")
		if len(parts) > 0 && parts[0] != "" {
			return parts[0]
		}
	}

	// AMP CLI format: /publishers/google/models/{model}:method -> *path parameter
	// Example: /publishers/google/models/gemini-3-pro-preview:streamGenerateContent
	if path := c.Param("path"); path != "" {
		// Look for /models/{model}:method pattern
		if _, after, ok := strings.Cut(path, "/models/"); ok {
			modelPart := after // Skip "/models/"
			// Split by colon to get model name
			if colonIdx := strings.Index(modelPart, ":"); colonIdx > 0 {
				return modelPart[:colonIdx]
			}
		}
	}

	return ""
}
