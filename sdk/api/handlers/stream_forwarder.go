package handlers

import (
	"errors"
	"io"
	"net/http"
	"slices"
	"time"

	internallogging "github.com/Pyrokine/CLIProxyAPI/v6/internal/logging"
	coreexecutor "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
	"github.com/gin-gonic/gin"
)

var (
	errStreamClosedBeforeTransportTerminal = errors.New("stream closed before transport terminal")
	errStreamTransportTerminalTimeout      = errors.New("timed out waiting for stream transport terminal")
	errStreamChannelCloseTimeout           = errors.New("timed out waiting for stream channel close after transport terminal")
	errStreamChunkAfterProtocolTerminal    = errors.New("stream chunk received after protocol terminal")
	errStreamErrorAfterProtocolTerminal    = errors.New("stream error received after protocol terminal")
	errStreamChunkAfterTransportTerminal   = errors.New("stream chunk received after terminal")
	errStreamErrorAfterTransportTerminal   = errors.New("stream error received after terminal")
)

func MarkStreamRequestFailure(c *gin.Context, err error) {
	if c == nil || c.Request == nil {
		return
	}
	reason := ""
	if err != nil {
		reason = err.Error()
	}
	internallogging.MarkRequestOutcomeFailure(
		c.Request.Context(),
		internallogging.RequestOutcomeStreamError,
		"stream",
		reason,
	)
}

type KeepAliveWriteResult struct {
	Written    bool
	Bytes      int
	Err        error
	Suppressed bool
}

type StreamForwardOptions struct {
	// KeepAliveInterval overrides the configured streaming keep-alive interval.
	// If nil, the configured default is used. If set to <= 0, keep-alives are disabled.
	KeepAliveInterval *time.Duration

	// WriteChunk writes a single data chunk to the response body. It should not flush.
	WriteChunk func(chunk []byte)

	// WriteTerminalError writes an error payload to the response body when streaming fails
	// after headers have already been committed. It should not flush.
	WriteTerminalError func(errMsg *ErrorMessage)

	// WriteDone optionally writes a terminal marker when the upstream data channel closes
	// without an error (e.g. OpenAI's `[DONE]`). It should not flush.
	WriteDone func()

	// DoneError reports an error discovered while finalizing a cleanly closed data stream.
	// WriteDone should write the matching terminal error payload before returning it here.
	DoneError func() error

	// WriteKeepAlive optionally writes a keep-alive heartbeat. It should not flush.
	// When nil, a standard SSE comment heartbeat is used.
	WriteKeepAlive func() KeepAliveWriteResult

	// OnKeepAliveFlush runs after a heartbeat is written and flushed.
	OnKeepAliveFlush func()
}

