// Package logging exposes request logging primitives for SDK consumers.
package logging

import (
	"fmt"
	"time"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/interfaces"
	internallogging "github.com/Pyrokine/CLIProxyAPI/v6/internal/logging"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/api/handlers"
)

const defaultErrorLogsMaxFiles = 10

// PendingCleanupRetrier is an optional request logger capability for retrying deferred cleanup.
type PendingCleanupRetrier interface {
	RetryPendingCleanup() error
}

// RequestLogger defines the interface for logging HTTP requests and responses.
type RequestLogger interface {
	LogRequest(
		url, method string,
		requestHeaders map[string][]string,
		body []byte,
		statusCode int,
		responseHeaders map[string][]string,
		response, websocketTimeline, apiRequest, apiResponse, apiWebsocketTimeline []byte,
		apiResponseErrors []*handlers.ErrorMessage,
		requestID string,
		requestTimestamp, apiResponseTimestamp time.Time,
	) error

	LogStreamingRequest(
		url, method string,
		headers map[string][]string,
		body []byte,
		requestID string,
	) (StreamingLogWriter, error)

	IsEnabled() bool
}

// StreamingLogWriter handles real-time logging of streaming response chunks.
type StreamingLogWriter interface {
	WriteChunkAsync(chunk []byte)
	WriteStatus(status int, headers map[string][]string) error
	WriteAPIRequest(apiRequest []byte) error
	WriteAPIResponse(apiResponse []byte) error
	WriteAPIWebsocketTimeline(apiWebsocketTimeline []byte) error
	SetFirstChunkTimestamp(timestamp time.Time)
	WriteRequestTimeline(requestTimeline []byte) error
	Close() error
}

// FileRequestLogger implements RequestLogger using file-based storage.
type FileRequestLogger struct {
	inner *internallogging.FileRequestLogger
}

type fileRequestLoggerSnapshot struct {
	inner internallogging.RequestLogger
}

// NewFileRequestLogger creates a new file-based request logger with default error log retention (10 files).
// noinspection GoUnusedExportedFunction
func NewFileRequestLogger(enabled bool, logsDir string, configDir string) *FileRequestLogger {
	return &FileRequestLogger{
		inner: internallogging.NewFileRequestLogger(enabled, logsDir, configDir, defaultErrorLogsMaxFiles),
	}
}

// NewFileRequestLoggerWithOptions creates a new file-based request logger with configurable error log retention.
func NewFileRequestLoggerWithOptions(
	enabled bool,
	logsDir string,
	configDir string,
	errorLogsMaxFiles int,
) *FileRequestLogger {
	return &FileRequestLogger{
		inner: internallogging.NewFileRequestLogger(enabled, logsDir, configDir, errorLogsMaxFiles),
	}
}

func (l *FileRequestLogger) LogRequest(
	url, method string,
	requestHeaders map[string][]string,
	body []byte,
	statusCode int,
	responseHeaders map[string][]string,
	response, websocketTimeline, apiRequest, apiResponse, apiWebsocketTimeline []byte,
	apiResponseErrors []*handlers.ErrorMessage,
	requestID string,
	requestTimestamp, apiResponseTimestamp time.Time,
) error {
	if l == nil || l.inner == nil {
		return fmt.Errorf("sdk logging: file request logger is nil")
	}
	return l.inner.LogRequest(
		url, method, requestHeaders, body, statusCode, responseHeaders, response, websocketTimeline,
		apiRequest, apiResponse, apiWebsocketTimeline, toInternalErrorMessages(apiResponseErrors), requestID,
		requestTimestamp, apiResponseTimestamp,
	)
}

func (l *FileRequestLogger) LogRequestWithOptions(
	url, method string,
	requestHeaders map[string][]string,
	body []byte,
	statusCode int,
	responseHeaders map[string][]string,
	response, websocketTimeline, apiRequest, apiResponse, apiWebsocketTimeline []byte,
	apiResponseErrors []*handlers.ErrorMessage,
	force bool,
	requestID string,
	requestTimestamp, apiResponseTimestamp time.Time,
) error {
	if l == nil || l.inner == nil {
		return fmt.Errorf("sdk logging: file request logger is nil")
	}
	return l.inner.LogRequestWithOptions(
		url, method, requestHeaders, body, statusCode, responseHeaders, response, websocketTimeline,
		apiRequest, apiResponse, apiWebsocketTimeline, toInternalErrorMessages(apiResponseErrors), force, requestID,
		requestTimestamp, apiResponseTimestamp,
	)
}

func (l *FileRequestLogger) LogRequestWithOptionsAndTimeline(
	url, method string,
	requestHeaders map[string][]string,
	body []byte,
	statusCode int,
	responseHeaders map[string][]string,
	response, websocketTimeline, apiRequest, apiResponse, apiWebsocketTimeline, requestTimeline []byte,
	apiResponseErrors []*handlers.ErrorMessage,
	force bool,
	requestID string,
	requestTimestamp, apiResponseTimestamp time.Time,
) error {
	if l == nil || l.inner == nil {
		return fmt.Errorf("sdk logging: file request logger is nil")
	}
	return l.inner.LogRequestWithOptionsAndTimeline(
		url, method, requestHeaders, body, statusCode, responseHeaders, response, websocketTimeline,
		apiRequest, apiResponse, apiWebsocketTimeline, requestTimeline, toInternalErrorMessages(apiResponseErrors),
		force,
		requestID, requestTimestamp, apiResponseTimestamp,
	)
}

