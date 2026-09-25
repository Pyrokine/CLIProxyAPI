// Package pluginapi defines host-side plugin capability schemas and adapters.
package pluginapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"time"
)

// Plugin is the host-side representation produced from a dynamic plugin registration.
type Plugin struct {
	// Metadata identifies the plugin binary and its published source.
	Metadata Metadata
	// Capabilities declares the optional integration points implemented by the plugin.
	Capabilities Capabilities
	// SchemaVersion is the plugin contract version negotiated at registration.
	// Zero means unset and is treated as legacy schema 1 by the host.
	SchemaVersion uint32
}

// Metadata describes a plugin for registry, logging, and diagnostics.
type Metadata struct {
	// Name is the stable human-readable plugin name.
	Name string
	// Version is the plugin release version.
	Version string
	// Author identifies the plugin author or organization.
	Author string
	// GitHubRepository is the repository URL for plugin source and support.
	GitHubRepository string
	// Logo is a plugin-provided display asset reference for management clients.
	Logo string
	// ConfigFields describes plugin-owned configuration fields for management clients.
	ConfigFields []ConfigField
}

// ConfigFieldType classifies plugin-owned configuration values for management clients.
type ConfigFieldType string

// noinspection GoUnusedConst
const (
	// ConfigFieldTypeString describes a string configuration value.
	ConfigFieldTypeString ConfigFieldType = "string"
	// ConfigFieldTypeNumber describes a numeric configuration value.
	ConfigFieldTypeNumber ConfigFieldType = "number"
	// ConfigFieldTypeInteger describes an integer configuration value.
	ConfigFieldTypeInteger ConfigFieldType = "integer"
	// ConfigFieldTypeBoolean describes a boolean configuration value.
	ConfigFieldTypeBoolean ConfigFieldType = "boolean"
	// ConfigFieldTypeEnum describes a string value constrained to EnumValues.
	ConfigFieldTypeEnum ConfigFieldType = "enum"
	// ConfigFieldTypeArray describes an array configuration value.
	ConfigFieldTypeArray ConfigFieldType = "array"
	// ConfigFieldTypeObject describes an object configuration value.
	ConfigFieldTypeObject ConfigFieldType = "object"
)

// ConfigField describes a plugin-owned configuration field for management clients.
type ConfigField struct {
	// Name is the configuration key under plugins.configs.<pluginID>.
	Name string
	// Type classifies the field value for management clients.
	Type ConfigFieldType
	// EnumValues lists allowed values when Type is ConfigFieldTypeEnum.
	EnumValues []string
	// Description explains how the plugin uses the field.
	Description string
}

// Capabilities groups the optional host integration interfaces exposed by a plugin.
type Capabilities struct {
	// ModelRegistrar contributes development-time model metadata to the host registry.
	ModelRegistrar ModelRegistrar
	// ModelProvider contributes provider-native static and per-auth model metadata.
	ModelProvider ModelProvider
	// AuthProvider lets the host parse, login, poll, and refresh plugin provider auths.
	AuthProvider AuthProvider
	// FrontendAuthProvider authenticates frontend requests before proxy handling.
	FrontendAuthProvider FrontendAuthProvider
	// FrontendAuthProviderExclusive makes this frontend auth provider the only active request auth provider when selected.
	FrontendAuthProviderExclusive bool
	// Scheduler chooses an auth candidate before the built-in scheduler runs.
	Scheduler Scheduler
	// Executor sends requests to an upstream provider or local backend.
	Executor ProviderExecutor
	// ExecutorModelScope declares whether Executor serves static models, OAuth auth models, or both.
	// Empty defaults to ExecutorModelScopeBoth for backward compatibility.
	ExecutorModelScope ExecutorModelScope
	// ExecutorInputFormats lists request protocols accepted directly by Executor. Executors must declare at least one.
	ExecutorInputFormats []string
	// ExecutorOutputFormats lists response protocols emitted directly by Executor. Executors must declare at least one.
	ExecutorOutputFormats []string
	// RequestTranslator converts canonical requests into provider-specific payloads.
	RequestTranslator RequestTranslator
	// RequestNormalizer converts provider-specific requests into canonical payloads.
	RequestNormalizer RequestNormalizer
	// ResponseTranslator converts canonical responses into provider-specific payloads.
	ResponseTranslator ResponseTranslator
	// ResponseBeforeTranslator normalizes upstream responses before native translation.
	ResponseBeforeTranslator ResponseNormalizer
	// ResponseAfterTranslator normalizes translated responses before delivery.
	ResponseAfterTranslator ResponseNormalizer
	// RequestInterceptor rewrites execution requests before and after credential selection.
	RequestInterceptor RequestInterceptor
	// RequestLifecyclePlugin asynchronously receives one terminal event for each request that reached request interception.
	RequestLifecyclePlugin RequestLifecyclePlugin
	// ResponseInterceptor rewrites successful non-streaming HTTP execution responses before downstream delivery.
	ResponseInterceptor ResponseInterceptor
	// StreamChunkInterceptor rewrites successful HTTP stream chunks before downstream delivery.
	StreamChunkInterceptor StreamChunkInterceptor
	// WebSocketResponseObserver receives sanitized upstream WebSocket response events during execution.
	WebSocketResponseObserver WebSocketResponseObserver
	// ThinkingApplier applies validated thinking configuration to provider payloads.
	ThinkingApplier ThinkingApplier
	// UsagePlugin receives completed usage records.
	UsagePlugin UsagePlugin
	// CommandLinePlugin declares and handles plugin-owned command-line flags.
	CommandLinePlugin CommandLinePlugin
	// ManagementAPI declares plugin-owned diagnostic Management API and resource routes.
	ManagementAPI ManagementAPI
	// QuotaProvider surfaces normalized credential quota information.
	QuotaProvider QuotaProvider
}

// ExecutorModelScope declares which model-registration paths a plugin executor supports.
type ExecutorModelScope string

const (
	// ExecutorModelScopeBoth means the executor supports static and OAuth auth-bound models.
	ExecutorModelScopeBoth ExecutorModelScope = "both"
	// ExecutorModelScopeStatic means the executor supports only non-OAuth static models.
	ExecutorModelScopeStatic ExecutorModelScope = "static"
	// ExecutorModelScopeOAuth means the executor supports only OAuth auth-bound models.
	ExecutorModelScopeOAuth ExecutorModelScope = "oauth"
)

