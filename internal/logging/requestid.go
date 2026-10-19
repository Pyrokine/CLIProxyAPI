package logging

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"io"
	"os"
	"strconv"
	"sync/atomic"
	"time"

	"github.com/gin-gonic/gin"
	log "github.com/sirupsen/logrus"
)

// requestIDKey is the context key for storing/retrieving request IDs.
type requestIDKey struct{}

// ginRequestIDKey is the Gin context key for request IDs.
const ginRequestIDKey = "__request_id__"

var readRequestIDRandom = rand.Read
var requestIDFallbackCounter atomic.Uint64

var requestIDFallbackPrefix = func() [8]byte {
	seed := strconv.FormatInt(time.Now().UnixNano(), 10) + ":" + strconv.Itoa(os.Getpid())
	digest := sha256.Sum256([]byte(seed))
	var prefix [8]byte
	copy(prefix[:], digest[:len(prefix)])
	return prefix
}()

// generateRequestID creates a new 128-bit hexadecimal request ID.
func generateRequestID() string {
	var id [16]byte
	n, err := readRequestIDRandom(id[:])
	if err == nil && n == len(id) {
		return hex.EncodeToString(id[:])
	}
	if err == nil {
		err = io.ErrUnexpectedEOF
	}
	log.WithError(err).Warn("request ID random source failed; using process-scoped fallback")

	copy(id[:8], requestIDFallbackPrefix[:])
	binary.BigEndian.PutUint64(id[8:], requestIDFallbackCounter.Add(1))
	return hex.EncodeToString(id[:])
}

// WithRequestID returns a new context with the request ID attached.
func WithRequestID(ctx context.Context, requestID string) context.Context {
	return context.WithValue(ctx, requestIDKey{}, requestID)
}

// GetRequestID retrieves the request ID from the context.
// Returns empty string if not found.
func GetRequestID(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	if id, ok := ctx.Value(requestIDKey{}).(string); ok {
		return id
	}
	return ""
}

// setGinRequestID stores the request ID in the Gin context.
func setGinRequestID(c *gin.Context, requestID string) {
	if c != nil {
		c.Set(ginRequestIDKey, requestID)
	}
}

// GetGinRequestID retrieves the request ID from the Gin context.
func GetGinRequestID(c *gin.Context) string {
	if c == nil {
		return ""
	}
	if id, exists := c.Get(ginRequestIDKey); exists {
		if s, ok := id.(string); ok {
			return s
		}
	}
	return ""
}

// EnsureGinRequestID returns the current request ID or creates one for this Gin request.
func EnsureGinRequestID(c *gin.Context) string {
	if c == nil {
		return ""
	}
	if requestID := GetGinRequestID(c); requestID != "" {
		return requestID
	}
	if c.Request != nil {
		if requestID := GetRequestID(c.Request.Context()); requestID != "" {
			setGinRequestID(c, requestID)
			return requestID
		}
	}
	requestID := generateRequestID()
	setGinRequestID(c, requestID)
	if c.Request != nil {
		c.Request = c.Request.WithContext(WithRequestID(c.Request.Context(), requestID))
	}
	return requestID
}