func (l *FileRequestLogger) LogRequestWithOptionsAndAllSources(
	url, method string,
	requestHeaders map[string][]string,
	body []byte,
	statusCode int,
	responseHeaders map[string][]string,
	response, websocketTimeline []byte,
	websocketTimelineSource *internallogging.FileBodySource,
	apiRequest []byte,
	apiRequestSource *internallogging.FileBodySource,
	apiResponse []byte,
	apiResponseSource *internallogging.FileBodySource,
	apiWebsocketTimeline []byte,
	apiWebsocketTimelineSource *internallogging.FileBodySource,
	requestTimeline []byte,
	apiResponseErrors []*handlers.ErrorMessage,
	force bool,
	requestID string,
	requestTimestamp, apiResponseTimestamp time.Time,
) error {
	if l == nil || l.inner == nil {
		return fmt.Errorf("sdk logging: file request logger is nil")
	}
	return l.inner.LogRequestWithOptionsAndAllSources(
		url, method, requestHeaders, body, statusCode, responseHeaders, response, websocketTimeline,
		websocketTimelineSource, apiRequest, apiRequestSource, apiResponse, apiResponseSource, apiWebsocketTimeline,
		apiWebsocketTimelineSource, requestTimeline, toInternalErrorMessages(apiResponseErrors), force, requestID,
		requestTimestamp, apiResponseTimestamp,
	)
}

func (l *FileRequestLogger) NewFileBodySource(prefix string) (*internallogging.FileBodySource, error) {
	if l == nil || l.inner == nil {
		return nil, fmt.Errorf("sdk logging: file request logger is nil")
	}
	return l.inner.NewFileBodySource(prefix)
}

func (l *FileRequestLogger) LogStreamingRequest(
	url, method string,
	headers map[string][]string,
	body []byte,
	requestID string,
) (StreamingLogWriter, error) {
	if l == nil || l.inner == nil {
		return nil, fmt.Errorf("sdk logging: file request logger is nil")
	}
	writer, err := l.inner.LogStreamingRequest(url, method, headers, body, requestID)
	if err != nil {
		return nil, err
	}
	return writer, nil
}

func (l *FileRequestLogger) RetryPendingCleanup() error {
	if l == nil || l.inner == nil {
		return nil
	}
	return l.inner.RetryPendingCleanup()
}

func (l *FileRequestLogger) IsEnabled() bool {
	return l != nil && l.inner != nil && l.inner.IsEnabled()
}

func (l *FileRequestLogger) Snapshot() RequestLogger {
	if l == nil || l.inner == nil {
		return nil
	}
	return &fileRequestLoggerSnapshot{inner: l.inner.Snapshot()}
}

func (s *fileRequestLoggerSnapshot) RetryPendingCleanup() error {
	if s == nil || s.inner == nil {
		return nil
	}
	if retrier, ok := s.inner.(interface{ RetryPendingCleanup() error }); ok {
		return retrier.RetryPendingCleanup()
	}
	return nil
}

func (s *fileRequestLoggerSnapshot) IsEnabled() bool {
	return s != nil && s.inner != nil && s.inner.IsEnabled()
}

func (s *fileRequestLoggerSnapshot) LogRequest(
	url, method string,
	requestHeaders map[string][]string,
	body []byte,
	statusCode int,
	responseHeaders map[string][]string,
	response, websocketTimeline, apiRequest, apiResponse, apiWebsocketTimeline []byte,
	apiResponseErrors []*handlers.ErrorMessage,
	requestID string,
	requestTimestamp, apiResponseTimestamp time.Time,
) error {
	if s == nil || s.inner == nil {
		return fmt.Errorf("sdk logging: file request logger snapshot is nil")
	}
	return s.inner.LogRequest(
		url, method, requestHeaders, body, statusCode, responseHeaders, response, websocketTimeline,
		apiRequest, apiResponse, apiWebsocketTimeline, toInternalErrorMessages(apiResponseErrors), requestID,
		requestTimestamp, apiResponseTimestamp,
	)
}