// ModelInfo describes a model contributed by a plugin.
type ModelInfo struct {
	// ID is the stable model identifier used in API requests.
	ID string
	// Object is the API object type, usually "model".
	Object string
	// Created is the Unix timestamp when the model metadata was created.
	Created int64
	// OwnedBy identifies the model owner or provider.
	OwnedBy string
	// Type classifies the model capability family.
	Type string
	// DisplayName is the user-facing model name.
	DisplayName string
	// Name is the provider-native model name.
	Name string
	// Version identifies the model revision when available.
	Version string
	// Description is a short user-facing model summary.
	Description string
	// InputTokenLimit is the maximum accepted input token count.
	InputTokenLimit int64
	// OutputTokenLimit is the maximum generated output token count.
	OutputTokenLimit int64
	// SupportedGenerationMethods lists supported generation method names.
	SupportedGenerationMethods []string
	// ContextLength is the maximum combined context length.
	ContextLength int64
	// MaxCompletionTokens is the maximum completion token count.
	MaxCompletionTokens int64
	// SupportedParameters lists request parameters supported by the model.
	SupportedParameters []string
	// SupportedInputModalities lists accepted input modality names.
	SupportedInputModalities []string
	// SupportedOutputModalities lists produced output modality names.
	SupportedOutputModalities []string
	// Thinking describes optional reasoning controls for the model.
	Thinking *ThinkingSupport
	// UserDefined reports whether the model was provided by user configuration.
	UserDefined bool
}

// ThinkingSupport describes supported reasoning budget controls.
type ThinkingSupport struct {
	// Min is the minimum accepted reasoning budget.
	Min int
	// Max is the maximum accepted reasoning budget.
	Max int
	// ZeroAllowed reports whether disabling reasoning is supported.
	ZeroAllowed bool
	// DynamicAllowed reports whether automatic reasoning budget selection is supported.
	DynamicAllowed bool
	// Levels lists supported named reasoning levels.
	Levels []string
}

// HostConfigSummary describes host configuration relevant to plugin providers.
type HostConfigSummary struct {
	// AuthDir is the resolved directory containing provider auth material.
	AuthDir string
	// ProxyURL is the configured upstream proxy URL.
	ProxyURL string
	// ForceModelPrefix reports whether model aliases should keep provider prefixes.
	ForceModelPrefix bool
	// OAuthModelAlias maps providers to configured model aliases.
	OAuthModelAlias map[string][]ModelAlias
	// ExcludedModels maps providers to model names hidden by host configuration.
	ExcludedModels map[string][]string
}

// ModelAlias describes one configured provider model alias.
type ModelAlias struct {
	// Name is the provider model name.
	Name string
	// Alias is the host-facing model alias.
	Alias string
}

// AuthData describes a plugin provider auth record exchanged with the host.
type AuthData struct {
	// Provider is the provider key associated with the auth.
	Provider string
	// ID is the stable host auth identifier.
	ID string
	// FileName is the source or persisted auth file name.
	FileName string
	// Label is the user-facing auth label.
	Label string
	// Prefix is the configured model prefix for this auth.
	Prefix string
	// ProxyURL is the auth-specific proxy URL when configured.
	ProxyURL string
	// Disabled reports whether the auth should be skipped.
	Disabled bool
	// StorageJSON contains provider-owned persisted auth data.
	StorageJSON []byte
	// Metadata contains mutable host-managed auth metadata.
	Metadata map[string]any
	// Attributes contains immutable routing and provider attributes.
	Attributes map[string]string
	// NextRefreshAfter is the earliest time the host should refresh this auth.
	NextRefreshAfter time.Time
}

// AuthParseRequest describes auth material offered to a plugin parser.
type AuthParseRequest struct {
	// Provider is the provider key being parsed.
	Provider string
	// Path is the source path of the auth material when available.
	Path string
	// FileName is the auth file name.
	FileName string
	// RawJSON contains the raw auth file payload.
	RawJSON []byte
	// Host contains relevant host configuration.
	Host HostConfigSummary
}

// AuthParseResponse returns the parser decision and parsed auth data.
type AuthParseResponse struct {
	// Handled reports whether the plugin recognized the auth material.
	Handled bool
	// Auth is the parsed auth record when Handled is true.
	Auth AuthData
	// Auths contains multiple parsed records when one source expands into several runtime auths.
	Auths []AuthData
}

// AuthProvider parses, logs in, polls, and refreshes plugin provider auths.
type AuthProvider interface {
	Identifier() string
	ParseAuth(context.Context, AuthParseRequest) (AuthParseResponse, error)
	StartLogin(context.Context, AuthLoginStartRequest) (AuthLoginStartResponse, error)
	PollLogin(context.Context, AuthLoginPollRequest) (AuthLoginPollResponse, error)
	RefreshAuth(context.Context, AuthRefreshRequest) (AuthRefreshResponse, error)
}

// AuthLoginStartRequest asks a plugin to start a provider login flow.
type AuthLoginStartRequest struct {
	// Provider is the provider key for the login flow.
	Provider string
	// BaseURL is the host callback or login base URL.
	BaseURL string
	// Host contains relevant host configuration.
	Host HostConfigSummary
	// HTTPClient executes upstream HTTP requests through host transport policy.
	HTTPClient HostHTTPClient `json:"-"`
	// Metadata carries plugin-defined login context.
	Metadata map[string]any
}

// AuthLoginStartResponse returns login flow state for polling.
type AuthLoginStartResponse struct {
	// Provider is the provider key for the login flow.
	Provider string
	// URL is the user-facing login URL.
	URL string
	// State is the opaque plugin login state used for polling.
	State string
	// ExpiresAt is the time when this login flow expires.
	ExpiresAt time.Time
	// Metadata carries plugin-defined polling context.
	Metadata map[string]any
}

// AuthLoginPollRequest asks a plugin to poll a provider login flow.
type AuthLoginPollRequest struct {
	// Provider is the provider key for the login flow.
	Provider string
	// State is the opaque plugin login state returned by StartLogin.
	State string
	// Host contains relevant host configuration.
	Host HostConfigSummary
	// HTTPClient executes upstream HTTP requests through host transport policy.
	HTTPClient HostHTTPClient `json:"-"`
	// Metadata carries plugin-defined polling context.
	Metadata map[string]any
}

// AuthLoginStatus describes the current provider login state.
type AuthLoginStatus string

// noinspection GoUnusedConst
const (
	// AuthLoginStatusPending means the login flow is still waiting.
	AuthLoginStatusPending AuthLoginStatus = "pending"
	// AuthLoginStatusSuccess means the login flow produced auth data.
	AuthLoginStatusSuccess AuthLoginStatus = "success"
	// AuthLoginStatusError means the login flow failed.
	AuthLoginStatusError AuthLoginStatus = "error"
)

// AuthLoginPollResponse returns the login poll status and auth data.
type AuthLoginPollResponse struct {
	// Status is the current login flow state.
	Status AuthLoginStatus
	// Message contains provider-facing login progress or error text.
	Message string
	// Auth is the completed auth record when Status is success.
	Auth AuthData
	// Auths contains multiple completed records when one login expands into several runtime auths.
	Auths []AuthData
}

