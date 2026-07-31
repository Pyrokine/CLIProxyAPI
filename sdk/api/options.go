// Package api exposes server option helpers for embedding CLIProxyAPI.
//
// It wraps internal server option types so external projects can configure the embedded
// HTTP server without importing internal packages.
package api

import (
	"errors"
	"fmt"
	"sync/atomic"
	"time"

	internalapi "github.com/Pyrokine/CLIProxyAPI/v6/internal/api"
	internalinterfaces "github.com/Pyrokine/CLIProxyAPI/v6/internal/interfaces"
	internallogging "github.com/Pyrokine/CLIProxyAPI/v6/internal/logging"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/api/handlers"
	coreauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/config"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/logging"
	"github.com/gin-gonic/gin"
	log "github.com/sirupsen/logrus"
)

// ServerOption customises HTTP server construction.
type ServerOption = internalapi.ServerOption

// WithMiddleware appends additional Gin middleware during server construction.
func WithMiddleware(mw ...gin.HandlerFunc) ServerOption {
	return internalapi.WithMiddleware(mw...)
}

// WithEngineConfigurator allows callers to mutate the Gin engine prior to middleware setup.
// noinspection GoUnusedExportedFunction
func WithEngineConfigurator(fn func(*gin.Engine)) ServerOption {
	return internalapi.WithEngineConfigurator(fn)
}

// WithRouterConfigurator appends a callback after default routes are registered.
// noinspection GoUnusedExportedFunction
func WithRouterConfigurator(fn func(*gin.Engine, *handlers.BaseAPIHandler, *config.Config)) ServerOption {
	return internalapi.WithRouterConfigurator(fn)
}

// WithLocalManagementPassword stores a runtime-only management password accepted for localhost requests.
// noinspection GoUnusedExportedFunction
func WithLocalManagementPassword(password string) ServerOption {
	return internalapi.WithLocalManagementPassword(password)
}

// WithKeepAliveEndpoint enables a keep-alive endpoint with the provided timeout and callback.
// noinspection GoUnusedExportedFunction
func WithKeepAliveEndpoint(timeout time.Duration, onTimeout func()) ServerOption {
	return internalapi.WithKeepAliveEndpoint(timeout, onTimeout)
}

// WithRequestLoggerFactory customises request logger creation.
func WithRequestLoggerFactory(factory func(*config.Config, string) logging.RequestLogger) ServerOption {
	return internalapi.WithRequestLoggerFactory(func(cfg *config.Config, configPath string) internallogging.RequestLogger {
		if factory == nil {
			return nil
		}
		logger := factory(cfg, configPath)
		if logger == nil {
			return nil
		}
		if errCleanup := internallogging.RetryLegacySDKRequestLogCleanup(); errCleanup != nil {
			log.WithError(errCleanup).Warn("failed to recover legacy SDK request log sources")
		}
		return newRequestLoggerAdapter(logger)
	})
}

// WithPostAuthHook registers a hook to be called after auth record creation.
func WithPostAuthHook(hook coreauth.PostAuthHook) ServerOption {
	return internalapi.WithPostAuthHook(hook)
}

// WithExampleAPIKeySafeMode blocks proxy API endpoints while template API keys remain configured.
func WithExampleAPIKeySafeMode() ServerOption {
	return internalapi.WithExampleAPIKeySafeMode()
}

type requestLoggerAdapter struct {
	logger             logging.RequestLogger
	enabledSnapshot    *bool
	legacyEnabledState *atomic.Bool
}

func newRequestLoggerAdapter(logger logging.RequestLogger) requestLoggerAdapter {
	adapter := requestLoggerAdapter{logger: logger}
	if logger == nil {
		return adapter
	}
	if _, ok := logger.(interface{ Snapshot() logging.RequestLogger }); ok {
		return adapter
	}
	if setter, ok := logger.(interface{ SetEnabled(bool) }); ok {
		state := &atomic.Bool{}
		state.Store(logger.IsEnabled())
		setter.SetEnabled(true)
		adapter.legacyEnabledState = state
	}
	return adapter
}

func (a requestLoggerAdapter) LogRequest(
	url, method string,
	requestHeaders map[string][]string,
	body []byte,
	statusCode int,
	responseHeaders map[string][]string,
	response, websocketTimeline, apiRequest, apiResponse, apiWebsocketTimeline []byte,
	apiResponseErrors []*internalinterfaces.ErrorMessage,
	requestID string,
	requestTimestamp, apiResponseTimestamp time.Time,
) error {
	return a.logger.LogRequest(
		url, method, requestHeaders, body, statusCode, responseHeaders, response, websocketTimeline,
		apiRequest, apiResponse, apiWebsocketTimeline, fromInternalErrorMessages(apiResponseErrors), requestID,
		requestTimestamp, apiResponseTimestamp,
	)
}