func (s *fileRequestLoggerSnapshot) LogRequestWithOptionsAndTimeline(
	url, method string,
	requestHeaders map[string][]string,
	body []byte,
	statusCode int,
	responseHeaders map[string][]string,
	response, websocketTimeline, apiRequest, apiResponse, apiWebsocketTimeline, requestTimeline []byte,
	apiResponseErrors []*handlers.ErrorMessage,
	force bool,
	requestID string,
	requestTimestamp, apiResponseTimestamp time.Time,
) error {
	if s == nil || s.inner == nil {
		return fmt.Errorf("sdk logging: file request logger snapshot is nil")
	}
	logger, ok := s.inner.(interface {
		LogRequestWithOptionsAndTimeline(
			string, string, map[string][]string, []byte, int, map[string][]string,
			[]byte, []byte, []byte, []byte, []byte, []byte, []*interfaces.ErrorMessage,
			bool, string, time.Time, time.Time,
		) error
	})
	if !ok {
		return s.LogRequest(
			url, method, requestHeaders, body, statusCode, responseHeaders, response, websocketTimeline,
			apiRequest, apiResponse, apiWebsocketTimeline, apiResponseErrors, requestID, requestTimestamp,
			apiResponseTimestamp,
		)
	}
	return logger.LogRequestWithOptionsAndTimeline(
		url, method, requestHeaders, body, statusCode, responseHeaders, response, websocketTimeline,
		apiRequest, apiResponse, apiWebsocketTimeline, requestTimeline, toInternalErrorMessages(apiResponseErrors),
		force,
		requestID, requestTimestamp, apiResponseTimestamp,
	)
}

func (s *fileRequestLoggerSnapshot) LogRequestWithOptionsAndAllSources(
	url, method string,
	requestHeaders map[string][]string,
	body []byte,
	statusCode int,
	responseHeaders map[string][]string,
	response, websocketTimeline []byte,
	websocketTimelineSource *internallogging.FileBodySource,
	apiRequest []byte,
	apiRequestSource *internallogging.FileBodySource,
	apiResponse []byte,
	apiResponseSource *internallogging.FileBodySource,
	apiWebsocketTimeline []byte,
	apiWebsocketTimelineSource *internallogging.FileBodySource,
	requestTimeline []byte,
	apiResponseErrors []*handlers.ErrorMessage,
	force bool,
	requestID string,
	requestTimestamp, apiResponseTimestamp time.Time,
) error {
	if s == nil || s.inner == nil {
		return fmt.Errorf("sdk logging: file request logger snapshot is nil")
	}
	logger, ok := s.inner.(interface {
		LogRequestWithOptionsAndAllSources(
			string, string, map[string][]string, []byte, int, map[string][]string,
			[]byte, []byte, *internallogging.FileBodySource, []byte, *internallogging.FileBodySource,
			[]byte, *internallogging.FileBodySource, []byte, *internallogging.FileBodySource,
			[]byte, []*interfaces.ErrorMessage, bool, string, time.Time, time.Time,
		) error
	})
	if !ok {
		return s.LogRequestWithOptionsAndTimeline(
			url, method, requestHeaders, body, statusCode, responseHeaders, response, websocketTimeline,
			apiRequest, apiResponse, apiWebsocketTimeline, requestTimeline, apiResponseErrors, force, requestID,
			requestTimestamp, apiResponseTimestamp,
		)
	}
	return logger.LogRequestWithOptionsAndAllSources(
		url, method, requestHeaders, body, statusCode, responseHeaders, response, websocketTimeline,
		websocketTimelineSource, apiRequest, apiRequestSource, apiResponse, apiResponseSource, apiWebsocketTimeline,
		apiWebsocketTimelineSource, requestTimeline, toInternalErrorMessages(apiResponseErrors), force, requestID,
		requestTimestamp, apiResponseTimestamp,
	)
}

func (s *fileRequestLoggerSnapshot) NewFileBodySource(prefix string) (*internallogging.FileBodySource, error) {
	if s == nil || s.inner == nil {
		return nil, fmt.Errorf("sdk logging: file request logger snapshot is nil")
	}
	factory, ok := s.inner.(interface {
		NewFileBodySource(string) (*internallogging.FileBodySource, error)
	})
	if !ok {
		return nil, fmt.Errorf("sdk logging: file request logger does not support file body sources")
	}
	return factory.NewFileBodySource(prefix)
}

func (s *fileRequestLoggerSnapshot) LogStreamingRequest(
	url, method string,
	headers map[string][]string,
	body []byte,
	requestID string,
) (StreamingLogWriter, error) {
	if s == nil || s.inner == nil {
		return nil, fmt.Errorf("sdk logging: file request logger snapshot is nil")
	}
	return s.inner.LogStreamingRequest(url, method, headers, body, requestID)
}

func (l *FileRequestLogger) SetHomeEnabled(enabled bool) {
	if l != nil && l.inner != nil {
		l.inner.SetHomeEnabled(enabled)
	}
}

func (l *FileRequestLogger) SetEnabled(enabled bool) {
	if l != nil && l.inner != nil {
		l.inner.SetEnabled(enabled)
	}
}

func (l *FileRequestLogger) SetErrorLogsMaxFiles(maxFiles int) {
	if l != nil && l.inner != nil {
		l.inner.SetErrorLogsMaxFiles(maxFiles)
	}
}

func toInternalErrorMessages(messages []*handlers.ErrorMessage) []*interfaces.ErrorMessage {
	if len(messages) == 0 {
		return nil
	}
	out := make([]*interfaces.ErrorMessage, 0, len(messages))
	for _, message := range messages {
		if message == nil {
			continue
		}
		out = append(
			out, &interfaces.ErrorMessage{
				StatusCode: message.StatusCode,
				Error:      message.Error,
				Addon:      message.Addon,
			},
		)
	}
	return out
}