// AuthRefreshRequest asks a plugin to refresh provider auth data.
type AuthRefreshRequest struct {
	// AuthID identifies the auth record to refresh.
	AuthID string
	// AuthProvider identifies the credential provider.
	AuthProvider string
	// StorageJSON contains provider-owned persisted auth data.
	StorageJSON []byte
	// Metadata contains mutable host-managed auth metadata.
	Metadata map[string]any
	// Attributes contains immutable routing and provider attributes.
	Attributes map[string]string
	// Host contains relevant host configuration.
	Host HostConfigSummary
	// HTTPClient executes upstream HTTP requests through host transport policy.
	HTTPClient HostHTTPClient `json:"-"`
}

// AuthRefreshResponse returns refreshed provider auth data.
type AuthRefreshResponse struct {
	// Auth is the refreshed auth record.
	Auth AuthData
	// NextRefreshAfter is the earliest time the host should refresh again.
	NextRefreshAfter time.Time
}

// ModelRegistrar registers plugin-provided models with the host.
type ModelRegistrar interface {
	RegisterModels(context.Context, ModelRegistrationRequest) (ModelRegistrationResponse, error)
}

// ModelRegistrationRequest carries host context for model registration.
type ModelRegistrationRequest struct {
	// Plugin is the metadata of the plugin being registered.
	Plugin Metadata
}

// ModelRegistrationResponse returns provider and model metadata to register.
type ModelRegistrationResponse struct {
	// Provider is the provider key associated with the returned models.
	Provider string
	// Models is the complete set of plugin-provided models.
	Models []ModelInfo
}

// ModelProvider contributes provider-native static and per-auth model metadata.
type ModelProvider interface {
	StaticModels(context.Context, StaticModelRequest) (ModelResponse, error)
	ModelsForAuth(context.Context, AuthModelRequest) (ModelResponse, error)
}

// StaticModelRequest carries host context for provider static models.
type StaticModelRequest struct {
	// Plugin is the metadata of the plugin being registered.
	Plugin Metadata
	// Host contains relevant host configuration.
	Host HostConfigSummary
}

// AuthModelRequest carries auth context for provider model discovery.
type AuthModelRequest struct {
	// Plugin is the metadata of the plugin being registered.
	Plugin Metadata
	// AuthID identifies the auth record used for discovery.
	AuthID string
	// AuthProvider identifies the credential provider.
	AuthProvider string
	// StorageJSON contains provider-owned persisted auth data.
	StorageJSON []byte
	// Metadata contains mutable host-managed auth metadata.
	Metadata map[string]any
	// Attributes contains immutable routing and provider attributes.
	Attributes map[string]string
	// Host contains relevant host configuration.
	Host HostConfigSummary
	// HTTPClient executes upstream HTTP requests through host transport policy.
	HTTPClient HostHTTPClient `json:"-"`
}

// ModelDiscoveryOutcome describes whether a plugin returned a complete or partial discovery result.
type ModelDiscoveryOutcome string

const (
	// ModelDiscoveryComplete means Models is the complete authoritative set, including an empty set.
	ModelDiscoveryComplete ModelDiscoveryOutcome = "complete"
	// ModelDiscoveryPartial means Models is usable but does not replace a prior complete result.
	ModelDiscoveryPartial ModelDiscoveryOutcome = "partial"
	// ModelDiscoveryInconclusive means the plugin could not determine the model set.
	ModelDiscoveryInconclusive ModelDiscoveryOutcome = "inconclusive"
)

// ModelResponse returns provider and model metadata discovered by a plugin.
type ModelResponse struct {
	// Provider is the provider key associated with the returned models.
	Provider string
	// Models contains the discovered provider models.
	Models []ModelInfo
	// Outcome defaults to ModelDiscoveryComplete for compatibility.
	Outcome ModelDiscoveryOutcome
	// AuthUpdate contains updated auth data from model discovery when needed.
	AuthUpdate AuthData
}

// FrontendAuthProvider authenticates frontend requests before proxy routing.
type FrontendAuthProvider interface {
	Identifier() string
	Authenticate(context.Context, FrontendAuthRequest) (FrontendAuthResponse, error)
}

// FrontendAuthRequest describes an inbound frontend authentication request.
type FrontendAuthRequest struct {
	// Method is the HTTP method.
	Method string
	// Path is the request path.
	Path string
	// Headers contains inbound request headers.
	Headers http.Header
	// Query contains inbound query parameters.
	Query url.Values
	// Body contains the raw request body.
	Body []byte
}

// FrontendAuthResponse reports the authentication decision and identity metadata.
type FrontendAuthResponse struct {
	// Authenticated reports whether the request was accepted.
	Authenticated bool
	// Principal is the authenticated subject identifier.
	Principal string
	// Metadata carries plugin-defined identity attributes for downstream use.
	Metadata map[string]string
}

const (
	// SchedulerBuiltinRoundRobin delegates auth selection to the built-in round-robin scheduler.
	SchedulerBuiltinRoundRobin = "round-robin"
	// SchedulerBuiltinFillFirst delegates auth selection to the built-in fill-first scheduler.
	SchedulerBuiltinFillFirst = "fill-first"
)

// Scheduler chooses an auth candidate before the built-in scheduler runs.
type Scheduler interface {
	Pick(context.Context, SchedulerPickRequest) (SchedulerPickResponse, error)
}

// SchedulerPickRequest describes the routing context offered to a scheduler plugin.
type SchedulerPickRequest struct {
	// Plugin is the metadata of the plugin being executed.
	Plugin Metadata
	// Provider is the primary provider key requested by the route.
	Provider string
	// Providers contains every provider key accepted by the route.
	Providers []string
	// Model is the requested model identifier.
	Model string
	// Stream reports whether the request expects streaming output.
	Stream bool
	// Options contains request-scoped scheduler inputs.
	Options SchedulerOptions
	// Candidates contains auth records available for selection.
	Candidates []SchedulerAuthCandidate
}

// SchedulerOptions carries request-scoped scheduler inputs.
type SchedulerOptions struct {
	// Headers contains request headers relevant to scheduling.
	Headers map[string][]string
	// Metadata carries host-provided scheduler context.
	Metadata map[string]any
}

// SchedulerAuthCandidate describes one auth candidate available to a scheduler.
type SchedulerAuthCandidate struct {
	// ID identifies the auth record.
	ID string
	// Provider identifies the auth provider.
	Provider string
	// Priority is the host priority assigned to the auth record.
	Priority int
	// Status is the current host-visible auth status.
	Status string
	// Attributes contains immutable routing and provider attributes.
	Attributes map[string]string
	// Metadata contains mutable host-managed auth metadata.
	Metadata map[string]any
}

// SchedulerPickResponse returns a scheduler plugin routing decision.
type SchedulerPickResponse struct {
	// AuthID identifies the selected auth record.
	AuthID string
	// DelegateBuiltin asks the host to use a named built-in scheduler.
	DelegateBuiltin string
	// Handled reports whether the plugin made a scheduling decision.
	Handled bool
}