func (a requestLoggerAdapter) LogRequestWithOptionsAndTimeline(
	url, method string,
	requestHeaders map[string][]string,
	body []byte,
	statusCode int,
	responseHeaders map[string][]string,
	response, websocketTimeline, apiRequest, apiResponse, apiWebsocketTimeline, requestTimeline []byte,
	apiResponseErrors []*internalinterfaces.ErrorMessage,
	force bool,
	requestID string,
	requestTimestamp, apiResponseTimestamp time.Time,
) error {
	if loggerWithTimeline, ok := a.logger.(interface {
		LogRequestWithOptionsAndTimeline(
			string,
			string,
			map[string][]string,
			[]byte,
			int,
			map[string][]string,
			[]byte,
			[]byte,
			[]byte,
			[]byte,
			[]byte,
			[]byte,
			[]*handlers.ErrorMessage,
			bool,
			string,
			time.Time,
			time.Time,
		) error
	}); ok {
		return loggerWithTimeline.LogRequestWithOptionsAndTimeline(
			url, method, requestHeaders, body, statusCode, responseHeaders, response, websocketTimeline,
			apiRequest, apiResponse, apiWebsocketTimeline, requestTimeline, fromInternalErrorMessages(apiResponseErrors), force,
			requestID, requestTimestamp, apiResponseTimestamp,
		)
	}
	if loggerWithOptions, ok := a.logger.(interface {
		LogRequestWithOptions(
			string,
			string,
			map[string][]string,
			[]byte,
			int,
			map[string][]string,
			[]byte,
			[]byte,
			[]byte,
			[]byte,
			[]byte,
			[]*handlers.ErrorMessage,
			bool,
			string,
			time.Time,
			time.Time,
		) error
	}); ok {
		return loggerWithOptions.LogRequestWithOptions(
			url, method, requestHeaders, body, statusCode, responseHeaders, response, websocketTimeline,
			apiRequest, apiResponse, apiWebsocketTimeline, fromInternalErrorMessages(apiResponseErrors), force,
			requestID, requestTimestamp, apiResponseTimestamp,
		)
	}
	return a.LogRequest(
		url, method, requestHeaders, body, statusCode, responseHeaders, response, websocketTimeline,
		apiRequest, apiResponse, apiWebsocketTimeline, apiResponseErrors, requestID, requestTimestamp,
		apiResponseTimestamp,
	)
}

func (a requestLoggerAdapter) LogRequestWithOptionsAndAllSources(
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
	apiResponseErrors []*internalinterfaces.ErrorMessage,
	force bool,
	requestID string,
	requestTimestamp, apiResponseTimestamp time.Time,
) (err error) {
	defer func() {
		err = errors.Join(
			err,
			cleanupRequestLogSources(
				websocketTimelineSource,
				apiRequestSource,
				apiResponseSource,
				apiWebsocketTimelineSource,
			),
		)
	}()
	logger, ok := a.logger.(interface {
		LogRequestWithOptionsAndAllSources(
			string, string, map[string][]string, []byte, int, map[string][]string,
			[]byte, []byte, *internallogging.FileBodySource, []byte, *internallogging.FileBodySource,
			[]byte, *internallogging.FileBodySource, []byte, *internallogging.FileBodySource,
			[]byte, []*handlers.ErrorMessage, bool, string, time.Time, time.Time,
		) error
	})
	if !ok {
		var mergeErr error
		var errMerge error
		websocketTimeline, errMerge = mergeRequestLogSource(websocketTimeline, websocketTimelineSource)
		if errMerge != nil {
			mergeErr = errors.Join(
				mergeErr,
				fmt.Errorf("merge SDK request log websocket timeline source: %w", errMerge),
			)
		}
		apiRequest, errMerge = mergeRequestLogSource(apiRequest, apiRequestSource)
		if errMerge != nil {
			mergeErr = errors.Join(mergeErr, fmt.Errorf("merge SDK request log API request source: %w", errMerge))
		}
		apiResponse, errMerge = mergeRequestLogSource(apiResponse, apiResponseSource)
		if errMerge != nil {
			mergeErr = errors.Join(mergeErr, fmt.Errorf("merge SDK request log API response source: %w", errMerge))
		}
		apiWebsocketTimeline, errMerge = mergeRequestLogSource(
			apiWebsocketTimeline,
			apiWebsocketTimelineSource,
		)
		if errMerge != nil {
			mergeErr = errors.Join(
				mergeErr,
				fmt.Errorf("merge SDK request log API websocket timeline source: %w", errMerge),
			)
		}
		errLog := a.LogRequestWithOptionsAndTimeline(
			url, method, requestHeaders, body, statusCode, responseHeaders, response, websocketTimeline,
			apiRequest, apiResponse, apiWebsocketTimeline, requestTimeline, apiResponseErrors, force, requestID,
			requestTimestamp, apiResponseTimestamp,
		)
		return errors.Join(mergeErr, errLog)
	}
	return logger.LogRequestWithOptionsAndAllSources(
		url, method, requestHeaders, body, statusCode, responseHeaders, response, websocketTimeline,
		websocketTimelineSource, apiRequest, apiRequestSource, apiResponse, apiResponseSource, apiWebsocketTimeline,
		apiWebsocketTimelineSource, requestTimeline, fromInternalErrorMessages(apiResponseErrors), force, requestID,
		requestTimestamp, apiResponseTimestamp,
	)
}