func (h *BaseAPIHandler) ForwardStream(
	c *gin.Context,
	flusher http.Flusher,
	cancel func(error),
	data <-chan []byte,
	errs <-chan *ErrorMessage,
	opts StreamForwardOptions,
) {
	if c == nil {
		return
	}
	if cancel == nil {
		return
	}

	writeChunk := opts.WriteChunk
	if writeChunk == nil {
		writeChunk = func([]byte) {}
	}

	writeKeepAlive := opts.WriteKeepAlive
	if writeKeepAlive == nil {
		writeKeepAlive = func() KeepAliveWriteResult {
			n, err := c.Writer.Write([]byte(": keep-alive\n\n"))
			return KeepAliveWriteResult{Written: n > 0, Bytes: n, Err: err}
		}
	}

	keepAliveInterval := StreamingKeepAliveInterval(h.ConfigSnapshot())
	if opts.KeepAliveInterval != nil {
		keepAliveInterval = *opts.KeepAliveInterval
	}
	var keepAlive *time.Ticker
	var keepAliveC <-chan time.Time
	if keepAliveInterval > 0 {
		keepAlive = time.NewTicker(keepAliveInterval)
		defer keepAlive.Stop()
		keepAliveC = keepAlive.C
	}

	var terminalErr *ErrorMessage
	for {
		select {
		case <-c.Request.Context().Done():
			cancel(c.Request.Context().Err())
			return
		case chunk, ok := <-data:
			if !ok {
				// Prefer surfacing a terminal error if one is pending.
				if terminalErr == nil {
					select {
					case errMsg, ok := <-errs:
						if ok && errMsg != nil {
							terminalErr = errMsg
						}
					default:
					}
				}
				if terminalErr != nil {
					MarkStreamRequestFailure(c, terminalErr.Error)
					if opts.WriteTerminalError != nil {
						opts.WriteTerminalError(terminalErr)
					}
					flusher.Flush()
					cancel(terminalErr.Error)
					return
				}
				if opts.WriteDone != nil {
					opts.WriteDone()
				}
				if opts.DoneError != nil {
					if err := opts.DoneError(); err != nil {
						MarkStreamRequestFailure(c, err)
						flusher.Flush()
						cancel(err)
						return
					}
				}
				flusher.Flush()
				cancel(nil)
				return
			}
			writeChunk(chunk)
			flusher.Flush()
		case errMsg, ok := <-errs:
			if !ok {
				errs = nil
				continue
			}
			if errMsg != nil {
				terminalErr = errMsg
				MarkStreamRequestFailure(c, errMsg.Error)
				if opts.WriteTerminalError != nil {
					opts.WriteTerminalError(errMsg)
					flusher.Flush()
				}
			}
			var execErr error
			if errMsg != nil {
				execErr = errMsg.Error
			}
			cancel(execErr)
			return
		case <-keepAliveC:
			result := writeKeepAlive()
			if result.Err != nil {
				MarkStreamRequestFailure(c, result.Err)
				cancel(result.Err)
				return
			}
			if result.Written {
				flusher.Flush()
				if opts.OnKeepAliveFlush != nil {
					opts.OnKeepAliveFlush()
				}
			}
		}
	}
}

// StreamChunkForwardOptions configures delivery-aware stream forwarding.
type StreamChunkForwardOptions struct {
	KeepAliveInterval  *time.Duration
	DeliveryTimeout    *time.Duration
	RequireTerminal    bool
	ProtocolTerminal   func() bool
	WriteChunk         func(chunk []byte) error
	WriteTerminalError func(errMsg *ErrorMessage) error
	WriteDone          func() error
	DoneError          func() error
	WriteKeepAlive     func() KeepAliveWriteResult
	OnKeepAliveFlush   func()
}