// ProviderExecutor handles model execution, streaming, HTTP bridging, and token counting.
type ProviderExecutor interface {
	Identifier() string
	Execute(context.Context, ExecutorRequest) (ExecutorResponse, error)
	ExecuteStream(context.Context, ExecutorRequest) (ExecutorStreamResponse, error)
	CountTokens(context.Context, ExecutorRequest) (ExecutorResponse, error)
	HttpRequest(context.Context, ExecutorHTTPRequest) (ExecutorHTTPResponse, error)
}

// HostHTTPClient executes plugin HTTP requests through host transport policy.
// Plugin executors must use this client for upstream calls so request-log can
// capture the outbound request and raw upstream response when enabled.
type HostHTTPClient interface {
	Do(context.Context, HTTPRequest) (HTTPResponse, error)
	DoStream(context.Context, HTTPRequest) (HTTPStreamResponse, error)
}

// HostModelExecutionRequest describes a model execution request issued through the host.
type HostModelExecutionRequest struct {
	// EntryProtocol is the inbound client protocol format.
	EntryProtocol string `json:"entry_protocol"`
	// ExitProtocol is the target provider protocol format.
	ExitProtocol string `json:"exit_protocol"`
	// Model is the requested model identifier.
	Model string `json:"model"`
	// Stream reports whether the request expects streaming output.
	Stream bool `json:"stream"`
	// Body contains the raw request body.
	Body []byte `json:"body"`
	// Headers contains request headers.
	Headers http.Header `json:"headers"`
	// Query contains request query parameters.
	Query url.Values `json:"query"`
	// Alt carries an alternate route or mode suffix when present.
	Alt string `json:"alt"`
}

// HostModelExecutionResponse describes a non-streaming host model execution response.
type HostModelExecutionResponse struct {
	// StatusCode is the model execution HTTP status code.
	StatusCode int `json:"status_code"`
	// Headers contains response headers.
	Headers http.Header `json:"headers"`
	// Body contains the raw response body.
	Body []byte `json:"body"`
}

// HostModelStreamResponse describes a streaming host model execution response.
type HostModelStreamResponse struct {
	// StatusCode is the model execution HTTP status code.
	StatusCode int `json:"status_code"`
	// Headers contains response headers.
	Headers http.Header `json:"headers"`
	// StreamID identifies the host-owned stream for later reads.
	StreamID string `json:"stream_id"`
}

// HostModelStreamReadRequest asks the host to read the next model stream chunk.
type HostModelStreamReadRequest struct {
	// StreamID identifies the host-owned stream.
	StreamID string `json:"stream_id"`
}

// HostModelStreamReadResponse returns one model stream chunk or terminal state.
type HostModelStreamReadResponse struct {
	// Payload contains the raw stream chunk bytes.
	Payload []byte `json:"payload"`
	// Error reports a stream error associated with this read.
	Error string `json:"error"`
	// Done reports whether the stream has ended.
	Done bool `json:"done"`
}

// HostModelStreamCloseRequest asks the host to close a model stream.
type HostModelStreamCloseRequest struct {
	// StreamID identifies the host-owned stream.
	StreamID string `json:"stream_id"`
}

// Host affinity lookup status outcomes.
const (
	HostAffinityStatusBound       = "bound"
	HostAffinityStatusUnbound     = "unbound"
	HostAffinityStatusAmbiguous   = "ambiguous"
	HostAffinityStatusUnsupported = "unsupported"
)

// HostAffinityLookupRequest asks the host to observe the current session binding.
type HostAffinityLookupRequest struct {
	// Provider identifies the requested model provider.
	Provider string `json:"provider"`
	// Model identifies the requested model.
	Model string `json:"model"`
	// SessionID identifies the client session.
	SessionID string `json:"session_id"`
}

// HostAffinityLookupResponse describes a read-only affinity observation.
type HostAffinityLookupResponse struct {
	// Status is bound, unbound, ambiguous, or unsupported.
	Status string `json:"status"`
	// AuthIndex identifies the bound credential without exposing credential material.
	AuthIndex string `json:"auth_index,omitempty"`
	// ObservedAt is the time at which the binding was observed.
	ObservedAt time.Time `json:"observed_at,omitempty"`
	// Disabled reports whether the bound credential is disabled.
	Disabled bool `json:"disabled,omitempty"`
	// Unavailable reports whether the bound credential is unavailable.
	Unavailable bool `json:"unavailable,omitempty"`
}

// HTTPWireProfile configures transport-level wire representation for plugin HTTP requests.
type HTTPWireProfile struct {
	// HTTP1Only forces HTTP/1.1 and rejects any TLS negotiation of HTTP/2.
	HTTP1Only bool `json:"http1_only,omitempty"`
	// DisableAutoCompression prevents transparent decompression and automatic Accept-Encoding injection.
	DisableAutoCompression bool `json:"disable_auto_compression,omitempty"`
	// HeaderProfile defines header-name order and exact casing on the wire.
	HeaderProfile []string `json:"header_profile,omitempty"`
}

// HTTPRequest describes an upstream HTTP request issued through the host.
type HTTPRequest struct {
	// Method is the HTTP method.
	Method string
	// URL is the absolute upstream URL.
	URL string
	// Headers contains request headers.
	Headers http.Header
	// Body contains the raw request body.
	Body []byte
	// WireProfile specifies optional outbound HTTP wire settings.
	WireProfile *HTTPWireProfile `json:"wire_profile,omitempty"`
}

// HTTPResponse describes a non-streaming host HTTP response.
type HTTPResponse struct {
	// StatusCode is the upstream HTTP status code.
	StatusCode int
	// Headers contains upstream response headers.
	Headers http.Header
	// Body contains the raw response body.
	Body []byte
}

// HTTPStreamResponse describes a streaming host HTTP response.
type HTTPStreamResponse struct {
	// StatusCode is the upstream HTTP status code.
	StatusCode int
	// Headers contains upstream response headers.
	Headers http.Header
	// Chunks yields streaming payload chunks until the channel closes.
	Chunks <-chan HTTPStreamChunk
}

// HTTPStreamChunk carries one host HTTP stream chunk or an error.
type HTTPStreamChunk struct {
	// Payload contains the raw stream chunk bytes.
	Payload []byte
	// Err reports a stream error associated with this chunk.
	Err error
}

// ExecutorHTTPRequest describes an executor-owned HTTP request.
type ExecutorHTTPRequest struct {
	// AuthID identifies the selected credential.
	AuthID string
	// AuthProvider identifies the credential provider.
	AuthProvider string
	// Method is the HTTP method.
	Method string
	// URL is the absolute upstream URL.
	URL string
	// Headers contains request headers.
	Headers http.Header
	// Body contains the raw request body.
	Body []byte
	// StorageJSON contains provider-owned auth storage for this concrete auth.
	StorageJSON []byte
	// Metadata contains mutable host-managed auth metadata.
	Metadata map[string]any
	// Attributes contains immutable routing and provider attributes.
	Attributes map[string]string
	// HTTPClient executes upstream HTTP requests through host transport policy and request-log capture.
	HTTPClient HostHTTPClient `json:"-"`
}