func (a requestLoggerAdapter) NewFileBodySource(prefix string) (*internallogging.FileBodySource, error) {
	factory, ok := a.logger.(interface {
		NewFileBodySource(string) (*internallogging.FileBodySource, error)
	})
	if ok {
		return factory.NewFileBodySource(prefix)
	}
	return internallogging.NewLegacySDKFileBodySource(prefix)
}

func (a requestLoggerAdapter) RetryPendingCleanup() error {
	var loggerCleanupErr error
	if retrier, ok := a.logger.(interface{ RetryPendingCleanup() error }); ok && retrier != nil {
		loggerCleanupErr = retrySDKRequestLogCleanup("retry SDK logger cleanup", retrier.RetryPendingCleanup)
	}
	legacyCleanupErr := retrySDKRequestLogCleanup(
		"retry legacy SDK request log cleanup",
		internallogging.RetryLegacySDKRequestLogCleanup,
	)
	return errors.Join(loggerCleanupErr, legacyCleanupErr)
}

func retrySDKRequestLogCleanup(operation string, cleanup func() error) (err error) {
	defer func() {
		if panicValue := recover(); panicValue != nil {
			err = fmt.Errorf("%s panic: %v", operation, panicValue)
		}
	}()
	if errCleanup := cleanup(); errCleanup != nil {
		return fmt.Errorf("%s: %w", operation, errCleanup)
	}
	return nil
}

func (a requestLoggerAdapter) LogStreamingRequest(
	url, method string,
	headers map[string][]string,
	body []byte,
	requestID string,
) (internallogging.StreamingLogWriter, error) {
	return a.logger.LogStreamingRequest(url, method, headers, body, requestID)
}

func (a requestLoggerAdapter) IsEnabled() bool {
	if a.enabledSnapshot != nil {
		return *a.enabledSnapshot
	}
	if a.legacyEnabledState != nil {
		return a.legacyEnabledState.Load()
	}
	return a.logger != nil && a.logger.IsEnabled()
}

func (a requestLoggerAdapter) Snapshot() internallogging.RequestLogger {
	if a.logger == nil {
		return nil
	}
	logger := a.logger
	if snapshotter, ok := logger.(interface{ Snapshot() logging.RequestLogger }); ok {
		logger = snapshotter.Snapshot()
		if logger == nil {
			return nil
		}
	}
	enabled := logger.IsEnabled()
	if a.legacyEnabledState != nil {
		enabled = a.legacyEnabledState.Load()
	}
	return requestLoggerAdapter{
		logger:             logger,
		enabledSnapshot:    &enabled,
		legacyEnabledState: a.legacyEnabledState,
	}
}

func (a requestLoggerAdapter) SetEnabled(enabled bool) {
	if a.enabledSnapshot != nil {
		return
	}
	if a.legacyEnabledState != nil {
		a.legacyEnabledState.Store(enabled)
		return
	}
	if setter, ok := a.logger.(interface{ SetEnabled(bool) }); ok {
		setter.SetEnabled(enabled)
	}
}

func (a requestLoggerAdapter) SetHomeEnabled(enabled bool) {
	if setter, ok := a.logger.(interface{ SetHomeEnabled(bool) }); ok {
		setter.SetHomeEnabled(enabled)
	}
}

func (a requestLoggerAdapter) SetErrorLogsMaxFiles(maxFiles int) {
	if setter, ok := a.logger.(interface{ SetErrorLogsMaxFiles(int) }); ok {
		setter.SetErrorLogsMaxFiles(maxFiles)
	}
}

func mergeRequestLogSource(
	payload []byte,
	source *internallogging.FileBodySource,
) (merged []byte, err error) {
	return internallogging.MergeFileBodySource(payload, source)
}

func cleanupRequestLogSources(sources ...*internallogging.FileBodySource) error {
	return internallogging.CleanupFileBodySources(sources...)
}

func fromInternalErrorMessages(messages []*internalinterfaces.ErrorMessage) []*handlers.ErrorMessage {
	if len(messages) == 0 {
		return nil
	}
	out := make([]*handlers.ErrorMessage, 0, len(messages))
	for _, message := range messages {
		if message == nil {
			continue
		}
		out = append(out, &handlers.ErrorMessage{
			StatusCode: message.StatusCode,
			Error:      message.Error,
			Addon:      message.Addon,
		})
	}
	return out
}