// ForwardStreamChunks acknowledges each chunk only after its final downstream write and flush.
func (h *BaseAPIHandler) ForwardStreamChunks(
	c *gin.Context,
	flusher http.Flusher,
	cancel func(error),
	data <-chan coreexecutor.StreamChunk,
	errs <-chan *ErrorMessage,
	opts StreamChunkForwardOptions,
	initial ...coreexecutor.StreamChunk,
) {
	if c == nil || cancel == nil {
		return
	}

	writeChunk := opts.WriteChunk
	if writeChunk == nil {
		writeChunk = func([]byte) error { return nil }
	}
	writeKeepAlive := opts.WriteKeepAlive
	if writeKeepAlive == nil {
		writeKeepAlive = func() KeepAliveWriteResult {
			n, err := c.Writer.Write([]byte(": keep-alive\n\n"))
			if err == nil && n != len(": keep-alive\n\n") {
				err = io.ErrShortWrite
			}
			return KeepAliveWriteResult{Written: n > 0, Bytes: n, Err: err}
		}
	}

	keepAliveInterval := StreamingKeepAliveInterval(h.ConfigSnapshot())
	if opts.KeepAliveInterval != nil {
		keepAliveInterval = *opts.KeepAliveInterval
	}
	deliveryTimeout := defaultStreamDeliveryTimeout
	if opts.DeliveryTimeout != nil {
		deliveryTimeout = *opts.DeliveryTimeout
	}
	if deliveryTimeout <= 0 {
		deliveryTimeout = defaultStreamDeliveryTimeout
	}
	var keepAlive *time.Ticker
	var keepAliveC <-chan time.Time
	if keepAliveInterval > 0 {
		keepAlive = time.NewTicker(keepAliveInterval)
		defer keepAlive.Stop()
		keepAliveC = keepAlive.C
	}

	fail := func(err error) {
		MarkStreamRequestFailure(c, err)
		cancel(err)
	}
	deliver := func(write func() error) error {
		return deliverStreamResponseWithTimeout(c.Writer, flusher, deliveryTimeout, write)
	}
	deliverTerminal := func() error {
		return withStreamWriteDeadline(
			c.Writer, deliveryTimeout, func() error {
				var errDelivery error
				if opts.WriteDone != nil {
					errDelivery = opts.WriteDone()
				}
				if opts.DoneError != nil {
					errDelivery = errors.Join(errDelivery, opts.DoneError())
				}
				errDelivery = errors.Join(errDelivery, FlushStreamResponse(c.Writer, flusher))
				return errDelivery
			},
		)
	}
	acknowledgeChunk := func(chunk coreexecutor.StreamChunk, err error) {
		if chunk.Acknowledge != nil {
			chunk.Acknowledge(err)
		}
	}
	postTerminalError := func(base, upstream error) error {
		if upstream == nil {
			return base
		}
		return errors.Join(base, upstream)
	}
	finishSuccess := func() {
		if data == nil {
			cancel(nil)
			return
		}
		timer := time.NewTimer(deliveryTimeout)
		defer timer.Stop()
		for {
			select {
			case <-c.Request.Context().Done():
				fail(c.Request.Context().Err())
				return
			case chunk, ok := <-data:
				if !ok {
					if errs != nil {
						select {
						case errMsg, okErr := <-errs:
							if okErr && errMsg != nil {
								err := postTerminalError(errStreamErrorAfterTransportTerminal, errMsg.Error)
								AcknowledgeStreamError(errMsg, err)
								fail(err)
								return
							}
						default:
						}
					}
					cancel(nil)
					return
				}
				if len(chunk.Payload) == 0 && chunk.Err == nil && !chunk.Terminal {
					acknowledgeChunk(chunk, nil)
					continue
				}
				err := errStreamChunkAfterTransportTerminal
				if chunk.Err != nil {
					err = postTerminalError(errStreamErrorAfterTransportTerminal, chunk.Err)
				}
				acknowledgeChunk(chunk, err)
				fail(err)
				return
			case errMsg, ok := <-errs:
				if !ok {
					errs = nil
					continue
				}
				if errMsg == nil {
					continue
				}
				err := postTerminalError(errStreamErrorAfterTransportTerminal, errMsg.Error)
				AcknowledgeStreamError(errMsg, err)
				fail(err)
				return
			case <-timer.C:
				fail(errStreamChannelCloseTimeout)
				return
			}
		}
	}
	forwardTerminalError := func(errMsg *ErrorMessage) {
		if errMsg == nil {
			cancel(nil)
			return
		}
		MarkStreamRequestFailure(c, errMsg.Error)
		if !c.Writer.Written() {
			status := http.StatusInternalServerError
			if errMsg.StatusCode > 0 {
				status = errMsg.StatusCode
			}
			c.Status(status)
		}
		errDelivery := deliver(
			func() error {
				if opts.WriteTerminalError == nil {
					return nil
				}
				return opts.WriteTerminalError(errMsg)
			},
		)
		AcknowledgeStreamError(errMsg, errDelivery)
		if errDelivery != nil {
			fail(errDelivery)
			return
		}
		cancel(errMsg.Error)
	}
	waitForTransportTerminal := func() {
		timer := time.NewTimer(deliveryTimeout)
		defer timer.Stop()
		for {
			select {
			case <-c.Request.Context().Done():
				fail(c.Request.Context().Err())
				return
			case chunk, ok := <-data:
				if !ok {
					if errs != nil {
						select {
						case errMsg, okErr := <-errs:
							if okErr && errMsg != nil {
								err := postTerminalError(errStreamErrorAfterProtocolTerminal, errMsg.Error)
								AcknowledgeStreamError(errMsg, err)
								fail(err)
								return
							}
						default:
						}
					}
					fail(errStreamClosedBeforeTransportTerminal)
					return
				}
				if chunk.Terminal {
					errDelivery := deliverTerminal()
					acknowledgeChunk(chunk, errDelivery)
					if errDelivery != nil {
						fail(errDelivery)
					} else {
						finishSuccess()
					}
					return
				}
				if len(chunk.Payload) == 0 && chunk.Err == nil {
					acknowledgeChunk(chunk, nil)
					continue
				}
				err := errStreamChunkAfterProtocolTerminal
				if chunk.Err != nil {
					err = postTerminalError(errStreamErrorAfterProtocolTerminal, chunk.Err)
				}
				acknowledgeChunk(chunk, err)
				fail(err)
				return
			case errMsg, ok := <-errs:
				if !ok {
					errs = nil
					continue
				}
				if errMsg == nil {
					continue
				}
				err := postTerminalError(errStreamErrorAfterProtocolTerminal, errMsg.Error)
				AcknowledgeStreamError(errMsg, err)
				fail(err)
				return
			case <-timer.C:
				fail(errStreamTransportTerminalTimeout)
				return
			}
		}
	}
	forwardChunk := func(chunk coreexecutor.StreamChunk) bool {
		if chunk.Err != nil {
			forwardTerminalError(
				&ErrorMessage{
					StatusCode:  http.StatusInternalServerError,
					Error:       chunk.Err,
					Acknowledge: chunk.Acknowledge,
				},
			)
			return true
		}
		if chunk.Terminal {
			errDelivery := deliverTerminal()
			acknowledgeChunk(chunk, errDelivery)
			if errDelivery != nil {
				fail(errDelivery)
			} else {
				finishSuccess()
			}
			return true
		}
		if len(chunk.Payload) == 0 {
			acknowledgeChunk(chunk, nil)
			return false
		}
		errDelivery := deliver(
			func() error {
				return writeChunk(chunk.Payload)
			},
		)
		acknowledgeChunk(chunk, errDelivery)
		if errDelivery != nil {
			fail(errDelivery)
			return true
		}
		if opts.ProtocolTerminal != nil && opts.ProtocolTerminal() {
			waitForTransportTerminal()
			return true
		}
		return false
	}
	if slices.ContainsFunc(initial, forwardChunk) {
		return
	}

	var terminalErr *ErrorMessage
	for {
		select {
		case <-c.Request.Context().Done():
			fail(c.Request.Context().Err())
			return
		case chunk, ok := <-data:
			if !ok {
				if terminalErr == nil {
					select {
					case errMsg, ok := <-errs:
						if ok && errMsg != nil {
							terminalErr = errMsg
						}
					default:
					}
				}
				if terminalErr != nil {
					forwardTerminalError(terminalErr)
					return
				}
				if opts.RequireTerminal {
					forwardTerminalError(
						&ErrorMessage{
							StatusCode: http.StatusBadGateway,
							Error:      errStreamClosedBeforeTransportTerminal,
						},
					)
					return
				}
				errDelivery := deliverTerminal()
				if errDelivery != nil {
					fail(errDelivery)
				} else {
					cancel(nil)
				}
				return
			}
			if forwardChunk(chunk) {
				return
			}
		case errMsg, ok := <-errs:
			if !ok {
				errs = nil
				continue
			}
			if errMsg != nil {
				terminalErr = errMsg
			}
			forwardTerminalError(errMsg)
			return
		case <-keepAliveC:
			var result KeepAliveWriteResult
			errDelivery := deliver(
				func() error {
					result = writeKeepAlive()
					return result.Err
				},
			)
			if errDelivery != nil {
				fail(errDelivery)
				return
			}
			if result.Written && opts.OnKeepAliveFlush != nil {
				opts.OnKeepAliveFlush()
			}
		}
	}
}