// ExecutorHTTPResponse describes an executor-owned HTTP response.
type ExecutorHTTPResponse struct {
	// StatusCode is the upstream HTTP status code.
	StatusCode int
	// Headers contains upstream response headers.
	Headers http.Header
	// Body contains the raw response body.
	Body []byte
}

// ExecutorRequest describes a model execution or token counting call.
type ExecutorRequest struct {
	// AuthID identifies the selected credential.
	AuthID string
	// AuthProvider identifies the credential provider.
	AuthProvider string
	// Model is the requested model identifier.
	Model string
	// Format is the target request or response protocol format.
	Format string
	// Stream reports whether the request expects streaming output.
	Stream bool
	// Alt carries an alternate route or mode suffix when present.
	Alt string
	// Headers contains request headers passed to the executor.
	Headers http.Header
	// Query contains request query parameters passed to the executor.
	Query url.Values
	// OriginalRequest contains the raw client request body.
	OriginalRequest []byte
	// SourceFormat is the original client protocol format.
	SourceFormat string
	// Payload contains the translated provider payload.
	Payload []byte
	// Metadata is an extension bag for host and plugin coordination data.
	Metadata map[string]any
	// StorageJSON contains provider-owned auth storage for this concrete auth.
	StorageJSON []byte
	// AuthMetadata contains mutable host-managed auth metadata.
	AuthMetadata map[string]any
	// AuthAttributes contains immutable routing and provider attributes.
	AuthAttributes map[string]string
	// HTTPClient executes upstream HTTP requests through host transport policy and request-log capture.
	HTTPClient HostHTTPClient `json:"-"`
}

// ExecutorResponse returns a non-streaming executor result.
type ExecutorResponse struct {
	// Payload contains the raw response body.
	Payload []byte
	// Headers contains response headers to forward or inspect.
	Headers http.Header
	// Metadata is an extension bag for executor-specific response data.
	Metadata map[string]any
}

// ExecutorStreamResponse returns a streaming executor result.
type ExecutorStreamResponse struct {
	// Headers contains response headers available before stream chunks.
	Headers http.Header
	// Chunks yields streaming payload chunks until the channel closes.
	Chunks <-chan ExecutorStreamChunk
}

// ExecutorStreamChunk carries one streaming payload chunk or an error.
type ExecutorStreamChunk struct {
	// Payload contains the raw stream chunk bytes.
	Payload []byte
	// Err reports a stream error associated with this chunk.
	Err error
}

// RequestTranslator converts canonical request payloads to another format.
type RequestTranslator interface {
	TranslateRequest(context.Context, RequestTransformRequest) (PayloadResponse, error)
}

// RequestNormalizer converts request payloads into a canonical format.
type RequestNormalizer interface {
	NormalizeRequest(context.Context, RequestTransformRequest) (PayloadResponse, error)
}

// ResponseTranslator converts canonical response payloads to another format.
type ResponseTranslator interface {
	TranslateResponse(context.Context, ResponseTransformRequest) (PayloadResponse, error)
}

// ResponseNormalizer converts response payloads into a canonical format.
type ResponseNormalizer interface {
	NormalizeResponse(context.Context, ResponseTransformRequest) (PayloadResponse, error)
}

// RequestInterceptor rewrites execution requests before and after credential selection.
type RequestInterceptor interface {
	InterceptRequestBeforeAuth(context.Context, RequestInterceptRequest) (RequestInterceptResponse, error)
	InterceptRequestAfterAuth(context.Context, RequestInterceptRequest) (RequestInterceptResponse, error)
}

// RequestLifecyclePlugin receives asynchronous terminal events after execution finishes, fails, is rejected, or is canceled.
type RequestLifecyclePlugin interface {
	HandleRequestComplete(context.Context, RequestCompletion) error
}

// ResponseInterceptor rewrites successful non-streaming execution responses before downstream delivery.
type ResponseInterceptor interface {
	InterceptResponse(context.Context, ResponseInterceptRequest) (ResponseInterceptResponse, error)
}

// StreamChunkInterceptor rewrites successful stream chunks before downstream delivery.
type StreamChunkInterceptor interface {
	InterceptStreamChunk(context.Context, StreamChunkInterceptRequest) (StreamChunkInterceptResponse, error)
}

// WebSocketResponseObserver observes sanitized upstream WebSocket response events.
type WebSocketResponseObserver interface {
	ObserveWebSocketResponseEvent(context.Context, WebSocketResponseEvent) error
}

// WebSocketResponseCallback is a request-scoped raw WebSocket event callback.
// Hosts invalidate callbacks when the request ends; callers must not retain them.
type WebSocketResponseCallback func(WebSocketResponseEvent)

// StreamChunkHeaderInitIndex marks the header-only stream initialization interceptor call.
const StreamChunkHeaderInitIndex = -1

// RequestTransformRequest describes a request payload transformation.
type RequestTransformRequest struct {
	// FromFormat is the source protocol format.
	FromFormat string
	// ToFormat is the target protocol format.
	ToFormat string
	// Model is the requested model identifier.
	Model string
	// Stream reports whether the request expects streaming output.
	Stream bool
	// Body contains the payload to transform.
	Body []byte
}

// ResponseTransformRequest describes a response payload transformation.
type ResponseTransformRequest struct {
	// FromFormat is the source protocol format.
	FromFormat string
	// ToFormat is the target protocol format.
	ToFormat string
	// Model is the requested model identifier.
	Model string
	// Stream reports whether the response is streaming.
	Stream bool
	// OriginalRequest contains the raw client request body.
	OriginalRequest []byte
	// TranslatedRequest contains the provider request body.
	TranslatedRequest []byte
	// Body contains the response payload to transform.
	Body []byte
}

// RequestInterceptRequest describes a request about to be executed upstream.
type RequestInterceptRequest struct {
	// RequestID uniquely identifies one model execution and correlates it with RequestCompletion.
	RequestID string
	// TraceID identifies the parent inbound HTTP request when available.
	TraceID string
	// SourceFormat is the original client protocol format.
	SourceFormat string
	// ToFormat is the selected upstream protocol format. It is empty before credential selection.
	ToFormat string
	// Model is the current execution model. After credential selection this is the selected upstream model.
	Model string
	// RequestedModel is the client-requested model before alias/model-pool rewriting.
	RequestedModel string
	// Stream reports whether the request expects streaming output.
	Stream bool
	// Headers contains the current upstream request headers.
	Headers http.Header
	// Body contains the current request payload.
	Body []byte
	// Metadata is a best-effort cloned context snapshot. Treat it as read-only and JSON-like.
	Metadata map[string]any
}

