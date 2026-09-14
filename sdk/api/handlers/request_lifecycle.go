package handlers

import (
	"context"
	"errors"
	"net/http"
	"sync"
	"time"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/clienterror"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/logging"
	coreexecutor "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/pluginapi"
	"github.com/google/uuid"
)

type requestLifecycleHost interface {
	CompleteRequest(context.Context, pluginapi.RequestCompletion)
}

type requestLifecycleSkipHost interface {
	CompleteRequestExcept(context.Context, pluginapi.RequestCompletion, string)
}

type requestLifecycleTracker struct {
	once         sync.Once
	ctx          context.Context
	host         PluginInterceptorHost
	skipPluginID string
	completion   pluginapi.RequestCompletion
}

func newExecutionRequestID() string {
	return uuid.NewString()
}

func (h *BaseAPIHandler) newRequestLifecycleTracker(
	ctx context.Context,
	requestID,
	sourceFormat,
	model,
	requestedModel string,
	stream bool,
	metadata map[string]any,
	skipPluginID string,
) *requestLifecycleTracker {
	if requestID == "" {
		requestID = newExecutionRequestID()
	}
	return &requestLifecycleTracker{
		ctx:          ctx,
		host:         h.interceptorHost(),
		skipPluginID: skipPluginID,
		completion: pluginapi.RequestCompletion{
			RequestID:      requestID,
			TraceID:        logging.GetRequestID(ctx),
			SourceFormat:   sourceFormat,
			Model:          model,
			RequestedModel: requestedModel,
			Stream:         stream,
			StartedAt:      time.Now(),
			Metadata:       metadata,
		},
	}
}

func (t *requestLifecycleTracker) requestID() string {
	if t == nil {
		return ""
	}
	return t.completion.RequestID
}

func (t *requestLifecycleTracker) traceID() string {
	if t == nil {
		return ""
	}
	return t.completion.TraceID
}

func (t *requestLifecycleTracker) complete(
	outcome pluginapi.RequestCompletionOutcome,
	statusCode int,
	err error,
) {
	if t == nil {
		return
	}
	t.once.Do(
		func() {
			completion := t.completion
			completion.Metadata = pluginExecutionMetadata(completion.Metadata)
			completion.Outcome = outcome
			completion.StatusCode = statusCode
			completion.CompletedAt = time.Now()
			if err != nil {
				completion.Error = err.Error()
			}
			if t.skipPluginID != "" {
				if host, ok := t.host.(requestLifecycleSkipHost); ok {
					host.CompleteRequestExcept(t.ctx, completion, t.skipPluginID)
					return
				}
			}
			if host, ok := t.host.(requestLifecycleHost); ok {
				host.CompleteRequest(t.ctx, completion)
			}
		},
	)
}

func (t *requestLifecycleTracker) completeError(ctx context.Context, msg *ErrorMessage) {
	outcome := pluginapi.RequestCompletionFailed
	if msg != nil && msg.DirectResponse {
		outcome = pluginapi.RequestCompletionRejected
	} else if ctx != nil && ctx.Err() != nil {
		outcome = pluginapi.RequestCompletionCanceled
	}
	statusCode := 0
	var err error
	if msg != nil {
		statusCode = msg.StatusCode
		err = msg.Error
	}
	if outcome == pluginapi.RequestCompletionCanceled {
		statusCode = 0
	}
	t.complete(outcome, statusCode, err)
}

func normalizedTerminationStatus(statusCode int) int {
	if statusCode < http.StatusOK || statusCode > 599 {
		return http.StatusForbidden
	}
	return statusCode
}

func requestTerminationError(resp pluginapi.RequestInterceptResponse) *ErrorMessage {
	return directTerminationError(resp.StatusCode, resp.ResponseHeaders, resp.ResponseBody, nil)
}

func directTerminationError(statusCode int, headers http.Header, body []byte, err error) *ErrorMessage {
	return &ErrorMessage{
		StatusCode:     normalizedTerminationStatus(statusCode),
		Error:          err,
		DirectResponse: true,
		Body:           cloneBytes(body),
		Headers:        cloneHeader(headers),
	}
}

func isRequestTerminatedExecutionError(err error) bool {
	var terminated *coreexecutor.RequestTerminatedError
	return errors.As(err, &terminated) && terminated != nil
}

func executionErrorMessage(err error) *ErrorMessage {
	if err == nil {
		return nil
	}
	var terminated *coreexecutor.RequestTerminatedError
	if errors.As(err, &terminated) && terminated != nil {
		return directTerminationError(
			terminated.StatusCode(),
			terminated.ResponseHeaders(),
			terminated.ResponseBody(),
			err,
		)
	}
	status := clienterror.HTTPStatusFromErrorOr(err, http.StatusInternalServerError)
	var addon http.Header
	if headerError, ok := err.(interface{ Headers() http.Header }); ok && headerError != nil {
		if headers := headerError.Headers(); headers != nil {
			addon = headers.Clone()
		}
	}
	return &ErrorMessage{StatusCode: status, Error: err, Addon: addon}
}
