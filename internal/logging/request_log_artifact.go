package logging

import (
	"fmt"
	"strings"
	"sync/atomic"
	"time"
)

// RequestLogState is the persisted lifecycle state of one request-log artifact.
type RequestLogState string

const (
	RequestLogPending       RequestLogState = "pending"
	RequestLogAvailable     RequestLogState = "available"
	RequestLogDisabled      RequestLogState = "disabled"
	RequestLogNotCaptured   RequestLogState = "not_captured"
	RequestLogPublishFailed RequestLogState = "publish_failed"
	RequestLogExpired       RequestLogState = "expired"
	RequestLogLegacyUnknown RequestLogState = "legacy_unknown"
)

// RequestLogLocation identifies where an available request log is stored.
type RequestLogLocation string

const (
	RequestLogLocationLocal RequestLogLocation = "local"
	RequestLogLocationHome  RequestLogLocation = "home"
)

// RequestLogArtifact is the indexed lifecycle record for one request ID.
type RequestLogArtifact struct {
	RequestID string
	State     RequestLogState
	Location  RequestLogLocation
	FileName  string
	HomeIP    string
	Reason    string
	Revision  int64
	UpdatedAt time.Time
}

// RequestLogArtifactSink persists request-log artifact lifecycle records.
type RequestLogArtifactSink interface {
	SubmitRequestLogArtifact(RequestLogArtifact) bool
}

// RequestLogArtifactReporter prepares and forwards lifecycle records to a sink.
type RequestLogArtifactReporter interface {
	RecordRequestLogArtifact(RequestLogArtifact) bool
}

var requestLogArtifactRevision atomic.Int64

// PrepareRequestLogArtifact normalizes an artifact and assigns a monotonic revision.
func PrepareRequestLogArtifact(artifact RequestLogArtifact) RequestLogArtifact {
	artifact.RequestID = strings.TrimSpace(artifact.RequestID)
	artifact.FileName = strings.TrimSpace(artifact.FileName)
	artifact.HomeIP = strings.TrimSpace(artifact.HomeIP)
	artifact.Reason = strings.TrimSpace(artifact.Reason)
	if artifact.UpdatedAt.IsZero() {
		artifact.UpdatedAt = time.Now().UTC()
	} else {
		artifact.UpdatedAt = artifact.UpdatedAt.UTC()
	}
	if artifact.Revision <= 0 {
		artifact.Revision = nextRequestLogArtifactRevision(artifact.UpdatedAt.UnixNano())
	}
	return artifact
}

// ValidateRequestLogArtifact checks the storage contract shared by loggers and SQLite.
func ValidateRequestLogArtifact(artifact RequestLogArtifact) error {
	if artifact.RequestID == "" || artifact.RequestID != strings.TrimSpace(artifact.RequestID) {
		return fmt.Errorf("request log artifact request ID is required")
	}
	switch artifact.State {
	case RequestLogPending,
		RequestLogAvailable,
		RequestLogDisabled,
		RequestLogNotCaptured,
		RequestLogPublishFailed,
		RequestLogExpired,
		RequestLogLegacyUnknown:
	default:
		return fmt.Errorf("invalid request log artifact state %q", artifact.State)
	}
	switch artifact.Location {
	case "", RequestLogLocationLocal, RequestLogLocationHome:
	default:
		return fmt.Errorf("invalid request log artifact location %q", artifact.Location)
	}
	if artifact.State == RequestLogAvailable && artifact.Location == "" {
		return fmt.Errorf("available request log artifact requires a location")
	}
	if artifact.Location == RequestLogLocationLocal && artifact.State == RequestLogAvailable && artifact.FileName == "" {
		return fmt.Errorf("available local request log artifact requires a file name")
	}
	if artifact.Revision <= 0 {
		return fmt.Errorf("request log artifact revision must be positive")
	}
	if artifact.UpdatedAt.IsZero() {
		return fmt.Errorf("request log artifact update time is required")
	}
	return nil
}

func nextRequestLogArtifactRevision(now int64) int64 {
	for {
		previous := requestLogArtifactRevision.Load()
		next := now
		if next <= previous {
			next = previous + 1
		}
		if requestLogArtifactRevision.CompareAndSwap(previous, next) {
			return next
		}
	}
}

// AdvanceRequestLogArtifactRevision prevents a restarted process or a clock
// rollback from generating revisions older than lifecycle rows already stored.
func AdvanceRequestLogArtifactRevision(revision int64) {
	if revision <= 0 {
		return
	}
	for {
		previous := requestLogArtifactRevision.Load()
		if revision <= previous || requestLogArtifactRevision.CompareAndSwap(previous, revision) {
			return
		}
	}
}

// SetRequestLogArtifactSink updates the lifecycle sink used by this logger.
func (l *FileRequestLogger) SetRequestLogArtifactSink(sink RequestLogArtifactSink) {
	if l == nil {
		return
	}
	l.configMu.Lock()
	l.artifactSink = sink
	l.configMu.Unlock()
}

// RecordRequestLogArtifact prepares and forwards one lifecycle record.
func (l *FileRequestLogger) RecordRequestLogArtifact(artifact RequestLogArtifact) bool {
	if l == nil {
		return false
	}
	artifact = PrepareRequestLogArtifact(artifact)
	if ValidateRequestLogArtifact(artifact) != nil {
		return false
	}
	l.configMu.RLock()
	sink := l.artifactSink
	l.configMu.RUnlock()
	return sink != nil && sink.SubmitRequestLogArtifact(artifact)
}

func (s *fileRequestLoggerSnapshot) RecordRequestLogArtifact(artifact RequestLogArtifact) bool {
	return s != nil && s.logger != nil && s.logger.RecordRequestLogArtifact(artifact)
}

func (l *FileRequestLogger) recordRequestLogAvailable(
	requestID string,
	location RequestLogLocation,
	fileName string,
) {
	if strings.TrimSpace(requestID) == "" {
		return
	}
	l.RecordRequestLogArtifact(
		RequestLogArtifact{
			RequestID: requestID,
			State:     RequestLogAvailable,
			Location:  location,
			FileName:  fileName,
		},
	)
}

func (l *FileRequestLogger) recordRequestLogPublishFailed(requestID string, err error) {
	if strings.TrimSpace(requestID) == "" || err == nil {
		return
	}
	l.RecordRequestLogArtifact(
		RequestLogArtifact{
			RequestID: requestID,
			State:     RequestLogPublishFailed,
			Reason:    err.Error(),
		},
	)
}

func requestIDFromRequestLogFileName(fileName string) string {
	base := strings.TrimSuffix(strings.TrimSpace(fileName), ".log")
	separator := strings.LastIndexByte(base, '-')
	if separator < 0 || separator == len(base)-1 {
		return ""
	}
	return strings.TrimSpace(base[separator+1:])
}