// RequestInterceptResponse returns request modifications.
type RequestInterceptResponse struct {
	// Headers replaces matching current request headers and preserves headers not mentioned here.
	Headers http.Header
	// Body replaces the current request body only when non-empty.
	Body []byte
	// ClearHeaders explicitly removes current request headers before Headers is applied.
	ClearHeaders []string
	// Terminate stops the interceptor chain and prevents the request from reaching an upstream executor.
	Terminate bool
	// StatusCode is the downstream HTTP status used when Terminate is true. Invalid values default to 403.
	StatusCode int
	// ResponseHeaders contains downstream response headers used when Terminate is true.
	ResponseHeaders http.Header
	// ResponseBody contains the downstream response body used when Terminate is true.
	ResponseBody []byte
}

// RequestCompletionOutcome identifies how an intercepted request ended.
type RequestCompletionOutcome string

const (
	// RequestCompletionSucceeded means the request completed successfully.
	RequestCompletionSucceeded RequestCompletionOutcome = "succeeded"
	// RequestCompletionFailed means model execution failed.
	RequestCompletionFailed RequestCompletionOutcome = "failed"
	// RequestCompletionRejected means a request interceptor terminated the request before execution.
	RequestCompletionRejected RequestCompletionOutcome = "rejected"
	// RequestCompletionCanceled means the request context was canceled or the downstream client disconnected.
	RequestCompletionCanceled RequestCompletionOutcome = "canceled"
)

// RequestCompletion describes the terminal state of an intercepted request.
type RequestCompletion struct {
	RequestID      string
	TraceID        string
	SourceFormat   string
	Model          string
	RequestedModel string
	Stream         bool
	Outcome        RequestCompletionOutcome
	StatusCode     int
	Error          string
	StartedAt      time.Time
	CompletedAt    time.Time
	Metadata       map[string]any
}

// ResponseInterceptRequest describes a successful non-streaming response.
type ResponseInterceptRequest struct {
	RequestID       string
	SourceFormat    string
	Model           string
	RequestedModel  string
	Stream          bool
	RequestHeaders  http.Header
	ResponseHeaders http.Header
	OriginalRequest []byte
	RequestBody     []byte
	Body            []byte
	StatusCode      int
	Metadata        map[string]any
}

// ResponseInterceptResponse returns non-streaming response modifications.
type ResponseInterceptResponse struct {
	// Headers replaces matching current response headers and preserves headers not mentioned here.
	Headers http.Header
	// Body replaces the current response body only when non-empty.
	Body []byte
	// ClearHeaders explicitly removes current response headers before Headers is applied.
	ClearHeaders []string
}

// StreamChunkInterceptRequest describes a successful stream chunk before downstream delivery.
type StreamChunkInterceptRequest struct {
	RequestID       string
	SourceFormat    string
	Model           string
	RequestedModel  string
	RequestHeaders  http.Header
	ResponseHeaders http.Header
	// OriginalRequest contains the raw client request body.
	// It is always retained on header-init. On payload chunks schema 3 and newer omit it.
	OriginalRequest []byte
	// RequestBody contains the provider request body and follows the same schema rules as OriginalRequest.
	RequestBody []byte
	Body        []byte
	// HistoryChunks contains a bounded recent history of chunks already delivered downstream.
	// It is always retained on header-init. On payload chunks schema 5 and newer omit it.
	// Legacy plugins receive an independent clone for every invocation.
	HistoryChunks [][]byte
	// ChunkIndex starts at 0 for payload chunks. StreamChunkHeaderInitIndex marks the header-only initialization call.
	ChunkIndex int
	// Metadata is a best-effort cloned context snapshot. Treat it as read-only and JSON-like.
	Metadata map[string]any
}

// StreamChunkInterceptResponse returns stream chunk modifications.
type StreamChunkInterceptResponse struct {
	// Headers replaces matching current stream headers and preserves headers not mentioned here.
	Headers http.Header
	// Body replaces the current stream chunk body only when non-empty.
	Body []byte
	// ClearHeaders explicitly removes current stream headers before Headers is applied.
	ClearHeaders []string
	// DropChunk skips delivery of the current payload chunk and prevents it from entering HistoryChunks.
	// Header updates returned with DropChunk still apply to the interceptor chain state.
	DropChunk bool
}

// WebSocketResponseEvent describes an upstream WebSocket response event received during execution.
type WebSocketResponseEvent struct {
	RequestID      string
	TraceID        string
	SourceFormat   string
	Model          string
	RequestedModel string
	Provider       string
	AuthID         string
	AuthLabel      string
	AuthType       string
	EventType      string
	Payload        []byte
	// Metadata is cloned and stripped of secret-bearing fields before delivery.
	Metadata map[string]any
}

// PayloadResponse returns a transformed raw payload.
type PayloadResponse struct {
	// Body contains the transformed payload bytes.
	Body []byte
}

// ThinkingConfig is the public canonical thinking configuration passed to plugins.
type ThinkingConfig struct {
	// Mode is the canonical thinking mode: budget, level, none, or auto.
	Mode string
	// Budget is the normalized thinking token budget.
	Budget int
	// Level is the normalized named thinking effort level.
	Level string
}

// ThinkingApplyRequest asks a plugin to apply canonical thinking config.
type ThinkingApplyRequest struct {
	// Provider is the normalized provider key being applied.
	Provider string
	// Model describes the model associated with the request.
	Model ModelInfo
	// Config is the already parsed and normalized thinking config.
	Config ThinkingConfig
	// Body contains the provider payload to rewrite.
	Body []byte
}

// ThinkingApplier applies provider-specific thinking configuration.
type ThinkingApplier interface {
	// Identifier returns the provider key handled by this thinking applier.
	Identifier() string
	// ApplyThinking returns the payload with provider-specific thinking fields.
	ApplyThinking(context.Context, ThinkingApplyRequest) (PayloadResponse, error)
}

// UsagePlugin receives usage records after request completion.
type UsagePlugin interface {
	HandleUsage(context.Context, UsageRecord)
}

// CommandLinePlugin declares and handles plugin-owned command-line flags.
type CommandLinePlugin interface {
	RegisterCommandLine(context.Context, CommandLineRegistrationRequest) (CommandLineRegistrationResponse, error)
	ExecuteCommandLine(context.Context, CommandLineExecutionRequest) (CommandLineExecutionResponse, error)
}

// CommandLineRegistrationRequest carries host context for command-line registration.
type CommandLineRegistrationRequest struct {
	// Plugin is the metadata of the plugin being registered.
	Plugin Metadata
}

// CommandLineRegistrationResponse lists command-line flags owned by a plugin.
type CommandLineRegistrationResponse struct {
	// Flags contains the concrete flags to expose in -help.
	Flags []CommandLineFlag
}

// CommandLineFlag describes one plugin-owned command-line flag.
type CommandLineFlag struct {
	// Name is the flag name without leading dashes.
	Name string
	// Usage is shown in -help output.
	Usage string
	// Type is one of bool, string, int, int64, float64, or duration.
	Type string
	// DefaultValue is parsed according to Type before flag registration.
	DefaultValue string
}

