package handlers

import "net/http"

// APIHandler defines the interface that all API handlers must implement.
type APIHandler interface {
	// HandlerType returns the type identifier for this API handler.
	HandlerType() string

	// Models returns metadata for models supported by this API handler.
	Models() []map[string]any
}

// ErrorMessage encapsulates an error with an associated HTTP status code.
type ErrorMessage struct {
	// StatusCode is the HTTP status code returned by the API.
	StatusCode int

	// Error is the underlying error that occurred.
	Error error

	// Addon contains additional headers to be added to the response.
	Addon http.Header
}