// CommandLineFlagValue describes a parsed command-line flag value.
type CommandLineFlagValue struct {
	// Name is the flag name without leading dashes.
	Name string
	// Type is one of bool, string, int, int64, float64, or duration.
	Type string
	// Value is the parsed value in string form.
	Value string
	// Set reports whether the user explicitly provided this flag.
	Set bool
}

// CommandLineExecutionRequest describes a plugin command-line invocation.
type CommandLineExecutionRequest struct {
	// Plugin is the metadata of the plugin being executed.
	Plugin Metadata
	// Program is os.Args[0].
	Program string
	// Args contains every command-line argument after Program, including all flags.
	Args []string
	// ConfigPath is the effective configuration path used by the host.
	ConfigPath string
	// Host contains relevant host configuration.
	Host HostConfigSummary
	// Flags contains all currently registered command-line flags visible to the host.
	Flags map[string]CommandLineFlagValue
	// TriggeredFlags contains the plugin-owned flags that triggered this execution.
	TriggeredFlags map[string]CommandLineFlagValue
}

// CommandLineExecutionResponse returns command-line output from a plugin.
type CommandLineExecutionResponse struct {
	// Stdout is written to process stdout after plugin execution.
	Stdout []byte
	// Stderr is written to process stderr after plugin execution.
	Stderr []byte
	// Auths contains auth records created by the command. The host persists them.
	Auths []AuthData
	// ExitCode is used as the process exit code when non-zero.
	ExitCode int
}

// ManagementAPI declares plugin-owned Management API and resource routes.
type ManagementAPI interface {
	RegisterManagement(context.Context, ManagementRegistrationRequest) (ManagementRegistrationResponse, error)
}

// ManagementRegistrationRequest carries host context for Management API registration.
type ManagementRegistrationRequest struct {
	// Plugin is the metadata of the plugin being registered.
	Plugin Metadata
	// BasePath is the only Management API prefix plugins may register under.
	BasePath string
	// ResourceBasePath is the plugin resource prefix for browser-navigable resources.
	ResourceBasePath string
}

// ManagementRegistrationResponse lists plugin-owned Management API and resource routes.
type ManagementRegistrationResponse struct {
	// Routes contains the exact Management API routes to expose.
	Routes []ManagementRoute
	// Resources contains browser-navigable plugin resources exposed under /v0/resource/plugins/<pluginID>/.
	Resources []ResourceRoute
}

// ManagementRoute describes one plugin-owned Management API route.
type ManagementRoute struct {
	// Method is the HTTP method, for example GET or POST.
	Method string
	// Path is an exact path under /v0/management/. Relative paths are resolved under that prefix.
	Path string
	// Menu is a legacy resource menu label. GET routes with Menu are registered under /v0/resource/plugins/<pluginID>/.
	Menu string
	// Description explains the legacy resource menu entry for UI display.
	Description string
	// Handler processes matching Management API requests.
	Handler ManagementHandler
}

// ResourceRoute describes one plugin-owned browser-navigable resource route.
type ResourceRoute struct {
	// Path is an exact path under /v0/resource/plugins/<pluginID>/. Relative paths are resolved under that prefix.
	Path string
	// Menu is the management UI menu label for this GET resource.
	Menu string
	// Description explains the resource route for UI display.
	Description string
	// Handler processes matching resource requests after management authentication.
	Handler ManagementHandler
}

// ManagementHandler handles one plugin-owned Management API or resource route.
type ManagementHandler interface {
	HandleManagement(context.Context, ManagementRequest) (ManagementResponse, error)
}

// ManagementRequest describes an authenticated Management API request.
type ManagementRequest struct {
	// Method is the HTTP method.
	Method string
	// Path is the request path.
	Path string
	// Headers contains request headers.
	Headers http.Header
	// Query contains request query parameters.
	Query url.Values
	// Body contains the raw request body.
	Body []byte
}

// ManagementResponse describes a plugin Management API response.
type ManagementResponse struct {
	// StatusCode is the HTTP status code. Zero defaults to 200.
	StatusCode int
	// Headers contains response headers.
	Headers http.Header
	// Body contains raw response bytes. Schema 6 makes preservation of JSON angle
	// brackets and ampersands explicit; hosts also preserve the existing raw behavior for legacy schemas.
	Body []byte
}

// UsageRecord describes request usage and billing metadata.
type UsageRecord struct {
	// Provider identifies the upstream provider.
	Provider string
	// BaseURL is the configured upstream base URL when available.
	BaseURL string
	// ExecutorType identifies the executor implementation.
	ExecutorType string
	// Model is the model used for the request.
	Model string
	// Stream reports whether the request used streaming execution.
	Stream bool
	// Alias is the user-facing model alias when one was used.
	Alias string
	// APIKey is the client API key identifier when available.
	APIKey string
	// SessionID identifies the client session when present.
	SessionID string
	// ParentSessionID identifies the parent session in a hierarchy or fork.
	ParentSessionID string
	// AuthID identifies the selected credential.
	AuthID string
	// AuthIndex identifies the credential index when applicable.
	AuthIndex string
	// AuthType identifies the credential type.
	AuthType string
	// Source identifies the request source or integration.
	Source string
	// ReasoningEffort records the requested reasoning effort.
	ReasoningEffort string
	// ServiceTier records the client-requested service tier for compatibility.
	ServiceTier string
	// RequestServiceTier explicitly records the client-requested service tier.
	RequestServiceTier string
	// ResponseServiceTier records the final tier reported by the upstream response.
	ResponseServiceTier string
	// Generate reports whether the client requested actual generation.
	// The host normalizes omitted core usage values to true before delivery.
	Generate bool
	// RequestedAt is the time the request was received.
	RequestedAt time.Time
	// Latency is the total request latency.
	Latency time.Duration
	// TTFT is the time to first token for streaming requests.
	TTFT time.Duration
	// Failed reports whether the request failed.
	Failed bool
	// Failure contains failure details when Failed is true.
	Failure UsageFailure
	// Detail contains token usage counters.
	Detail UsageDetail
	// ResponseHeaders contains selected upstream response headers.
	ResponseHeaders http.Header
}

// UsageFailure describes an upstream or executor failure.
type UsageFailure struct {
	// StatusCode is the HTTP status code associated with the failure.
	StatusCode int
	// Body contains the failure response body or message.
	Body string
}

// UsageDetail contains token accounting counters.
type UsageDetail struct {
	// InputTokens is the prompt or input token count.
	InputTokens int64
	// OutputTokens is the completion or output token count.
	OutputTokens int64
	// ReasoningTokens is the reasoning token count.
	ReasoningTokens int64
	// CachedTokens is the total cached token count.
	CachedTokens int64
	// CacheReadTokens is the cache read token count.
	CacheReadTokens int64
	// CacheCreationTokens is the cache creation token count.
	CacheCreationTokens int64
	// TotalTokens is the total token count.
	TotalTokens int64
	// ResponseServiceTier records the final tier reported by the upstream response.
	ResponseServiceTier string
}

// ErrQuotaUnsupported lets a quota provider explicitly decline an operation so the host may try a declarative probe.
var ErrQuotaUnsupported = errors.New("quota operation unsupported")

// QuotaProvider surfaces normalized credential quota information.
type QuotaProvider interface {
	Identifier() string
	DescribeQuota(context.Context, QuotaDescribeRequest) (QuotaDescribeResponse, error)
	FetchQuota(context.Context, QuotaFetchRequest) (QuotaFetchResponse, error)
	ResetQuota(context.Context, QuotaResetRequest) (QuotaResetResponse, error)
}

// QuotaDescribeRequest carries host context for describing quota capabilities.
type QuotaDescribeRequest struct {
	Plugin Metadata `json:"plugin,omitempty"`
}

// QuotaDescribeResponse describes supported providers and reset capability.
type QuotaDescribeResponse struct {
	SupportedProviders []string `json:"supported_providers,omitempty"`
	DisplayName        string   `json:"display_name,omitempty"`
	SupportsReset      bool     `json:"supports_reset,omitempty"`
}

// QuotaFetchRequest carries credential context for querying quota.
type QuotaFetchRequest struct {
	AuthIndex   string            `json:"auth_index"`
	AuthID      string            `json:"auth_id"`
	Provider    string            `json:"provider"`
	StorageJSON []byte            `json:"storage_json,omitempty"`
	Metadata    map[string]any    `json:"metadata,omitempty"`
	Attributes  map[string]string `json:"attributes,omitempty"`
	Host        HostConfigSummary `json:"host,omitempty"`
	HTTPClient  HostHTTPClient    `json:"-"`
}

// QuotaSubscription describes normalized plan and tier information.
type QuotaSubscription struct {
	Plan     string `json:"plan,omitempty"`
	TierName string `json:"tierName,omitempty"`
	TierID   string `json:"tierId,omitempty"`
}

// UnmarshalJSON accepts both camelCase and snake_case field names.
func (s *QuotaSubscription) UnmarshalJSON(data []byte) error {
	type alias QuotaSubscription
	aux := struct {
		*alias
		AltTierName string `json:"tier_name"`
		AltTierID   string `json:"tier_id"`
	}{alias: (*alias)(s)}
	if err := json.Unmarshal(data, &aux); err != nil {
		return err
	}
	if s.TierName == "" {
		s.TierName = aux.AltTierName
	}
	if s.TierID == "" {
		s.TierID = aux.AltTierID
	}
	return nil
}

// QuotaGroup describes a group of normalized quota buckets.
type QuotaGroup struct {
	DisplayName string        `json:"displayName,omitempty"`
	Buckets     []QuotaBucket `json:"buckets,omitempty"`
}

// UnmarshalJSON accepts both camelCase and snake_case field names.
func (g *QuotaGroup) UnmarshalJSON(data []byte) error {
	type alias QuotaGroup
	aux := struct {
		*alias
		AltDisplayName string `json:"display_name"`
	}{alias: (*alias)(g)}
	if err := json.Unmarshal(data, &aux); err != nil {
		return err
	}
	if g.DisplayName == "" {
		g.DisplayName = aux.AltDisplayName
	}
	return nil
}

// QuotaBucket describes one normalized quota window or limit.
type QuotaBucket struct {
	Window            string  `json:"window,omitempty"`
	RemainingFraction float64 `json:"remainingFraction"`
	ResetTime         string  `json:"resetTime,omitempty"`
	Description       string  `json:"description,omitempty"`
}

// UnmarshalJSON accepts both camelCase and snake_case fields while preserving an explicit zero fraction.
func (b *QuotaBucket) UnmarshalJSON(data []byte) error {
	type alias QuotaBucket
	aux := struct {
		*alias
		RemainingFraction    *float64 `json:"remainingFraction"`
		AltRemainingFraction *float64 `json:"remaining_fraction"`
		AltResetTime         string   `json:"reset_time"`
	}{alias: (*alias)(b)}
	if err := json.Unmarshal(data, &aux); err != nil {
		return err
	}
	if aux.RemainingFraction != nil {
		b.RemainingFraction = *aux.RemainingFraction
	} else if aux.AltRemainingFraction != nil {
		b.RemainingFraction = *aux.AltRemainingFraction
	}
	if b.ResetTime == "" {
		b.ResetTime = aux.AltResetTime
	}
	return nil
}

// QuotaFetchResponse carries normalized quota information.
type QuotaFetchResponse struct {
	Subscription       *QuotaSubscription `json:"subscription,omitempty"`
	ServerTimeOffsetMs int64              `json:"serverTimeOffsetMs,omitempty"`
	Groups             []QuotaGroup       `json:"groups,omitempty"`
}

// UnmarshalJSON accepts both camelCase and snake_case field names.
func (r *QuotaFetchResponse) UnmarshalJSON(data []byte) error {
	type alias QuotaFetchResponse
	aux := struct {
		*alias
		AltServerTimeOffsetMs *int64 `json:"server_time_offset_ms"`
	}{alias: (*alias)(r)}
	if err := json.Unmarshal(data, &aux); err != nil {
		return err
	}
	if r.ServerTimeOffsetMs == 0 && aux.AltServerTimeOffsetMs != nil {
		r.ServerTimeOffsetMs = *aux.AltServerTimeOffsetMs
	}
	return nil
}

// QuotaResetRequest carries credential context for an explicitly authorized quota reset.
type QuotaResetRequest struct {
	AuthIndex   string            `json:"auth_index"`
	AuthID      string            `json:"auth_id"`
	Provider    string            `json:"provider"`
	StorageJSON []byte            `json:"storage_json,omitempty"`
	Metadata    map[string]any    `json:"metadata,omitempty"`
	Attributes  map[string]string `json:"attributes,omitempty"`
	Host        HostConfigSummary `json:"host,omitempty"`
	HTTPClient  HostHTTPClient    `json:"-"`
}

// QuotaResetResponse returns the result of a quota reset action.
type QuotaResetResponse struct {
	Success bool   `json:"success"`
	Message string `json:"message,omitempty"`
}

const (
	// QuotaProbeMetadataKey is the auth metadata key containing a declarative quota probe.
	QuotaProbeMetadataKey = "quota_probe"
	// QuotaProbeTokenPlaceholder is replaced by the credential token only by an authorized probe executor.
	QuotaProbeTokenPlaceholder = "TOKEN"
)

// QuotaProbe describes the declarative HTTP probe contract consumed after plugin providers decline a credential.
// Probe execution, target validation, redirects, limits, and response mapping belong to the management integration.
type QuotaProbe struct {
	URL     string            `json:"url"`
	Method  string            `json:"method,omitempty"`
	Data    any               `json:"data,omitempty"`
	Headers map[string]string `json:"headers,omitempty"`
	// Mapping maps normalized quota output fields to gjson response paths.
	Mapping map[string]string `json:"mapping,omitempty"`
}
