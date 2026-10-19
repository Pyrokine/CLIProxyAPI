// Package logging provides request logging functionality for the CLI Proxy API server.
// It handles capturing and storing detailed HTTP request and response data when enabled
// through configuration, supporting both regular and streaming responses.
package logging

import (
	"bytes"
	"compress/flate"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/andybalholm/brotli"
	"github.com/klauspost/compress/zstd"
	log "github.com/sirupsen/logrus"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/buildinfo"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/home"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/interfaces"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/util"
)

var requestLogID atomic.Uint64
var removeRequestLogPath = os.Remove
var pendingFileBodySourceCleanup FileBodySourceCleanupQueue

type activeRequestLogLeaseIdentity struct {
	info  os.FileInfo
	count int
}

var requestLogLeaseAcquireMu sync.Mutex
var activeRequestLogLeases = struct {
	sync.Mutex
	paths      map[string]int
	identities []activeRequestLogLeaseIdentity
}{}

var errStreamingLogWriterClosed = errors.New("streaming log writer is closed")
var errRequestLogCaptureLimit = errors.New("request log capture limit exceeded")
var truncatedFileBodySourceLog = []byte("[payload omitted: exceeds safe redaction limit]")

const (
	maxMaskedPayloadBytes          = 8 << 20
	maxStreamingResponseBodyBytes  = maxMaskedPayloadBytes
	maxPendingRequestLogTempFiles  = 128
	maxPendingFileBodySourceCount  = 128
	requestLogCleanupRetryInterval = 30 * time.Second
	requestLogPendingLeaseStaleAge = time.Hour
	requestLogCleanupScanBatchSize = 256
	legacySDKRequestLogTempDirName = "cliproxyapi-request-log"
	fileBodySourceOwnerLeaseName   = ".owner.lock"
	responseBodyLeaseSuffix        = ".lock"

	WebsocketTimelineSourceContextKey    = "WEBSOCKET_TIMELINE_SOURCE"
	APIRequestSourceContextKey           = "API_REQUEST_SOURCE"
	DeferredAPIRequestContextKey         = "DEFERRED_API_REQUEST"
	APIResponseSourceContextKey          = "API_RESPONSE_SOURCE"
	APIResponseCapturedContextKey        = "API_RESPONSE_CAPTURED"
	APIWebsocketTimelineSourceContextKey = "API_WEBSOCKET_TIMELINE_SOURCE"
	FileBodySourceCleanupQueueContextKey = "FILE_BODY_SOURCE_CLEANUP_QUEUE"
)

// DeferredAPIRequest builds an upstream request log only when an error log needs it.
type DeferredAPIRequest func() []byte

type requestLogCaptureWriter struct {
	writer    io.Writer
	remaining int64
}

func (w *requestLogCaptureWriter) Write(data []byte) (int, error) {
	if int64(len(data)) > w.remaining {
		return 0, errRequestLogCaptureLimit
	}
	n, errWrite := w.writer.Write(data)
	w.remaining -= int64(n)
	return n, errWrite
}

type homeRequestLogClient interface {
	HeartbeatOK() bool
	RPushRequestLog(ctx context.Context, payload []byte) error
}

var currentHomeRequestLogClient = func() homeRequestLogClient {
	return home.Current()
}

type requestLogFileLease struct {
	file        *os.File
	path        string
	stagingPath string
	identity    os.FileInfo
}

func registerActiveRequestLogLease(path string, identity os.FileInfo) {
	activeRequestLogLeases.Lock()
	if path != "" {
		path = filepath.Clean(path)
		if activeRequestLogLeases.paths == nil {
			activeRequestLogLeases.paths = make(map[string]int)
		}
		activeRequestLogLeases.paths[path]++
	}
	if identity != nil {
		for index := range activeRequestLogLeases.identities {
			entry := &activeRequestLogLeases.identities[index]
			if os.SameFile(entry.info, identity) {
				entry.count++
				activeRequestLogLeases.Unlock()
				return
			}
		}
		activeRequestLogLeases.identities = append(
			activeRequestLogLeases.identities,
			activeRequestLogLeaseIdentity{info: identity, count: 1},
		)
	}
	activeRequestLogLeases.Unlock()
}

func unregisterActiveRequestLogLease(path string, identity os.FileInfo) {
	activeRequestLogLeases.Lock()
	if path != "" {
		path = filepath.Clean(path)
		if count := activeRequestLogLeases.paths[path]; count > 1 {
			activeRequestLogLeases.paths[path] = count - 1
		} else {
			delete(activeRequestLogLeases.paths, path)
		}
	}
	if identity != nil {
		for index := range activeRequestLogLeases.identities {
			entry := &activeRequestLogLeases.identities[index]
			if !os.SameFile(entry.info, identity) {
				continue
			}
			if entry.count > 1 {
				entry.count--
			} else {
				activeRequestLogLeases.identities = append(
					activeRequestLogLeases.identities[:index],
					activeRequestLogLeases.identities[index+1:]...,
				)
			}
			break
		}
	}
	activeRequestLogLeases.Unlock()
}

func isActiveRequestLogLease(path string, identity os.FileInfo) bool {
	activeRequestLogLeases.Lock()
	defer activeRequestLogLeases.Unlock()
	if path != "" && activeRequestLogLeases.paths[filepath.Clean(path)] > 0 {
		return true
	}
	if identity != nil {
		for _, entry := range activeRequestLogLeases.identities {
			if entry.count > 0 && os.SameFile(entry.info, identity) {
				return true
			}
		}
	}
	return false
}

func createRequestLogFileLease(path string) (*requestLogFileLease, error) {
	dir := filepath.Dir(path)
	base := filepath.Base(path)
	return createPublishedRequestLogLease(
		dir,
		"."+base+".pending-",
		func(string) string { return path },
	)
}

func createRequestLogTempLease(dir string, prefix string) (*requestLogFileLease, error) {
	return createPublishedRequestLogLease(
		dir,
		"."+prefix+"lease-pending-",
		func(pendingPath string) string {
			publishedName := strings.TrimPrefix(filepath.Base(pendingPath), ".") + responseBodyLeaseSuffix
			return filepath.Join(dir, publishedName)
		},
	)
}

func createPublishedRequestLogLease(
	dir string,
	pattern string,
	publishedPathFor func(string) string,
) (*requestLogFileLease, error) {
	requestLogLeaseAcquireMu.Lock()
	defer requestLogLeaseAcquireMu.Unlock()
	file, errCreate := os.CreateTemp(dir, pattern)
	if errCreate != nil {
		return nil, errCreate
	}
	pendingPath := file.Name()
	identity, errStat := file.Stat()
	if errStat != nil {
		return nil, errors.Join(errStat, file.Close(), os.Remove(pendingPath))
	}
	registerActiveRequestLogLease(pendingPath, identity)
	if errLock := lockRequestLogFile(file); errLock != nil {
		unregisterActiveRequestLogLease(pendingPath, identity)
		return nil, errors.Join(errLock, file.Close(), os.Remove(pendingPath))
	}
	publishedPath := publishedPathFor(pendingPath)
	registerActiveRequestLogLease(publishedPath, identity)
	if errPublish := os.Link(pendingPath, publishedPath); errPublish != nil {
		unregisterActiveRequestLogLease(publishedPath, identity)
		unregisterActiveRequestLogLease(pendingPath, identity)
		return nil, errors.Join(errPublish, unlockRequestLogFile(file), file.Close(), os.Remove(pendingPath))
	}
	lease := &requestLogFileLease{
		file:        file,
		path:        publishedPath,
		stagingPath: pendingPath,
		identity:    identity,
	}
	if errRemove := os.Remove(pendingPath); errRemove == nil {
		unregisterActiveRequestLogLease(pendingPath, identity)
		lease.stagingPath = ""
	}
	return lease, nil
}

func tryAcquireRequestLogFileLease(path string) (*requestLogFileLease, bool, error) {
	requestLogLeaseAcquireMu.Lock()
	defer requestLogLeaseAcquireMu.Unlock()
	identity, errStat := os.Stat(path)
	if errStat != nil {
		if os.IsNotExist(errStat) {
			return nil, false, nil
		}
		return nil, false, errStat
	}
	if isActiveRequestLogLease(path, identity) {
		return nil, true, nil
	}
	file, errOpen := os.OpenFile(path, os.O_RDWR, 0o600)
	if errOpen != nil {
		if os.IsNotExist(errOpen) {
			return nil, false, nil
		}
		return nil, false, errOpen
	}
	openedIdentity, errOpenedStat := file.Stat()
	if errOpenedStat != nil {
		return nil, false, errors.Join(errOpenedStat, file.Close())
	}
	acquired, errLock := tryLockRequestLogFile(file)
	if errLock != nil {
		return nil, false, errors.Join(errLock, file.Close())
	}
	if !acquired {
		return nil, true, file.Close()
	}
	registerActiveRequestLogLease(path, openedIdentity)
	return &requestLogFileLease{file: file, path: path, identity: openedIdentity}, false, nil
}

func (l *requestLogFileLease) release(remove bool) error {
	if l == nil {
		return nil
	}
	requestLogLeaseAcquireMu.Lock()
	defer requestLogLeaseAcquireMu.Unlock()
	var releaseErr error
	if l.file != nil {
		releaseErr = errors.Join(unlockRequestLogFile(l.file), l.file.Close())
		l.file = nil
	}
	if l.stagingPath != "" {
		stagingPath := l.stagingPath
		if errRemove := os.Remove(stagingPath); errRemove != nil && !os.IsNotExist(errRemove) {
			releaseErr = errors.Join(releaseErr, errRemove)
		} else {
			l.stagingPath = ""
		}
		unregisterActiveRequestLogLease(stagingPath, l.identity)
	}
	if remove && l.path != "" {
		if errRemove := os.Remove(l.path); errRemove != nil && !os.IsNotExist(errRemove) {
			releaseErr = errors.Join(releaseErr, errRemove)
		}
	}
	unregisterActiveRequestLogLease(l.path, l.identity)
	l.identity = nil
	return releaseErr
}

// FileBodySource stores large log sections as ordered temp-file parts.
type FileBodySource struct {
	mu                  sync.Mutex
	dir                 string
	paths               []string
	validSizes          []int64
	capturedBytes       int64
	truncated           bool
	ownerLease          *requestLogFileLease
	cleanupSlotReserved bool
	cleaned             bool
}

func NewFileBodySourceInDir(baseDir string, prefix string) (_ *FileBodySource, err error) {
	if errCleanup := RetryPendingFileBodySourceCleanup(); errCleanup != nil {
		log.WithError(errCleanup).Warn("failed to retry file body source cleanup before allocation")
	}
	if !pendingFileBodySourceCleanup.reserveSlot() {
		return nil, fmt.Errorf(
			"file body source cleanup capacity reached %d sources",
			maxPendingFileBodySourceCount,
		)
	}
	slotReserved := true
	defer func() {
		if err != nil && slotReserved {
			pendingFileBodySourceCleanup.releaseSlot(nil)
		}
	}()

	prefix = sanitizeTempPrefix(prefix)
	baseDir = strings.TrimSpace(baseDir)
	if baseDir == "" {
		return nil, fmt.Errorf("base directory is required")
	}
	if errMkdir := os.MkdirAll(baseDir, 0o755); errMkdir != nil {
		return nil, errMkdir
	}
	dir, errCreate := os.MkdirTemp(baseDir, "request-log-parts-"+prefix+"-*")
	if errCreate != nil {
		return nil, errCreate
	}
	ownerLease, errLease := createRequestLogFileLease(filepath.Join(dir, fileBodySourceOwnerLeaseName))
	if errLease != nil {
		return nil, errors.Join(errLease, os.Remove(dir))
	}
	slotReserved = false
	return &FileBodySource{
		dir:                 dir,
		ownerLease:          ownerLease,
		cleanupSlotReserved: true,
	}, nil
}

// NewLegacySDKFileBodySource creates a recoverable source for SDK loggers without source support.
func NewLegacySDKFileBodySource(prefix string) (*FileBodySource, error) {
	if errMkdir := os.MkdirAll(legacySDKRequestLogCleanup.logsDir, 0o700); errMkdir != nil {
		return nil, errMkdir
	}
	if errChmod := os.Chmod(legacySDKRequestLogCleanup.logsDir, 0o700); errChmod != nil {
		return nil, errChmod
	}
	if errCleanup := RetryLegacySDKRequestLogCleanup(); errCleanup != nil {
		log.WithError(errCleanup).Warn("failed to recover legacy SDK request log sources")
	}
	return NewFileBodySourceInDir(legacySDKRequestLogCleanup.logsDir, prefix)
}

// RetryLegacySDKRequestLogCleanup retries cleanup of crash-orphaned legacy SDK sources.
func RetryLegacySDKRequestLogCleanup() error {
	return legacySDKRequestLogCleanup.retryPendingCleanup(false)
}

func sanitizeTempPrefix(prefix string) string {
	prefix = strings.TrimSpace(prefix)
	if prefix == "" {
		return "log"
	}
	var builder strings.Builder
	for _, r := range prefix {
		switch {
		case r >= 'a' && r <= 'z':
			builder.WriteRune(r)
		case r >= 'A' && r <= 'Z':
			builder.WriteRune(r)
		case r >= '0' && r <= '9':
			builder.WriteRune(r)
		case r == '-' || r == '_':
			builder.WriteRune(r)
		default:
			builder.WriteByte('-')
		}
	}
	out := strings.Trim(builder.String(), "-_")
	if out == "" {
		return "log"
	}
	return out
}

func (s *FileBodySource) createPartLocked(prefix string) (*os.File, error) {
	if s.cleaned {
		return nil, fmt.Errorf("file body source has been cleaned")
	}
	prefix = sanitizeTempPrefix(prefix)
	if errMkdir := os.MkdirAll(s.dir, 0o755); errMkdir != nil {
		return nil, errMkdir
	}
	file, errCreate := os.CreateTemp(s.dir, prefix+"-*.tmp")
	if errCreate != nil {
		return nil, errCreate
	}
	s.paths = append(s.paths, file.Name())
	s.validSizes = append(s.validSizes, 0)
	return file, nil
}

func (s *FileBodySource) AppendPart(data []byte) error {
	return s.appendPart(data, writeLogPart)
}

func (s *FileBodySource) appendPart(
	data []byte,
	writePart func(io.Writer, []byte, bool) error,
) error {
	if s == nil {
		return fmt.Errorf("file body source is nil")
	}
	data = bytes.TrimSpace(data)
	if len(data) == 0 {
		return nil
	}
	if writePart == nil {
		return fmt.Errorf("file body part writer is nil")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.truncated {
		return nil
	}
	file, errCreate := s.createPartLocked("part")
	if errCreate != nil {
		return errCreate
	}
	partIndex := len(s.paths) - 1
	captureWriter := &requestLogCaptureWriter{
		writer:    file,
		remaining: int64(maxMaskedPayloadBytes) - s.capturedBytes,
	}
	writeErr := writePart(captureWriter, data, false)
	if errors.Is(writeErr, errRequestLogCaptureLimit) {
		s.truncated = true
		return errors.Join(file.Truncate(0), file.Close())
	}
	closeErr := file.Close()
	if errAppend := errors.Join(writeErr, closeErr); errAppend != nil {
		return errAppend
	}
	info, errInfo := os.Stat(s.paths[partIndex])
	if errInfo != nil {
		return errInfo
	}
	s.validSizes[partIndex] = info.Size()
	s.capturedBytes += info.Size()
	return nil
}

func (s *FileBodySource) AppendBytes(data []byte) error {
	return s.appendBytes(data, writeAll)
}

func (s *FileBodySource) appendBytes(
	data []byte,
	writeData func(io.Writer, []byte) error,
) error {
	if s == nil {
		return fmt.Errorf("file body source is nil")
	}
	if len(data) == 0 {
		return nil
	}
	if writeData == nil {
		return fmt.Errorf("file body byte writer is nil")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cleaned {
		return fmt.Errorf("file body source has been cleaned")
	}
	if s.truncated {
		return nil
	}
	if errMkdir := os.MkdirAll(s.dir, 0o755); errMkdir != nil {
		return errMkdir
	}

	var file *os.File
	var errOpen error
	partIndex := len(s.paths) - 1
	if partIndex < 0 {
		file, errOpen = os.CreateTemp(s.dir, "part-*.tmp")
		if errOpen == nil {
			s.paths = append(s.paths, file.Name())
			s.validSizes = append(s.validSizes, 0)
			partIndex = 0
		}
	} else {
		file, errOpen = os.OpenFile(s.paths[partIndex], os.O_CREATE|os.O_WRONLY, 0o600)
	}
	if errOpen != nil {
		return errOpen
	}
	validSize := s.validSizes[partIndex]
	if errTruncate := file.Truncate(validSize); errTruncate != nil {
		return errors.Join(errTruncate, file.Close())
	}
	if _, errSeek := file.Seek(validSize, io.SeekStart); errSeek != nil {
		return errors.Join(errSeek, file.Close())
	}
	captureWriter := &requestLogCaptureWriter{
		writer:    file,
		remaining: int64(maxMaskedPayloadBytes) - s.capturedBytes,
	}
	writeErr := writeData(captureWriter, data)
	if errors.Is(writeErr, errRequestLogCaptureLimit) {
		s.truncated = true
		return errors.Join(file.Truncate(validSize), file.Close())
	}
	closeErr := file.Close()
	if errAppend := errors.Join(writeErr, closeErr); errAppend != nil {
		return errAppend
	}
	info, errInfo := os.Stat(s.paths[partIndex])
	if errInfo != nil {
		return errInfo
	}
	writtenBytes := info.Size() - validSize
	s.validSizes[partIndex] = info.Size()
	s.capturedBytes += writtenBytes
	return nil
}

func (s *FileBodySource) HasPayload() bool {
	if s == nil {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cleaned {
		return false
	}
	if s.truncated {
		return true
	}
	for _, size := range s.validSizes {
		if size > 0 {
			return true
		}
	}
	return false
}

func (s *FileBodySource) Paths() []string {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]string, len(s.paths))
	copy(out, s.paths)
	return out
}

func (s *FileBodySource) WritePayloadTo(w io.Writer) error {
	if s == nil || w == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cleaned {
		return fmt.Errorf("file body source has been cleaned")
	}
	if s.truncated {
		_, errWrite := w.Write(truncatedFileBodySourceLog)
		return errWrite
	}

	var payload bytes.Buffer
	partsRead := 0
	for index, path := range s.paths {
		if index >= len(s.validSizes) || s.validSizes[index] <= 0 {
			continue
		}
		file, errOpen := os.Open(path)
		if errOpen != nil {
			return errOpen
		}
		if partsRead > 0 {
			payload.WriteByte('\n')
		}
		remaining := maxMaskedPayloadBytes + 1 - payload.Len()
		if remaining > 0 {
			partLimit := min(int64(remaining), s.validSizes[index])
			_, errCopy := io.Copy(&payload, io.LimitReader(file, partLimit))
			if errFile := errors.Join(errCopy, file.Close()); errFile != nil {
				return errFile
			}
		} else if errClose := file.Close(); errClose != nil {
			return errClose
		}
		partsRead++
		if payload.Len() > maxMaskedPayloadBytes {
			break
		}
	}
	if partsRead == 0 {
		return nil
	}
	if payload.Len() > maxMaskedPayloadBytes {
		_, errWrite := w.Write(truncatedFileBodySourceLog)
		return errWrite
	}
	return writeMaskedPayload(w, payload.Bytes())
}

func (s *FileBodySource) Bytes() ([]byte, error) {
	var buf bytes.Buffer
	if errWrite := s.WritePayloadTo(&buf); errWrite != nil {
		return nil, errWrite
	}
	return buf.Bytes(), nil
}

// ReadRawBytes returns a bounded unmasked byte stream for callers that must decode it before logging.
func (s *FileBodySource) ReadRawBytes(limit int64) ([]byte, bool, error) {
	if s == nil || limit <= 0 {
		return nil, false, nil
	}
	limit = min(limit, int64(maxMaskedPayloadBytes))
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cleaned {
		return nil, false, fmt.Errorf("file body source has been cleaned")
	}

	var payload bytes.Buffer
	truncated := s.truncated
	for index, path := range s.paths {
		if index >= len(s.validSizes) || s.validSizes[index] <= 0 {
			continue
		}
		remaining := limit + 1 - int64(payload.Len())
		if remaining <= 0 {
			truncated = true
			break
		}
		file, errOpen := os.Open(path)
		if errOpen != nil {
			return nil, truncated, errOpen
		}
		readLength := min(remaining, s.validSizes[index])
		copied, errCopy := io.Copy(&payload, io.LimitReader(file, readLength))
		if errCopy == nil && copied != readLength {
			errCopy = io.ErrUnexpectedEOF
		}
		if errFile := errors.Join(errCopy, file.Close()); errFile != nil {
			return nil, truncated, errFile
		}
		if readLength < s.validSizes[index] {
			truncated = true
			break
		}
	}
	if int64(payload.Len()) > limit {
		truncated = true
		return bytes.Clone(payload.Bytes()[:limit]), truncated, nil
	}
	return payload.Bytes(), truncated, nil
}

// MergeFileBodySource prepends a file-backed prefix and preserves the memory suffix when the prefix is unreadable.
func MergeFileBodySource(payload []byte, source *FileBodySource) ([]byte, error) {
	if source == nil || !source.HasPayload() {
		return payload, nil
	}
	var buf bytes.Buffer
	if errWrite := source.WritePayloadTo(&buf); errWrite != nil {
		buf.Reset()
		buf.WriteString("[source prefix unavailable: read failed]\n")
		buf.Write(payload)
		return buf.Bytes(), errWrite
	}
	buf.Write(payload)
	return buf.Bytes(), nil
}

func (s *FileBodySource) Cleanup() error {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	if s.cleaned {
		s.mu.Unlock()
		return nil
	}

	remainingPaths := make([]string, 0, len(s.paths))
	remainingSizes := make([]int64, 0, len(s.validSizes))
	var cleanupErr error
	for index, path := range s.paths {
		if errRemove := os.Remove(path); errRemove != nil && !os.IsNotExist(errRemove) {
			remainingPaths = append(remainingPaths, path)
			if index < len(s.validSizes) {
				remainingSizes = append(remainingSizes, s.validSizes[index])
			} else {
				remainingSizes = append(remainingSizes, 0)
			}
			cleanupErr = errors.Join(cleanupErr, errRemove)
		}
	}
	s.paths = remainingPaths
	s.validSizes = remainingSizes
	if cleanupErr != nil {
		s.mu.Unlock()
		return cleanupErr
	}

	ownerLease := s.ownerLease
	dir := s.dir
	if errInspect := fileBodySourceDirReadyForRemoval(dir, ownerLease); errInspect != nil {
		s.mu.Unlock()
		return errInspect
	}
	if errLease := ownerLease.release(true); errLease != nil {
		s.mu.Unlock()
		return errLease
	}
	s.ownerLease = nil
	if dir != "" {
		if errRemove := os.Remove(dir); errRemove != nil && !os.IsNotExist(errRemove) {
			restoredLease, errRestore := createRequestLogFileLease(
				filepath.Join(dir, fileBodySourceOwnerLeaseName),
			)
			if errRestore == nil {
				s.ownerLease = restoredLease
			}
			s.mu.Unlock()
			return errors.Join(errRemove, errRestore)
		}
	}
	s.paths = nil
	s.validSizes = nil
	s.capturedBytes = 0
	s.truncated = false
	s.dir = ""
	s.cleaned = true
	releaseSlot := s.cleanupSlotReserved
	s.cleanupSlotReserved = false
	s.mu.Unlock()
	if releaseSlot {
		pendingFileBodySourceCleanup.releaseSlot(s)
	}
	return nil
}

func fileBodySourceDirReadyForRemoval(dir string, ownerLease *requestLogFileLease) error {
	if dir == "" || ownerLease == nil {
		return nil
	}
	entries, errRead := os.ReadDir(dir)
	if errRead != nil {
		if os.IsNotExist(errRead) {
			return nil
		}
		return errRead
	}
	allowed := map[string]struct{}{
		filepath.Base(ownerLease.path): {},
	}
	if ownerLease.stagingPath != "" {
		allowed[filepath.Base(ownerLease.stagingPath)] = struct{}{}
	}
	for _, entry := range entries {
		if _, ok := allowed[entry.Name()]; !ok {
			return fmt.Errorf("file body source directory contains unexpected entry %q", entry.Name())
		}
	}
	return nil
}

func CleanupFileBodySources(sources ...*FileBodySource) error {
	var cleanupErr error
	for _, source := range sources {
		if source == nil {
			continue
		}
		if errCleanup := source.Cleanup(); errCleanup != nil {
			pendingFileBodySourceCleanup.Add(source)
			cleanupErr = errors.Join(cleanupErr, errCleanup)
		}
	}
	return cleanupErr
}

func cleanupFileBodySources(sources ...*FileBodySource) error {
	return CleanupFileBodySources(sources...)
}

type FileBodySourceCleanupQueue struct {
	mu                 sync.Mutex
	cleanupRunMu       sync.Mutex
	sources            map[*FileBodySource]struct{}
	reservedSlots      int
	nextCleanupAttempt time.Time
}

func (q *FileBodySourceCleanupQueue) reserveSlot() bool {
	if q == nil {
		return false
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.reservedSlots >= maxPendingFileBodySourceCount {
		return false
	}
	q.reservedSlots++
	return true
}

func (q *FileBodySourceCleanupQueue) releaseSlot(source *FileBodySource) {
	if q == nil {
		return
	}
	q.mu.Lock()
	if source != nil {
		delete(q.sources, source)
	}
	if q.reservedSlots > 0 {
		q.reservedSlots--
	}
	q.mu.Unlock()
}

func (q *FileBodySourceCleanupQueue) Add(source *FileBodySource) {
	if q == nil || source == nil {
		return
	}
	q.mu.Lock()
	if q.sources == nil {
		q.sources = make(map[*FileBodySource]struct{})
	}
	if _, exists := q.sources[source]; !exists {
		wasEmpty := len(q.sources) == 0
		q.sources[source] = struct{}{}
		if q == &pendingFileBodySourceCleanup && wasEmpty {
			q.nextCleanupAttempt = time.Time{}
		}
	}
	q.mu.Unlock()
}

func (q *FileBodySourceCleanupQueue) Cleanup() error {
	if q == nil {
		return nil
	}
	q.cleanupRunMu.Lock()
	defer q.cleanupRunMu.Unlock()
	return q.cleanupSources()
}

func (q *FileBodySourceCleanupQueue) cleanupSources() error {
	q.mu.Lock()
	sources := make([]*FileBodySource, 0, len(q.sources))
	for source := range q.sources {
		sources = append(sources, source)
	}
	q.mu.Unlock()

	var cleanupErr error
	for _, source := range sources {
		if errCleanup := source.Cleanup(); errCleanup != nil {
			cleanupErr = errors.Join(cleanupErr, errCleanup)
			continue
		}
		q.mu.Lock()
		delete(q.sources, source)
		q.mu.Unlock()
	}
	return cleanupErr
}

func (q *FileBodySourceCleanupQueue) sourceCount() int {
	if q == nil {
		return 0
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	return len(q.sources)
}

func (q *FileBodySourceCleanupQueue) CleanupAndRetainFailures() error {
	if q == nil {
		return nil
	}
	errCleanup := q.Cleanup()
	if errCleanup == nil || q == &pendingFileBodySourceCleanup {
		return errCleanup
	}

	q.mu.Lock()
	failedSources := make([]*FileBodySource, 0, len(q.sources))
	for source := range q.sources {
		failedSources = append(failedSources, source)
		delete(q.sources, source)
	}
	q.mu.Unlock()
	for _, source := range failedSources {
		pendingFileBodySourceCleanup.Add(source)
	}
	return errCleanup
}

func RetryPendingFileBodySourceCleanup() error {
	q := &pendingFileBodySourceCleanup
	now := time.Now()
	q.mu.Lock()
	if len(q.sources) == 0 || now.Before(q.nextCleanupAttempt) {
		q.mu.Unlock()
		return nil
	}
	q.mu.Unlock()
	if !q.cleanupRunMu.TryLock() {
		return nil
	}
	defer q.cleanupRunMu.Unlock()

	errCleanup := q.cleanupSources()
	q.mu.Lock()
	q.nextCleanupAttempt = time.Now().Add(requestLogCleanupRetryInterval)
	q.mu.Unlock()
	return errCleanup
}

func PendingFileBodySourceCleanupCount() int {
	return pendingFileBodySourceCleanup.sourceCount()
}

// RequestLogger defines the interface for logging HTTP requests and responses.
// It provides methods for logging both regular and streaming HTTP request/response cycles.
type RequestLogger interface {
	// LogRequest logs a complete non-streaming request/response cycle.
	//
	// Parameters:
	//   - url: The request URL
	//   - method: The HTTP method
	//   - requestHeaders: The request headers
	//   - body: The request body
	//   - statusCode: The response status code
	//   - responseHeaders: The response headers
	//   - response: The raw response data
	//   - apiRequest: The API request data
	//   - apiResponse: The API response data
	//   - requestID: Optional request ID for log file naming
	//   - requestTimestamp: When the request was received
	//   - apiResponseTimestamp: When the API response was received
	//
	// Returns:
	//   - error: An error if logging fails, nil otherwise
	LogRequest(
		url, method string,
		requestHeaders map[string][]string,
		body []byte,
		statusCode int,
		responseHeaders map[string][]string,
		response, websocketTimeline, apiRequest, apiResponse, apiWebsocketTimeline []byte,
		apiResponseErrors []*interfaces.ErrorMessage,
		requestID string,
		requestTimestamp, apiResponseTimestamp time.Time,
	) error

	// LogStreamingRequest initiates logging for a streaming request and returns a writer for chunks.
	//
	// Parameters:
	//   - url: The request URL
	//   - method: The HTTP method
	//   - headers: The request headers
	//   - body: The request body
	//   - requestID: Optional request ID for log file naming
	//
	// Returns:
	//   - StreamingLogWriter: A writer for streaming response chunks
	//   - error: An error if logging initialization fails, nil otherwise
	LogStreamingRequest(
		url, method string,
		headers map[string][]string,
		body []byte,
		requestID string,
	) (StreamingLogWriter, error)

	// IsEnabled returns whether request logging is currently enabled.
	//
	// Returns:
	//   - bool: True if logging is enabled, false otherwise
	IsEnabled() bool
}

// StreamingLogWriter handles real-time logging of streaming response chunks.
// It provides methods for writing streaming response data asynchronously.
type StreamingLogWriter interface {
	// WriteChunkAsync writes a response chunk asynchronously (non-blocking).
	//
	// Parameters:
	//   - chunk: The response chunk to write
	WriteChunkAsync(chunk []byte)

	// WriteStatus writes the response status and headers to the log.
	//
	// Parameters:
	//   - status: The response status code
	//   - headers: The response headers
	//
	// Returns:
	//   - error: An error if writing fails, nil otherwise
	WriteStatus(status int, headers map[string][]string) error

	// WriteAPIRequest writes the upstream API request details to the log.
	// This should be called before WriteStatus to maintain proper log ordering.
	//
	// Parameters:
	//   - apiRequest: The API request data (typically includes URL, headers, body sent upstream)
	//
	// Returns:
	//   - error: An error if writing fails, nil otherwise
	WriteAPIRequest(apiRequest []byte) error

	// WriteAPIResponse writes the upstream API response details to the log.
	// This should be called after the streaming response is complete.
	//
	// Parameters:
	//   - apiResponse: The API response data
	//
	// Returns:
	//   - error: An error if writing fails, nil otherwise
	WriteAPIResponse(apiResponse []byte) error

	// WriteAPIWebsocketTimeline writes the upstream websocket timeline to the log.
	// This should be called when upstream communication happened over websocket.
	//
	// Parameters:
	//   - apiWebsocketTimeline: The upstream websocket event timeline
	//
	// Returns:
	//   - error: An error if writing fails, nil otherwise
	WriteAPIWebsocketTimeline(apiWebsocketTimeline []byte) error

	// SetFirstChunkTimestamp sets the TTFB timestamp captured when first chunk was received.
	//
	// Parameters:
	//   - timestamp: The time when first response chunk was received
	SetFirstChunkTimestamp(timestamp time.Time)

	WriteRequestTimeline(requestTimeline []byte) error

	// Close finalizes the log file and cleans up resources.
	//
	// Returns:
	//   - error: An error if closing fails, nil otherwise
	Close() error
}

// FileRequestLogger implements RequestLogger using file-based storage.
// It provides file-based logging functionality for HTTP requests and responses.
type FileRequestLogger struct {
	configMu              sync.RWMutex
	cleanupMu             sync.Mutex
	cleanupRunMu          sync.Mutex
	pendingTempFiles      map[string]struct{}
	activeTempFiles       int
	pendingFinalTempFiles map[string]struct{}
	activeFinalTempFiles  int
	nextCleanupAttempt    time.Time
	cleanupScanDir        *os.File
	cleanupScanScheduled  atomic.Bool

	// enabled indicates whether request logging is currently enabled.
	enabled bool

	// logsDir is the directory where log files are stored.
	logsDir string

	// errorLogsMaxFiles limits the number of error log files retained.
	errorLogsMaxFiles int

	homeEnabled        bool
	homeClientProvider func() homeRequestLogClient
	artifactSink       RequestLogArtifactSink
}

var legacySDKRequestLogCleanup = &FileRequestLogger{
	logsDir: filepath.Join(os.TempDir(), legacySDKRequestLogTempDirName),
}

type fileRequestLoggerConfig struct {
	enabled           bool
	homeEnabled       bool
	errorLogsMaxFiles int
}

type fileRequestLoggerSnapshot struct {
	logger *FileRequestLogger
	config fileRequestLoggerConfig
}

type homeRequestLogPayload struct {
	Headers    map[string][]string `json:"headers,omitempty"`
	RequestID  string              `json:"request_id,omitempty"`
	RequestLog string              `json:"request_log,omitempty"`
}

func cloneHeaders(headers map[string][]string) map[string][]string {
	if len(headers) == 0 {
		return nil
	}
	out := make(map[string][]string, len(headers))
	for key, values := range headers {
		if strings.TrimSpace(key) == "" {
			continue
		}
		if values == nil {
			out[key] = nil
			continue
		}
		copied := make([]string, len(values))
		copy(copied, values)
		out[key] = copied
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func cloneMaskedHeaders(headers map[string][]string) map[string][]string {
	if len(headers) == 0 {
		return nil
	}
	out := make(map[string][]string, len(headers))
	for key, values := range headers {
		if strings.TrimSpace(key) == "" {
			continue
		}
		if values == nil {
			out[key] = nil
			continue
		}
		masked := make([]string, 0, len(values))
		for _, value := range values {
			masked = append(masked, util.MaskSensitiveHeaderValue(key, value))
		}
		out[key] = masked
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func (l *FileRequestLogger) homeRequestLogClient() homeRequestLogClient {
	if l == nil {
		return nil
	}
	l.configMu.RLock()
	provider := l.homeClientProvider
	l.configMu.RUnlock()
	if provider != nil {
		return provider()
	}
	return currentHomeRequestLogClient()
}

func (l *FileRequestLogger) forwardRequestLogToHome(
	ctx context.Context,
	headers map[string][]string,
	requestID string,
	logText string,
) error {
	if l == nil {
		return nil
	}
	client := l.homeRequestLogClient()
	if client == nil {
		return fmt.Errorf("home request log client unavailable")
	}
	if !client.HeartbeatOK() {
		return fmt.Errorf("home request log heartbeat unhealthy")
	}
	payload := homeRequestLogPayload{
		Headers:    cloneMaskedHeaders(headers),
		RequestID:  strings.TrimSpace(requestID),
		RequestLog: logText,
	}
	raw, errMarshal := json.Marshal(&payload)
	if errMarshal != nil {
		return errMarshal
	}
	if ctx == nil {
		ctx = context.Background()
	}
	return client.RPushRequestLog(ctx, raw)
}

// NewFileRequestLogger creates a new file-based request logger.
//
// Parameters:
//   - enabled: Whether request logging should be enabled
//   - logsDir: The directory where log files should be stored (can be relative)
//   - configDir: The directory of the configuration file; when logsDir is
//     relative, it will be resolved relative to this directory
//   - errorLogsMaxFiles: Maximum number of error log files to retain (0 = no cleanup)
//
// Returns:
//   - *FileRequestLogger: A new file-based request logger instance
func NewFileRequestLogger(enabled bool, logsDir string, configDir string, errorLogsMaxFiles int) *FileRequestLogger {
	// Resolve logsDir relative to the configuration file directory when it's not absolute.
	if !filepath.IsAbs(logsDir) {
		// If configDir is provided, resolve logsDir relative to it.
		if configDir != "" {
			logsDir = filepath.Join(configDir, logsDir)
		}
	}
	logger := &FileRequestLogger{
		enabled:           enabled,
		logsDir:           logsDir,
		errorLogsMaxFiles: errorLogsMaxFiles,
	}
	if errCleanup := logger.retryPendingCleanup(true); errCleanup != nil {
		log.WithError(errCleanup).Warn("failed to recover request log temp files")
	}
	return logger
}

// IsEnabled returns whether request logging is currently enabled.
//
// Returns:
//   - bool: True if logging is enabled, false otherwise
func (l *FileRequestLogger) configSnapshot() fileRequestLoggerConfig {
	if l == nil {
		return fileRequestLoggerConfig{}
	}
	l.configMu.RLock()
	defer l.configMu.RUnlock()
	return fileRequestLoggerConfig{
		enabled:           l.enabled,
		homeEnabled:       l.homeEnabled,
		errorLogsMaxFiles: l.errorLogsMaxFiles,
	}
}

func (l *FileRequestLogger) IsEnabled() bool {
	return l.configSnapshot().enabled
}

func (l *FileRequestLogger) Snapshot() RequestLogger {
	if l == nil {
		return nil
	}
	return &fileRequestLoggerSnapshot{logger: l, config: l.configSnapshot()}
}

func (s *fileRequestLoggerSnapshot) IsEnabled() bool {
	return s != nil && s.logger != nil && s.config.enabled
}

func (s *fileRequestLoggerSnapshot) Snapshot() RequestLogger {
	if s == nil {
		return nil
	}
	return s
}

func (s *fileRequestLoggerSnapshot) RetryPendingCleanup() error {
	if s == nil || s.logger == nil {
		return nil
	}
	return s.logger.retryPendingCleanup(false)
}

func (s *fileRequestLoggerSnapshot) NewFileBodySource(prefix string) (*FileBodySource, error) {
	if s == nil || s.logger == nil {
		return nil, fmt.Errorf("file request logger snapshot is nil")
	}
	return s.logger.NewFileBodySource(prefix)
}

func (s *fileRequestLoggerSnapshot) LogRequest(
	url, method string,
	requestHeaders map[string][]string,
	body []byte,
	statusCode int,
	responseHeaders map[string][]string,
	response, websocketTimeline, apiRequest, apiResponse, apiWebsocketTimeline []byte,
	apiResponseErrors []*interfaces.ErrorMessage,
	requestID string,
	requestTimestamp, apiResponseTimestamp time.Time,
) error {
	if s == nil || s.logger == nil {
		return fmt.Errorf("file request logger snapshot is nil")
	}
	return s.logger.logRequestWithSources(
		s.config, url, method, requestHeaders, body, statusCode, responseHeaders, response, websocketTimeline, nil,
		apiRequest, nil, apiResponse, nil, apiWebsocketTimeline, nil, nil, apiResponseErrors, false, requestID,
		requestTimestamp, apiResponseTimestamp,
	)
}

func (s *fileRequestLoggerSnapshot) LogRequestWithOptions(
	url, method string,
	requestHeaders map[string][]string,
	body []byte,
	statusCode int,
	responseHeaders map[string][]string,
	response, websocketTimeline, apiRequest, apiResponse, apiWebsocketTimeline []byte,
	apiResponseErrors []*interfaces.ErrorMessage,
	force bool,
	requestID string,
	requestTimestamp, apiResponseTimestamp time.Time,
) error {
	if s == nil || s.logger == nil {
		return fmt.Errorf("file request logger snapshot is nil")
	}
	return s.logger.logRequestWithSources(
		s.config, url, method, requestHeaders, body, statusCode, responseHeaders, response, websocketTimeline, nil,
		apiRequest, nil, apiResponse, nil, apiWebsocketTimeline, nil, nil, apiResponseErrors, force, requestID,
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
	apiResponseErrors []*interfaces.ErrorMessage,
	force bool,
	requestID string,
	requestTimestamp, apiResponseTimestamp time.Time,
) error {
	if s == nil || s.logger == nil {
		return fmt.Errorf("file request logger snapshot is nil")
	}
	return s.logger.logRequestWithSources(
		s.config, url, method, requestHeaders, body, statusCode, responseHeaders, response, websocketTimeline, nil,
		apiRequest, nil, apiResponse, nil, apiWebsocketTimeline, nil, requestTimeline, apiResponseErrors, force,
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
	websocketTimelineSource *FileBodySource,
	apiRequest []byte,
	apiRequestSource *FileBodySource,
	apiResponse []byte,
	apiResponseSource *FileBodySource,
	apiWebsocketTimeline []byte,
	apiWebsocketTimelineSource *FileBodySource,
	requestTimeline []byte,
	apiResponseErrors []*interfaces.ErrorMessage,
	force bool,
	requestID string,
	requestTimestamp, apiResponseTimestamp time.Time,
) error {
	if s == nil || s.logger == nil {
		return errors.Join(
			fmt.Errorf("file request logger snapshot is nil"),
			cleanupFileBodySources(
				websocketTimelineSource, apiRequestSource, apiResponseSource, apiWebsocketTimelineSource,
			),
		)
	}
	return s.logger.logRequestWithSources(
		s.config, url, method, requestHeaders, body, statusCode, responseHeaders, response, websocketTimeline,
		websocketTimelineSource, apiRequest, apiRequestSource, apiResponse, apiResponseSource, apiWebsocketTimeline,
		apiWebsocketTimelineSource, requestTimeline, apiResponseErrors, force, requestID, requestTimestamp,
		apiResponseTimestamp,
	)
}

func (s *fileRequestLoggerSnapshot) LogStreamingRequest(
	url, method string,
	headers map[string][]string,
	body []byte,
	requestID string,
) (StreamingLogWriter, error) {
	if s == nil || s.logger == nil {
		return nil, fmt.Errorf("file request logger snapshot is nil")
	}
	return s.logger.logStreamingRequest(s.config, url, method, headers, body, requestID)
}

func (l *FileRequestLogger) SetHomeEnabled(enabled bool) {
	if l == nil {
		return
	}
	if errCleanup := l.retryPendingCleanup(true); errCleanup != nil {
		log.WithError(errCleanup).Warn("failed to retry request log cleanup")
	}
	l.configMu.Lock()
	l.homeEnabled = enabled
	l.configMu.Unlock()
}

func (l *FileRequestLogger) SetHomeClientProvider(provider func() *home.Client) {
	if l == nil {
		return
	}
	var wrapped func() homeRequestLogClient
	if provider != nil {
		wrapped = func() homeRequestLogClient {
			client := provider()
			if client == nil {
				return nil
			}
			return client
		}
	}
	l.configMu.Lock()
	l.homeClientProvider = wrapped
	l.configMu.Unlock()
}

func (l *FileRequestLogger) NewFileBodySource(prefix string) (*FileBodySource, error) {
	if l == nil {
		return nil, fmt.Errorf("file request logger is nil")
	}
	if errEnsure := l.ensureLogsDir(); errEnsure != nil {
		return nil, errEnsure
	}
	return NewFileBodySourceInDir(l.logsDir, prefix)
}

// SetEnabled updates the request logging enabled state.
// This method allows dynamic enabling/disabling of request logging.
//
// Parameters:
//   - enabled: Whether request logging should be enabled
func (l *FileRequestLogger) SetEnabled(enabled bool) {
	if l == nil {
		return
	}
	if errCleanup := l.retryPendingCleanup(true); errCleanup != nil {
		log.WithError(errCleanup).Warn("failed to retry request log cleanup")
	}
	l.configMu.Lock()
	l.enabled = enabled
	l.configMu.Unlock()
}

// SetErrorLogsMaxFiles updates the maximum number of error log files to retain.
func (l *FileRequestLogger) SetErrorLogsMaxFiles(maxFiles int) {
	if l == nil {
		return
	}
	l.configMu.Lock()
	l.errorLogsMaxFiles = maxFiles
	l.configMu.Unlock()
}

// LogRequest logs a complete non-streaming request/response cycle to a file.
func (l *FileRequestLogger) LogRequest(
	url, method string,
	requestHeaders map[string][]string,
	body []byte,
	statusCode int,
	responseHeaders map[string][]string,
	response, websocketTimeline, apiRequest, apiResponse, apiWebsocketTimeline []byte,
	apiResponseErrors []*interfaces.ErrorMessage,
	requestID string,
	requestTimestamp, apiResponseTimestamp time.Time,
) error {
	return l.logRequestWithSources(
		l.configSnapshot(), url, method, requestHeaders, body, statusCode, responseHeaders, response, websocketTimeline,
		nil, apiRequest,
		nil, apiResponse, nil, apiWebsocketTimeline, nil, nil, apiResponseErrors, false, requestID,
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
	apiResponseErrors []*interfaces.ErrorMessage,
	force bool,
	requestID string,
	requestTimestamp, apiResponseTimestamp time.Time,
) error {
	return l.logRequestWithSources(
		l.configSnapshot(), url, method, requestHeaders, body, statusCode, responseHeaders, response, websocketTimeline,
		nil, apiRequest,
		nil, apiResponse, nil, apiWebsocketTimeline, nil, nil, apiResponseErrors, force, requestID,
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
	apiResponseErrors []*interfaces.ErrorMessage,
	force bool,
	requestID string,
	requestTimestamp, apiResponseTimestamp time.Time,
) error {
	return l.logRequestWithSources(
		l.configSnapshot(), url, method, requestHeaders, body, statusCode, responseHeaders, response, websocketTimeline,
		nil, apiRequest,
		nil, apiResponse, nil, apiWebsocketTimeline, nil, requestTimeline, apiResponseErrors, force, requestID,
		requestTimestamp, apiResponseTimestamp,
	)
}

func (l *FileRequestLogger) LogRequestWithOptionsAndAllSources(
	url, method string,
	requestHeaders map[string][]string,
	body []byte,
	statusCode int,
	responseHeaders map[string][]string,
	response, websocketTimeline []byte,
	websocketTimelineSource *FileBodySource,
	apiRequest []byte,
	apiRequestSource *FileBodySource,
	apiResponse []byte,
	apiResponseSource *FileBodySource,
	apiWebsocketTimeline []byte,
	apiWebsocketTimelineSource *FileBodySource,
	requestTimeline []byte,
	apiResponseErrors []*interfaces.ErrorMessage,
	force bool,
	requestID string,
	requestTimestamp, apiResponseTimestamp time.Time,
) error {
	return l.logRequestWithSources(
		l.configSnapshot(), url, method, requestHeaders, body, statusCode, responseHeaders, response, websocketTimeline,
		websocketTimelineSource, apiRequest, apiRequestSource, apiResponse, apiResponseSource, apiWebsocketTimeline,
		apiWebsocketTimelineSource, requestTimeline, apiResponseErrors, force, requestID, requestTimestamp,
		apiResponseTimestamp,
	)
}

func (l *FileRequestLogger) logRequestWithSources(
	loggerConfig fileRequestLoggerConfig,
	url, method string,
	requestHeaders map[string][]string,
	body []byte,
	statusCode int,
	responseHeaders map[string][]string,
	response, websocketTimeline []byte,
	websocketTimelineSource *FileBodySource,
	apiRequest []byte,
	apiRequestSource *FileBodySource,
	apiResponse []byte,
	apiResponseSource *FileBodySource,
	apiWebsocketTimeline []byte,
	apiWebsocketTimelineSource *FileBodySource,
	requestTimeline []byte,
	apiResponseErrors []*interfaces.ErrorMessage,
	force bool,
	requestID string,
	requestTimestamp, apiResponseTimestamp time.Time,
) (err error) {
	artifactRecorded := false
	defer func() {
		if err != nil && !artifactRecorded {
			l.recordRequestLogPublishFailed(requestID, err)
		}
	}()
	defer func() {
		err = errors.Join(
			err,
			cleanupFileBodySources(
				websocketTimelineSource, apiRequestSource, apiResponseSource, apiWebsocketTimelineSource,
			),
		)
	}()

	if errCleanup := l.retryPendingCleanup(false); errCleanup != nil {
		log.WithError(errCleanup).Warn("failed to retry request log cleanup")
	}
	if !loggerConfig.enabled && !force {
		return nil
	}

	if loggerConfig.homeEnabled && loggerConfig.enabled {
		responseToWrite, decompressErr := l.decompressResponse(responseHeaders, response)
		if decompressErr != nil {
			responseToWrite = response
		}

		var buf bytes.Buffer
		writeErr := l.writeNonStreamingLog(
			&buf, url, method, requestHeaders, body, websocketTimeline, websocketTimelineSource, apiRequest,
			apiRequestSource, apiResponse, apiResponseSource, apiWebsocketTimeline, apiWebsocketTimelineSource,
			requestTimeline, apiResponseErrors, statusCode, responseHeaders, responseToWrite, decompressErr,
			requestTimestamp, apiResponseTimestamp,
		)
		if writeErr != nil {
			return fmt.Errorf("failed to build request log content: %w", writeErr)
		}
		if errForward := l.forwardRequestLogToHome(
			context.Background(), requestHeaders, requestID, buf.String(),
		); errForward != nil {
			return errForward
		}
		l.recordRequestLogAvailable(requestID, RequestLogLocationHome, "")
		artifactRecorded = true
		return nil
	}

	if errEnsure := l.ensureLogsDir(); errEnsure != nil {
		return fmt.Errorf("failed to create logs directory: %w", errEnsure)
	}

	filename := l.generateFilename(url, requestID)
	if force && !loggerConfig.enabled {
		filename = l.generateErrorFilename(url, requestID)
	}
	filePath := filepath.Join(l.logsDir, filename)

	responseToWrite, decompressErr := l.decompressResponse(responseHeaders, response)
	if decompressErr != nil {
		responseToWrite = response
	}

	published, publishedPath, writeErr := writeRequestLogAtomicallyWithResult(
		l, filePath, func(logFile io.Writer) error {
			return l.writeNonStreamingLog(
				logFile, url, method, requestHeaders, body, websocketTimeline, websocketTimelineSource,
				apiRequest, apiRequestSource, apiResponse, apiResponseSource, apiWebsocketTimeline,
				apiWebsocketTimelineSource, requestTimeline, apiResponseErrors, statusCode, responseHeaders,
				responseToWrite,
				decompressErr, requestTimestamp, apiResponseTimestamp,
			)
		},
	)
	if published {
		l.recordRequestLogAvailable(requestID, RequestLogLocationLocal, filepath.Base(publishedPath))
		artifactRecorded = true
	}
	if writeErr != nil {
		return fmt.Errorf("failed to write log file: %w", writeErr)
	}

	if force && !loggerConfig.enabled {
		if errCleanup := l.cleanupOldErrorLogs(loggerConfig.errorLogsMaxFiles); errCleanup != nil {
			log.WithError(errCleanup).Warn("failed to clean up old error logs")
		}
	}

	return nil
}

// LogStreamingRequest initiates logging for a streaming request.
//
// Parameters:
//   - url: The request URL
//   - method: The HTTP method
//   - headers: The request headers
//   - body: The request body
//   - requestID: Optional request ID for log file naming
//
// Returns:
//   - StreamingLogWriter: A writer for streaming response chunks
//   - error: An error if logging initialization fails, nil otherwise
func (l *FileRequestLogger) LogStreamingRequest(
	url, method string,
	headers map[string][]string,
	body []byte,
	requestID string,
) (StreamingLogWriter, error) {
	return l.logStreamingRequest(l.configSnapshot(), url, method, headers, body, requestID)
}

func (l *FileRequestLogger) logStreamingRequest(
	loggerConfig fileRequestLoggerConfig,
	url, method string,
	headers map[string][]string,
	body []byte,
	requestID string,
) (StreamingLogWriter, error) {
	if errCleanup := l.retryPendingCleanup(false); errCleanup != nil {
		log.WithError(errCleanup).Warn("failed to retry request log cleanup")
	}
	if !loggerConfig.enabled {
		return &noOpStreamingLogWriter{}, nil
	}
	if loggerConfig.homeEnabled {
		return newHomeStreamingLogWriter(
			url,
			method,
			headers,
			body,
			requestID,
			l.homeRequestLogClient,
			l,
		), nil
	}

	// Ensure logs directory exists
	if err := l.ensureLogsDir(); err != nil {
		err = fmt.Errorf("failed to create logs directory: %w", err)
		l.recordRequestLogPublishFailed(requestID, err)
		return nil, err
	}

	if !l.reserveTempFileSlot() {
		err := fmt.Errorf(
			"request log temp cleanup capacity reached %d files",
			maxPendingRequestLogTempFiles,
		)
		l.recordRequestLogPublishFailed(requestID, err)
		return nil, err
	}
	slotReserved := true
	defer func() {
		if slotReserved {
			l.releaseActiveTempFileSlot()
		}
	}()

	// Generate filename with request ID
	filename := l.generateFilename(url, requestID)
	filePath := filepath.Join(l.logsDir, filename)

	requestHeaders := make(map[string][]string, len(headers))
	for key, values := range headers {
		headerValues := make([]string, len(values))
		copy(headerValues, values)
		requestHeaders[key] = headerValues
	}

	responseBodyLease, errLease := createRequestLogTempLease(l.logsDir, "response-body-")
	if errLease != nil {
		err := fmt.Errorf("failed to create response body temp lease: %w", errLease)
		l.recordRequestLogPublishFailed(requestID, err)
		return nil, err
	}
	responseBodyPath := strings.TrimSuffix(responseBodyLease.path, responseBodyLeaseSuffix) + ".tmp"
	responseBodyFile, errCreate := os.OpenFile(responseBodyPath, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0o600)
	if errCreate != nil {
		err := errors.Join(
			fmt.Errorf("failed to create response body temp file: %w", errCreate),
			responseBodyLease.release(true),
		)
		l.recordRequestLogPublishFailed(requestID, err)
		return nil, err
	}

	// Create streaming writer
	writer := &fileStreamingLogWriter{
		logFilePath:       filePath,
		requestID:         strings.TrimSpace(requestID),
		url:               url,
		method:            method,
		timestamp:         time.Now(),
		requestHeaders:    requestHeaders,
		logger:            l,
		requestBody:       cloneBoundedLogPayload(body),
		responseBodyPath:  responseBodyPath,
		responseBodyFile:  responseBodyFile,
		responseBodyLease: responseBodyLease,
		tempSlotReserved:  true,
		chunkChan:         make(chan []byte, 100), // Buffered channel for async writes
		closeChan:         make(chan struct{}),
	}

	// Start async writer goroutine
	go writer.asyncWriter()
	slotReserved = false

	return writer, nil
}

// generateErrorFilename creates a filename with an error prefix to differentiate forced error logs.
func (l *FileRequestLogger) generateErrorFilename(url string, requestID ...string) string {
	return fmt.Sprintf("error-%s", l.generateFilename(url, requestID...))
}

// ensureLogsDir creates the logs directory if it doesn't exist.
//
// Returns:
//   - error: An error if directory creation fails, nil otherwise
func (l *FileRequestLogger) ensureLogsDir() error {
	if _, err := os.Stat(l.logsDir); os.IsNotExist(err) {
		return os.MkdirAll(l.logsDir, 0o700)
	}
	return nil
}

// generateFilename creates a sanitized filename from the URL path and current timestamp.
// Format: v1-responses-2025-12-23T195811-a1b2c3d4.log
//
// Parameters:
//   - url: The request URL
//   - requestID: Optional request ID to include in filename
//
// Returns:
//   - string: A sanitized filename for the log file
func (l *FileRequestLogger) generateFilename(url string, requestID ...string) string {
	// Extract path from URL
	path := url
	if strings.Contains(url, "?") {
		path = strings.Split(url, "?")[0]
	}

	// Remove leading slash
	path = strings.TrimPrefix(path, "/")

	// Sanitize path for filename
	sanitized := l.sanitizeForFilename(path)

	// Add timestamp
	timestamp := time.Now().Format("2006-01-02T150405")

	// Use request ID if provided, otherwise use sequential ID
	var idPart string
	if len(requestID) > 0 && requestID[0] != "" {
		idPart = requestID[0]
	} else {
		id := requestLogID.Add(1)
		idPart = fmt.Sprintf("%d", id)
	}

	return fmt.Sprintf("%s-%s-%s.log", sanitized, timestamp, idPart)
}

// sanitizeForFilename replaces characters that are not safe for filenames.
//
// Parameters:
//   - path: The path to sanitize
//
// Returns:
//   - string: A sanitized filename
func (l *FileRequestLogger) sanitizeForFilename(path string) string {
	// Replace slashes with hyphens
	sanitized := strings.ReplaceAll(path, "/", "-")

	// Replace colons with hyphens
	sanitized = strings.ReplaceAll(sanitized, ":", "-")

	// Replace other problematic characters with hyphens
	reg := regexp.MustCompile(`[<>:"|?*\s]`)
	sanitized = reg.ReplaceAllString(sanitized, "-")

	// Remove multiple consecutive hyphens
	reg = regexp.MustCompile(`-+`)
	sanitized = reg.ReplaceAllString(sanitized, "-")

	// Remove leading/trailing hyphens
	sanitized = strings.Trim(sanitized, "-")

	// Handle empty result
	if sanitized == "" {
		sanitized = "root"
	}

	return sanitized
}

// cleanupOldErrorLogs keeps only the newest errorLogsMaxFiles forced error log files.
func (l *FileRequestLogger) cleanupOldErrorLogs(maxFiles int) error {
	if maxFiles <= 0 {
		return nil
	}

	entries, errRead := os.ReadDir(l.logsDir)
	if errRead != nil {
		return errRead
	}

	type logFile struct {
		name    string
		modTime time.Time
	}

	var files []logFile
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		name := entry.Name()
		if !strings.HasPrefix(name, "error-") || !strings.HasSuffix(name, ".log") {
			continue
		}
		info, errInfo := entry.Info()
		if errInfo != nil {
			log.WithError(errInfo).Warn("failed to read error log info")
			continue
		}
		files = append(files, logFile{name: name, modTime: info.ModTime()})
	}

	if len(files) <= maxFiles {
		return nil
	}

	sort.Slice(
		files, func(i, j int) bool {
			return files[i].modTime.After(files[j].modTime)
		},
	)

	for _, file := range files[maxFiles:] {
		if errRemove := os.Remove(filepath.Join(l.logsDir, file.name)); errRemove != nil {
			log.WithError(errRemove).Warnf("failed to remove old error log: %s", file.name)
			continue
		}
		requestID := requestIDFromRequestLogFileName(file.name)
		if requestID != "" {
			l.RecordRequestLogArtifact(
				RequestLogArtifact{
					RequestID: requestID,
					State:     RequestLogExpired,
					Reason:    "request log retention limit reached",
				},
			)
		}
	}

	return nil
}

func (l *FileRequestLogger) reserveTempFileSlot() bool {
	if l == nil {
		return false
	}
	l.cleanupMu.Lock()
	defer l.cleanupMu.Unlock()
	if l.activeTempFiles+len(l.pendingTempFiles) >= maxPendingRequestLogTempFiles {
		return false
	}
	l.activeTempFiles++
	return true
}

func (l *FileRequestLogger) releaseActiveTempFileSlot() {
	if l == nil {
		return
	}
	l.cleanupMu.Lock()
	if l.activeTempFiles > 0 {
		l.activeTempFiles--
	}
	l.cleanupMu.Unlock()
}

func (l *FileRequestLogger) retainReservedTempFile(path string) {
	if l == nil || path == "" {
		return
	}
	l.cleanupMu.Lock()
	if l.activeTempFiles > 0 {
		l.activeTempFiles--
	}
	if l.pendingTempFiles == nil {
		l.pendingTempFiles = make(map[string]struct{})
	}
	if _, exists := l.pendingTempFiles[path]; !exists {
		wasEmpty := len(l.pendingTempFiles) == 0 && len(l.pendingFinalTempFiles) == 0
		l.pendingTempFiles[path] = struct{}{}
		if wasEmpty {
			l.nextCleanupAttempt = time.Time{}
		}
	}
	l.cleanupMu.Unlock()
}

func (l *FileRequestLogger) retainTempFileForCleanup(path string) bool {
	if l == nil || path == "" {
		return false
	}
	l.cleanupMu.Lock()
	defer l.cleanupMu.Unlock()
	if _, exists := l.pendingTempFiles[path]; exists {
		return true
	}
	if l.activeTempFiles+len(l.pendingTempFiles) >= maxPendingRequestLogTempFiles {
		return false
	}
	if l.pendingTempFiles == nil {
		l.pendingTempFiles = make(map[string]struct{})
	}
	wasEmpty := len(l.pendingTempFiles) == 0 && len(l.pendingFinalTempFiles) == 0
	l.pendingTempFiles[path] = struct{}{}
	if wasEmpty {
		l.nextCleanupAttempt = time.Time{}
	}
	return true
}

func (l *FileRequestLogger) forgetTempFile(path string) {
	if l == nil || path == "" {
		return
	}
	l.cleanupMu.Lock()
	delete(l.pendingTempFiles, path)
	l.cleanupMu.Unlock()
}

func (l *FileRequestLogger) reserveFinalTempFileSlot() bool {
	if l == nil {
		return false
	}
	l.cleanupMu.Lock()
	defer l.cleanupMu.Unlock()
	if l.activeFinalTempFiles+len(l.pendingFinalTempFiles) >= maxPendingRequestLogTempFiles {
		return false
	}
	l.activeFinalTempFiles++
	return true
}

func (l *FileRequestLogger) releaseActiveFinalTempFileSlot() {
	if l == nil {
		return
	}
	l.cleanupMu.Lock()
	if l.activeFinalTempFiles > 0 {
		l.activeFinalTempFiles--
	}
	l.cleanupMu.Unlock()
}

func (l *FileRequestLogger) retainReservedFinalTempFile(path string) {
	if l == nil || path == "" {
		return
	}
	l.cleanupMu.Lock()
	if l.activeFinalTempFiles > 0 {
		l.activeFinalTempFiles--
	}
	if l.pendingFinalTempFiles == nil {
		l.pendingFinalTempFiles = make(map[string]struct{})
	}
	if _, exists := l.pendingFinalTempFiles[path]; !exists {
		wasEmpty := len(l.pendingTempFiles) == 0 && len(l.pendingFinalTempFiles) == 0
		l.pendingFinalTempFiles[path] = struct{}{}
		if wasEmpty {
			l.nextCleanupAttempt = time.Time{}
		}
	}
	l.cleanupMu.Unlock()
}

func (l *FileRequestLogger) retainFinalTempFileForCleanup(path string) bool {
	if l == nil || path == "" {
		return false
	}
	l.cleanupMu.Lock()
	defer l.cleanupMu.Unlock()
	if _, exists := l.pendingFinalTempFiles[path]; exists {
		return true
	}
	if l.activeFinalTempFiles+len(l.pendingFinalTempFiles) >= maxPendingRequestLogTempFiles {
		return false
	}
	if l.pendingFinalTempFiles == nil {
		l.pendingFinalTempFiles = make(map[string]struct{})
	}
	wasEmpty := len(l.pendingTempFiles) == 0 && len(l.pendingFinalTempFiles) == 0
	l.pendingFinalTempFiles[path] = struct{}{}
	if wasEmpty {
		l.nextCleanupAttempt = time.Time{}
	}
	return true
}

func (l *FileRequestLogger) forgetFinalTempFile(path string) {
	if l == nil || path == "" {
		return
	}
	l.cleanupMu.Lock()
	delete(l.pendingFinalTempFiles, path)
	l.cleanupMu.Unlock()
}

func (l *FileRequestLogger) pendingFinalTempFileCount() int {
	if l == nil {
		return 0
	}
	l.cleanupMu.Lock()
	defer l.cleanupMu.Unlock()
	return len(l.pendingFinalTempFiles)
}

func (l *FileRequestLogger) RetryPendingCleanup() error {
	return l.retryPendingCleanup(false)
}

func (l *FileRequestLogger) retryPendingCleanup(force bool) error {
	if l == nil {
		return nil
	}
	now := time.Now()
	l.cleanupMu.Lock()
	if !force && now.Before(l.nextCleanupAttempt) {
		l.cleanupMu.Unlock()
		return nil
	}
	l.cleanupMu.Unlock()
	if !l.cleanupRunMu.TryLock() {
		return nil
	}
	defer l.cleanupRunMu.Unlock()

	errCleanup := errors.Join(
		l.retryPendingTempFileCleanup(),
		RetryPendingFileBodySourceCleanup(),
		l.discoverTempFileCleanup(),
	)
	if l.cleanupScanDir != nil {
		l.scheduleCleanupScan()
	}
	l.cleanupMu.Lock()
	l.nextCleanupAttempt = time.Now().Add(requestLogCleanupRetryInterval)
	l.cleanupMu.Unlock()
	return errCleanup
}

func (l *FileRequestLogger) scheduleCleanupScan() {
	if l == nil || !l.cleanupScanScheduled.CompareAndSwap(false, true) {
		return
	}
	time.AfterFunc(
		10*time.Millisecond, func() {
			l.cleanupScanScheduled.Store(false)
			if !l.cleanupRunMu.TryLock() {
				l.scheduleCleanupScan()
				return
			}
			errCleanup := l.discoverTempFileCleanup()
			more := l.cleanupScanDir != nil
			l.cleanupRunMu.Unlock()
			if errCleanup != nil {
				log.WithError(errCleanup).Warn("failed to continue request log cleanup scan")
			}
			if more {
				l.scheduleCleanupScan()
			}
		},
	)
}

func (l *FileRequestLogger) discoverTempFileCleanup() error {
	if l == nil || l.logsDir == "" {
		return nil
	}
	if l.cleanupScanDir == nil {
		dir, errOpen := os.Open(l.logsDir)
		if errOpen != nil {
			if os.IsNotExist(errOpen) {
				return nil
			}
			return errOpen
		}
		l.cleanupScanDir = dir
	}

	entries, errRead := l.cleanupScanDir.ReadDir(requestLogCleanupScanBatchSize)
	if errRead != nil && !errors.Is(errRead, io.EOF) {
		return errors.Join(errRead, l.closeCleanupScanDir())
	}
	var cleanupErr error
	for _, entry := range entries {
		path := filepath.Join(l.logsDir, entry.Name())
		switch {
		case !entry.IsDir() &&
			(strings.HasPrefix(entry.Name(), "response-body-") ||
				strings.HasPrefix(entry.Name(), "request-log-final-")) &&
			strings.HasSuffix(entry.Name(), ".tmp"):
			cleanupErr = errors.Join(cleanupErr, l.cleanupDiscoveredResponseTempFile(path))
		case !entry.IsDir() &&
			(strings.HasPrefix(entry.Name(), "response-body-") ||
				strings.HasPrefix(entry.Name(), "request-log-final-")) &&
			strings.HasSuffix(entry.Name(), responseBodyLeaseSuffix):
			cleanupErr = errors.Join(cleanupErr, l.cleanupDiscoveredResponseLease(path))
		case !entry.IsDir() && strings.HasPrefix(entry.Name(), ".response-body-lease-pending-"):
			cleanupErr = errors.Join(cleanupErr, cleanupPendingRequestLogLease(path))
		case !entry.IsDir() && strings.HasPrefix(entry.Name(), ".request-log-final-lease-pending-"):
			cleanupErr = errors.Join(cleanupErr, cleanupPendingRequestLogLease(path))
		case entry.IsDir() && strings.HasPrefix(entry.Name(), "request-log-parts-"):
			cleanupErr = errors.Join(cleanupErr, cleanupDiscoveredFileBodySourceDir(path))
		}
	}
	if errors.Is(errRead, io.EOF) || len(entries) < requestLogCleanupScanBatchSize {
		cleanupErr = errors.Join(cleanupErr, l.closeCleanupScanDir())
	}
	return cleanupErr
}

func (l *FileRequestLogger) closeCleanupScanDir() error {
	if l == nil || l.cleanupScanDir == nil {
		return nil
	}
	dir := l.cleanupScanDir
	l.cleanupScanDir = nil
	return dir.Close()
}

func (l *FileRequestLogger) cleanupDiscoveredResponseLease(path string) error {
	tempPath := strings.TrimSuffix(path, responseBodyLeaseSuffix) + ".tmp"
	if _, errStat := os.Stat(tempPath); errStat == nil {
		return nil
	} else if !os.IsNotExist(errStat) {
		return errStat
	}
	lease, active, errLease := tryAcquireRequestLogFileLease(path)
	if errLease != nil || active || lease == nil {
		return errLease
	}
	if errRelease := lease.release(true); errRelease != nil {
		return errRelease
	}
	if isFinalRequestLogTempPath(tempPath) {
		l.forgetFinalTempFile(tempPath)
	} else {
		l.forgetTempFile(tempPath)
	}
	return nil
}

func isFinalRequestLogTempPath(path string) bool {
	return strings.HasPrefix(filepath.Base(path), "request-log-final-")
}

func cleanupPendingRequestLogLease(path string) error {
	stale, errStale := requestLogPendingLeaseIsStale(path)
	if errStale != nil || !stale {
		return errStale
	}
	lease, active, errLease := tryAcquireRequestLogFileLease(path)
	if errLease != nil || active || lease == nil {
		return errLease
	}
	return lease.release(true)
}

func requestLogPendingLeaseIsStale(path string) (bool, error) {
	info, errStat := os.Stat(path)
	if errStat != nil {
		if os.IsNotExist(errStat) {
			return false, nil
		}
		return false, errStat
	}
	return time.Since(info.ModTime()) >= requestLogPendingLeaseStaleAge, nil
}

func (l *FileRequestLogger) cleanupDiscoveredResponseTempFile(path string) error {
	leasePath := strings.TrimSuffix(path, ".tmp") + responseBodyLeaseSuffix
	lease, active, errLease := tryAcquireRequestLogFileLease(leasePath)
	if errLease != nil {
		return errLease
	}
	if active {
		return nil
	}
	if lease == nil {
		return nil
	}

	if errRemove := os.Remove(path); errRemove != nil && !os.IsNotExist(errRemove) {
		_ = lease.release(false)
		var retained bool
		if isFinalRequestLogTempPath(path) {
			retained = l.retainFinalTempFileForCleanup(path)
		} else {
			retained = l.retainTempFileForCleanup(path)
		}
		if !retained {
			return fmt.Errorf(
				"request log temp cleanup capacity reached %d files: %w",
				maxPendingRequestLogTempFiles,
				errRemove,
			)
		}
		return fmt.Errorf("failed to remove response body temp file: %w", errRemove)
	}
	if errRelease := lease.release(true); errRelease != nil {
		return errRelease
	}
	if isFinalRequestLogTempPath(path) {
		l.forgetFinalTempFile(path)
	} else {
		l.forgetTempFile(path)
	}
	return nil
}

func cleanupDiscoveredFileBodySourceDir(path string) error {
	leasePath := filepath.Join(path, fileBodySourceOwnerLeaseName)
	lease, active, errLease := tryAcquireRequestLogFileLease(leasePath)
	if errLease != nil {
		return errLease
	}
	if active {
		return nil
	}
	if lease == nil {
		pendingFound, pendingActive, errPending := cleanupPendingFileBodySourceLeases(path)
		if errPending != nil || pendingActive || !pendingFound {
			return errPending
		}
	} else if errRelease := lease.release(false); errRelease != nil {
		return errRelease
	}

	entries, errRead := os.ReadDir(path)
	if errRead != nil {
		if os.IsNotExist(errRead) {
			return nil
		}
		return errRead
	}
	var cleanupErr error
	for _, entry := range entries {
		if entry.Name() == fileBodySourceOwnerLeaseName {
			continue
		}
		cleanupErr = errors.Join(cleanupErr, os.RemoveAll(filepath.Join(path, entry.Name())))
	}
	if cleanupErr != nil {
		return cleanupErr
	}
	if errRemove := os.Remove(leasePath); errRemove != nil && !os.IsNotExist(errRemove) {
		return errRemove
	}
	if errRemove := os.Remove(path); errRemove != nil && !os.IsNotExist(errRemove) {
		restoredLease, errRestore := createRequestLogFileLease(leasePath)
		if errRestore == nil {
			errRestore = restoredLease.release(false)
		}
		return errors.Join(errRemove, errRestore)
	}
	return nil
}

func cleanupPendingFileBodySourceLeases(path string) (found bool, active bool, err error) {
	entries, errRead := os.ReadDir(path)
	if errRead != nil {
		if os.IsNotExist(errRead) {
			return false, false, nil
		}
		return false, false, errRead
	}
	prefix := "." + fileBodySourceOwnerLeaseName + ".pending-"
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasPrefix(entry.Name(), prefix) {
			continue
		}
		found = true
		pendingPath := filepath.Join(path, entry.Name())
		stale, errStale := requestLogPendingLeaseIsStale(pendingPath)
		if errStale != nil {
			return found, false, errStale
		}
		if !stale {
			return found, true, nil
		}
		lease, leaseActive, errLease := tryAcquireRequestLogFileLease(pendingPath)
		if errLease != nil {
			return found, false, errLease
		}
		if leaseActive {
			return found, true, nil
		}
		if lease != nil {
			if errRelease := lease.release(true); errRelease != nil {
				return found, false, errRelease
			}
		}
	}
	return found, false, nil
}

func (l *FileRequestLogger) pendingTempFileCount() int {
	if l == nil {
		return 0
	}
	l.cleanupMu.Lock()
	defer l.cleanupMu.Unlock()
	return len(l.pendingTempFiles)
}

func (l *FileRequestLogger) retryPendingTempFileCleanup() error {
	if l == nil {
		return nil
	}
	l.cleanupMu.Lock()
	responsePaths := make([]string, 0, len(l.pendingTempFiles))
	for path := range l.pendingTempFiles {
		responsePaths = append(responsePaths, path)
	}
	finalPaths := make([]string, 0, len(l.pendingFinalTempFiles))
	for path := range l.pendingFinalTempFiles {
		finalPaths = append(finalPaths, path)
	}
	l.cleanupMu.Unlock()

	var cleanupErr error
	for _, path := range responsePaths {
		if errRemove := removeRequestLogTempFile(path, "response body"); errRemove != nil {
			cleanupErr = errors.Join(cleanupErr, errRemove)
			continue
		}
		l.forgetTempFile(path)
	}
	for _, path := range finalPaths {
		if errRemove := removeRequestLogTempFile(path, "request log publish"); errRemove != nil {
			cleanupErr = errors.Join(cleanupErr, errRemove)
			continue
		}
		l.forgetFinalTempFile(path)
	}
	return cleanupErr
}

func removeRequestLogTempFile(path string, description string) error {
	if path == "" {
		return nil
	}
	if errRemove := removeRequestLogPath(path); errRemove != nil && !os.IsNotExist(errRemove) {
		return fmt.Errorf("failed to remove %s temp file: %w", description, errRemove)
	}
	leasePath := strings.TrimSuffix(path, ".tmp") + responseBodyLeaseSuffix
	if errRemove := removeRequestLogPath(leasePath); errRemove != nil && !os.IsNotExist(errRemove) {
		return fmt.Errorf("failed to remove %s temp lease: %w", description, errRemove)
	}
	return nil
}

func (l *FileRequestLogger) writeNonStreamingLog(
	w io.Writer,
	url, method string,
	requestHeaders map[string][]string,
	requestBody []byte,
	websocketTimeline []byte,
	websocketTimelineSource *FileBodySource,
	apiRequest []byte,
	apiRequestSource *FileBodySource,
	apiResponse []byte,
	apiResponseSource *FileBodySource,
	apiWebsocketTimeline []byte,
	apiWebsocketTimelineSource *FileBodySource,
	requestTimeline []byte,
	apiResponseErrors []*interfaces.ErrorMessage,
	statusCode int,
	responseHeaders map[string][]string,
	response []byte,
	decompressErr error,
	requestTimestamp time.Time,
	apiResponseTimestamp time.Time,
) error {
	if requestTimestamp.IsZero() {
		requestTimestamp = time.Now()
	}
	isWebsocketTranscript := hasSectionPayload(websocketTimeline) || hasFileBodySourcePayload(websocketTimelineSource)
	timeline, routeTrace := splitRequestTimelinePayload(requestTimeline)
	if errWrite := writeRequestInfoWithBody(
		w, url, method, requestHeaders, requestBody, requestTimestamp,
	); errWrite != nil {
		return errWrite
	}
	if errWrite := writeLogPayload(w, timeline); errWrite != nil {
		return errWrite
	}
	if errWrite := writeAPISectionWithSource(
		w, "=== WEBSOCKET TIMELINE ===\n", "=== WEBSOCKET TIMELINE", websocketTimeline, websocketTimelineSource,
		time.Time{},
	); errWrite != nil {
		return errWrite
	}
	if errWrite := writeAPISectionWithSource(
		w, "=== API WEBSOCKET TIMELINE ===\n", "=== API WEBSOCKET TIMELINE", apiWebsocketTimeline,
		apiWebsocketTimelineSource, time.Time{},
	); errWrite != nil {
		return errWrite
	}
	if errWrite := writePreformattedAPISectionWithSource(
		w, "=== API REQUEST ===\n", "=== API REQUEST", apiRequest, apiRequestSource, time.Time{},
	); errWrite != nil {
		return errWrite
	}
	if errWrite := writeAPIErrorResponses(w, apiResponseErrors); errWrite != nil {
		return errWrite
	}
	if errWrite := writePreformattedAPISectionWithSource(
		w, "=== API RESPONSE ===\n", "=== API RESPONSE", apiResponse, apiResponseSource, apiResponseTimestamp,
	); errWrite != nil {
		return errWrite
	}
	if isWebsocketTranscript {
		return writeTrailingRouteTrace(w, routeTrace)
	}
	if errWrite := writeResponseSection(
		w, statusCode, true, responseHeaders, bytes.NewReader(response), decompressErr, true,
	); errWrite != nil {
		return errWrite
	}
	return writeTrailingRouteTrace(w, routeTrace)
}

func writeRequestInfoWithBody(
	w io.Writer,
	url, method string,
	headers map[string][]string,
	body []byte,
	timestamp time.Time,
) error {
	if _, errWrite := io.WriteString(w, "=== REQUEST INFO ===\n"); errWrite != nil {
		return errWrite
	}
	if _, errWrite := io.WriteString(w, fmt.Sprintf("Version: %s\n", buildinfo.Version)); errWrite != nil {
		return errWrite
	}
	if _, errWrite := io.WriteString(w, fmt.Sprintf("URL: %s\n", url)); errWrite != nil {
		return errWrite
	}
	if _, errWrite := io.WriteString(w, fmt.Sprintf("Method: %s\n", method)); errWrite != nil {
		return errWrite
	}
	if _, errWrite := io.WriteString(
		w, fmt.Sprintf("Timestamp: %s\n", timestamp.Format(time.RFC3339Nano)),
	); errWrite != nil {
		return errWrite
	}
	if _, errWrite := io.WriteString(w, "\n"); errWrite != nil {
		return errWrite
	}

	if _, errWrite := io.WriteString(w, "=== HEADERS ===\n"); errWrite != nil {
		return errWrite
	}
	for key, values := range headers {
		for _, value := range values {
			masked := util.MaskSensitiveHeaderValue(key, value)
			if _, errWrite := io.WriteString(w, fmt.Sprintf("%s: %s\n", key, masked)); errWrite != nil {
				return errWrite
			}
		}
	}
	if _, errWrite := io.WriteString(w, "\n"); errWrite != nil {
		return errWrite
	}

	if _, errWrite := io.WriteString(w, "=== REQUEST BODY ===\n"); errWrite != nil {
		return errWrite
	}

	if errWrite := writeMaskedPayloadFromReader(w, bytes.NewReader(body)); errWrite != nil {
		return errWrite
	}

	if _, errWrite := io.WriteString(w, "\n\n"); errWrite != nil {
		return errWrite
	}
	return nil
}

func writeMaskedPayload(w io.Writer, payload []byte) error {
	if len(payload) == 0 {
		return nil
	}
	_, errWrite := w.Write(util.MaskSensitivePayload(payload))
	return errWrite
}

func writeMaskedPayloadFromReader(w io.Writer, reader io.Reader) error {
	payload, errRead := io.ReadAll(io.LimitReader(reader, maxMaskedPayloadBytes+1))
	if errRead != nil {
		return errRead
	}
	if len(payload) > maxMaskedPayloadBytes {
		_, errWrite := io.WriteString(w, "[payload omitted: exceeds safe redaction limit]")
		return errWrite
	}
	return writeMaskedPayload(w, payload)
}

func writeRequestLogAtomically(
	logger *FileRequestLogger,
	path string,
	writeLog func(io.Writer) error,
) error {
	_, _, err := writeRequestLogAtomicallyWithResult(logger, path, writeLog)
	return err
}

func writeRequestLogAtomicallyWithResult(
	logger *FileRequestLogger,
	path string,
	writeLog func(io.Writer) error,
) (published bool, publishedPath string, err error) {
	if strings.TrimSpace(path) == "" {
		return false, "", fmt.Errorf("request log path is required")
	}
	if writeLog == nil {
		return false, "", fmt.Errorf("request log writer is required")
	}
	slotReserved := false
	if logger != nil {
		if !logger.reserveFinalTempFileSlot() {
			return false, "", fmt.Errorf(
				"request log publish cleanup capacity reached %d files",
				maxPendingRequestLogTempFiles,
			)
		}
		slotReserved = true
		defer func() {
			if slotReserved {
				logger.releaseActiveFinalTempFileSlot()
			}
		}()
	}
	dir := filepath.Dir(path)
	lease, errLease := createRequestLogTempLease(dir, "request-log-final-")
	if errLease != nil {
		return false, "", fmt.Errorf("create request log publish lease: %w", errLease)
	}
	tempPath := strings.TrimSuffix(lease.path, responseBodyLeaseSuffix) + ".tmp"
	defer func() {
		var cleanupErr error
		if published {
			if errRemove := removeRequestLogPath(tempPath); errRemove != nil && !os.IsNotExist(errRemove) {
				cleanupErr = fmt.Errorf("failed to remove published request log temp file: %w", errRemove)
			}
			cleanupErr = errors.Join(cleanupErr, lease.release(true))
		} else if errRemove := removeRequestLogPath(tempPath); errRemove != nil && !os.IsNotExist(errRemove) {
			cleanupErr = errors.Join(
				fmt.Errorf("failed to remove request log publish temp file: %w", errRemove),
				lease.release(false),
			)
		} else {
			cleanupErr = lease.release(true)
		}
		if cleanupErr != nil && logger != nil && slotReserved {
			logger.retainReservedFinalTempFile(tempPath)
			slotReserved = false
		}
		err = errors.Join(err, cleanupErr)
	}()

	file, errCreate := os.OpenFile(tempPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if errCreate != nil {
		return false, "", errCreate
	}
	writeErr := writeLog(file)
	writeErr = errors.Join(writeErr, file.Sync(), file.Close())
	if writeErr != nil {
		return false, "", writeErr
	}
	for sequence := 0; sequence <= 1000; sequence++ {
		candidate := path
		if sequence > 0 {
			filename := filepath.Base(path)
			extension := filepath.Ext(filename)
			base := strings.TrimSuffix(filename, extension)
			if lastDash := strings.LastIndex(base, "-"); lastDash >= 0 {
				base = fmt.Sprintf("%s_%d%s", base[:lastDash], sequence, base[lastDash:])
			} else {
				base = fmt.Sprintf("%s_%d", base, sequence)
			}
			candidate = filepath.Join(dir, base+extension)
		}
		if errLink := os.Link(tempPath, candidate); errLink == nil {
			published = true
			return true, candidate, nil
		} else if !errors.Is(errLink, os.ErrExist) {
			return false, "", errLink
		}
	}
	return false, "", fmt.Errorf("too many conflicting request log files for %s", path)
}

func writeAll(w io.Writer, data []byte) error {
	for len(data) > 0 {
		n, errWrite := w.Write(data)
		if n > 0 {
			data = data[n:]
		}
		if errWrite != nil {
			return errWrite
		}
		if n == 0 {
			return io.ErrShortWrite
		}
	}
	return nil
}

func writeLogPart(w io.Writer, payload []byte, prependNewline bool) error {
	if w == nil {
		return nil
	}
	if prependNewline {
		if errWrite := writeAll(w, []byte("\n")); errWrite != nil {
			return errWrite
		}
	}
	if errWrite := writeAll(w, payload); errWrite != nil {
		return errWrite
	}
	if !bytes.HasSuffix(payload, []byte("\n")) {
		if errWrite := writeAll(w, []byte("\n")); errWrite != nil {
			return errWrite
		}
	}
	return nil
}

func writeAPISection(
	w io.Writer,
	sectionHeader string,
	sectionPrefix string,
	payload []byte,
	timestamp time.Time,
) error {
	if len(payload) == 0 {
		return nil
	}

	maskedPayload := util.MaskSensitivePayload(payload)
	if bytes.HasPrefix(maskedPayload, []byte(sectionPrefix)) {
		if _, errWrite := w.Write(maskedPayload); errWrite != nil {
			return errWrite
		}
		if !bytes.HasSuffix(maskedPayload, []byte("\n")) {
			if _, errWrite := io.WriteString(w, "\n"); errWrite != nil {
				return errWrite
			}
		}
	} else {
		if _, errWrite := io.WriteString(w, sectionHeader); errWrite != nil {
			return errWrite
		}
		if !timestamp.IsZero() {
			if _, errWrite := io.WriteString(
				w, fmt.Sprintf("Timestamp: %s\n", timestamp.Format(time.RFC3339Nano)),
			); errWrite != nil {
				return errWrite
			}
		}
		if _, errWrite := w.Write(maskedPayload); errWrite != nil {
			return errWrite
		}
		if _, errWrite := io.WriteString(w, "\n"); errWrite != nil {
			return errWrite
		}
	}

	if _, errWrite := io.WriteString(w, "\n"); errWrite != nil {
		return errWrite
	}
	return nil
}

func hasFileBodySourcePayload(source *FileBodySource) bool {
	return source != nil && source.HasPayload()
}

func writeAPISectionWithSource(
	w io.Writer,
	sectionHeader string,
	sectionPrefix string,
	payload []byte,
	source *FileBodySource,
	timestamp time.Time,
) error {
	if !hasFileBodySourcePayload(source) {
		return writeAPISection(w, sectionHeader, sectionPrefix, payload, timestamp)
	}
	if _, errWrite := io.WriteString(w, sectionHeader); errWrite != nil {
		return errWrite
	}
	if !timestamp.IsZero() {
		if _, errWrite := io.WriteString(
			w, fmt.Sprintf("Timestamp: %s\n", timestamp.Format(time.RFC3339Nano)),
		); errWrite != nil {
			return errWrite
		}
	}
	sourcePayload, errSource := source.Bytes()
	if errSource != nil {
		log.WithError(errSource).Warn("failed to read request log source; writing retained memory suffix")
		if _, errWrite := io.WriteString(w, "[source prefix unavailable: read failed]\n"); errWrite != nil {
			return errors.Join(errSource, errWrite)
		}
	} else if errWrite := writeAll(w, sourcePayload); errWrite != nil {
		return errWrite
	}
	if len(payload) > 0 {
		if _, errWrite := io.WriteString(w, "\n"); errWrite != nil {
			return errWrite
		}
		if errWrite := writeMaskedPayload(w, payload); errWrite != nil {
			return errWrite
		}
	}
	if _, errWrite := io.WriteString(w, "\n\n"); errWrite != nil {
		return errWrite
	}
	return nil
}

func writePreformattedAPISectionWithSource(
	w io.Writer,
	sectionHeader string,
	sectionPrefix string,
	payload []byte,
	source *FileBodySource,
	timestamp time.Time,
) error {
	if !hasFileBodySourcePayload(source) {
		return writeAPISection(w, sectionHeader, sectionPrefix, payload, timestamp)
	}
	sourcePayload, errSource := source.Bytes()
	if errSource != nil {
		log.WithError(errSource).Warn("failed to read request log source; writing retained memory suffix")
		if _, errWrite := io.WriteString(
			w, sectionHeader+"[source prefix unavailable: read failed]\n",
		); errWrite != nil {
			return errors.Join(errSource, errWrite)
		}
	} else if errWrite := writeAll(w, sourcePayload); errWrite != nil {
		return errWrite
	}
	if len(payload) > 0 {
		if errWrite := writeMaskedPayload(w, payload); errWrite != nil {
			return errWrite
		}
	}
	if _, errWrite := io.WriteString(w, "\n\n"); errWrite != nil {
		return errWrite
	}
	return nil
}

func writeAPIErrorResponses(w io.Writer, apiResponseErrors []*interfaces.ErrorMessage) error {
	for i := range apiResponseErrors {
		if apiResponseErrors[i] == nil {
			continue
		}
		if _, errWrite := io.WriteString(w, "=== API ERROR RESPONSE ===\n"); errWrite != nil {
			return errWrite
		}
		if _, errWrite := io.WriteString(
			w, fmt.Sprintf("HTTP Status: %d\n", apiResponseErrors[i].StatusCode),
		); errWrite != nil {
			return errWrite
		}
		if apiResponseErrors[i].Error != nil {
			if errWrite := writeMaskedPayload(w, []byte(apiResponseErrors[i].Error.Error())); errWrite != nil {
				return errWrite
			}
		}
		if _, errWrite := io.WriteString(w, "\n\n"); errWrite != nil {
			return errWrite
		}
	}
	return nil
}

var routeTraceSectionMarker = []byte("=== ROUTE TRACE ===\n")

func splitRequestTimelinePayload(payload []byte) (timeline, routeTrace []byte) {
	index := bytes.LastIndex(payload, routeTraceSectionMarker)
	if index < 0 || index > 0 && payload[index-1] != '\n' {
		return payload, nil
	}
	return payload[:index], payload[index:]
}

func writeLogPayload(w io.Writer, payload []byte) error {
	if len(payload) == 0 {
		return nil
	}
	if _, errWrite := w.Write(payload); errWrite != nil {
		return errWrite
	}
	if !bytes.HasSuffix(payload, []byte("\n")) {
		_, errWrite := io.WriteString(w, "\n")
		return errWrite
	}
	return nil
}

func writeTrailingRouteTrace(w io.Writer, routeTrace []byte) error {
	if len(routeTrace) == 0 {
		return nil
	}
	if _, errWrite := io.WriteString(w, "\n"); errWrite != nil {
		return errWrite
	}
	return writeLogPayload(w, routeTrace)
}

func hasSectionPayload(payload []byte) bool {
	return len(bytes.TrimSpace(payload)) > 0
}

func writeResponseSection(
	w io.Writer,
	statusCode int,
	statusWritten bool,
	responseHeaders map[string][]string,
	responseReader io.Reader,
	decompressErr error,
	trailingNewline bool,
) error {
	if _, errWrite := io.WriteString(w, "=== RESPONSE ===\n"); errWrite != nil {
		return errWrite
	}
	if statusWritten {
		if _, errWrite := io.WriteString(w, fmt.Sprintf("Status: %d\n", statusCode)); errWrite != nil {
			return errWrite
		}
	}

	for key, values := range responseHeaders {
		for _, value := range values {
			masked := util.MaskSensitiveHeaderValue(key, value)
			if _, errWrite := io.WriteString(w, fmt.Sprintf("%s: %s\n", key, masked)); errWrite != nil {
				return errWrite
			}
		}
	}

	if _, errWrite := io.WriteString(w, "\n"); errWrite != nil {
		return errWrite
	}

	if responseReader != nil {
		if errCopy := writeMaskedPayloadFromReader(w, responseReader); errCopy != nil {
			return errCopy
		}
	}
	if decompressErr != nil {
		if _, errWrite := io.WriteString(
			w, fmt.Sprintf("\n[DECOMPRESSION ERROR: %v]", decompressErr),
		); errWrite != nil {
			return errWrite
		}
	}

	if trailingNewline {
		if _, errWrite := io.WriteString(w, "\n"); errWrite != nil {
			return errWrite
		}
	}
	return nil
}

// decompressResponse decompresses response data based on Content-Encoding header.
//
// Parameters:
//   - responseHeaders: The response headers
//   - response: The response data to decompress
//
// Returns:
//   - []byte: The decompressed response data
//   - error: An error if decompression fails, nil otherwise
func (l *FileRequestLogger) decompressResponse(responseHeaders map[string][]string, response []byte) ([]byte, error) {
	if responseHeaders == nil || len(response) == 0 {
		return response, nil
	}

	// Check Content-Encoding header
	var contentEncoding string
	for key, values := range responseHeaders {
		if strings.ToLower(key) == "content-encoding" && len(values) > 0 {
			contentEncoding = strings.ToLower(values[0])
			break
		}
	}

	switch contentEncoding {
	case "gzip":
		return l.decompressGzip(response)
	case "deflate":
		return l.decompressDeflate(response)
	case "br":
		return l.decompressBrotli(response)
	case "zstd":
		return l.decompressZstd(response)
	default:
		// No compression or unsupported compression
		return response, nil
	}
}

// decompressGzip decompresses gzip-encoded data.
//
// Parameters:
//   - data: The gzip-encoded data to decompress
//
// Returns:
//   - []byte: The decompressed data
//   - error: An error if decompression fails, nil otherwise
func (l *FileRequestLogger) decompressGzip(data []byte) ([]byte, error) {
	reader, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		return nil, fmt.Errorf("failed to create gzip reader: %w", err)
	}
	defer func() {
		if errClose := reader.Close(); errClose != nil {
			log.WithError(errClose).Warn("failed to close gzip reader in request logger")
		}
	}()

	decompressed, err := io.ReadAll(io.LimitReader(reader, 50<<20))
	if err != nil {
		return nil, fmt.Errorf("failed to decompress gzip data: %w", err)
	}

	return decompressed, nil
}

// decompressDeflate decompresses deflate-encoded data.
//
// Parameters:
//   - data: The deflate-encoded data to decompress
//
// Returns:
//   - []byte: The decompressed data
//   - error: An error if decompression fails, nil otherwise
func (l *FileRequestLogger) decompressDeflate(data []byte) ([]byte, error) {
	reader := flate.NewReader(bytes.NewReader(data))
	defer func() {
		if errClose := reader.Close(); errClose != nil {
			log.WithError(errClose).Warn("failed to close deflate reader in request logger")
		}
	}()

	decompressed, err := io.ReadAll(io.LimitReader(reader, 50<<20))
	if err != nil {
		return nil, fmt.Errorf("failed to decompress deflate data: %w", err)
	}

	return decompressed, nil
}

// decompressBrotli decompresses brotli-encoded data.
//
// Parameters:
//   - data: The brotli-encoded data to decompress
//
// Returns:
//   - []byte: The decompressed data
//   - error: An error if decompression fails, nil otherwise
func (l *FileRequestLogger) decompressBrotli(data []byte) ([]byte, error) {
	reader := brotli.NewReader(bytes.NewReader(data))

	decompressed, err := io.ReadAll(io.LimitReader(reader, 50<<20))
	if err != nil {
		return nil, fmt.Errorf("failed to decompress brotli data: %w", err)
	}

	return decompressed, nil
}

// decompressZstd decompresses zstd-encoded data.
//
// Parameters:
//   - data: The zstd-encoded data to decompress
//
// Returns:
//   - []byte: The decompressed data
//   - error: An error if decompression fails, nil otherwise
func (l *FileRequestLogger) decompressZstd(data []byte) ([]byte, error) {
	decoder, err := zstd.NewReader(
		bytes.NewReader(data),
		zstd.WithDecoderConcurrency(1),
		zstd.WithDecoderMaxWindow(50<<20),
		zstd.WithDecoderMaxMemory(58<<20),
	)
	if err != nil {
		return nil, fmt.Errorf("failed to create zstd reader: %w", err)
	}
	defer decoder.Close()

	decompressed, err := io.ReadAll(io.LimitReader(decoder, 50<<20))
	if err != nil {
		return nil, fmt.Errorf("failed to decompress zstd data: %w", err)
	}

	return decompressed, nil
}

// fileStreamingLogWriter implements StreamingLogWriter for file-based streaming logs.
// It spools streaming response chunks to a temporary file to avoid retaining large responses in memory.
// The final log file is assembled when Close is called.
type fileStreamingLogWriter struct {
	mu        sync.Mutex
	closeOnce sync.Once
	closed    bool
	closeErr  error

	// logFilePath is the final log file path.
	logFilePath string
	requestID   string

	// url is the request URL (masked upstream in middleware).
	url string

	// method is the HTTP method.
	method string

	// timestamp is captured when the streaming log is initialized.
	timestamp time.Time

	// requestHeaders stores the request headers.
	requestHeaders map[string][]string
	logger         *FileRequestLogger

	// requestBody stores the bounded request body captured by middleware.
	requestBody []byte

	// responseBodyPath is a temporary file path holding the streaming response body.
	responseBodyPath string

	// responseBodyFile is the temp file where chunks are appended by the async writer.
	responseBodyFile *os.File

	responseBodyLease *requestLogFileLease
	tempSlotReserved  bool

	// chunkChan is a channel for receiving response chunks to spool.
	chunkChan chan []byte

	responseBodyBytes     int
	responseBodyTruncated bool

	// closeChan is a channel for signaling when the writer is closed.
	closeChan chan struct{}

	// asyncErr stores all response spool write and close errors.
	asyncErr error

	// responseStatus stores the HTTP status code.
	responseStatus int

	// statusWritten indicates whether a non-zero status was recorded.
	statusWritten bool

	// responseHeaders stores the response headers.
	responseHeaders map[string][]string

	// apiRequest stores the upstream API request data.
	apiRequest []byte

	apiRequestSource *FileBodySource

	// apiResponse stores the upstream API response data.
	apiResponse []byte

	apiResponseSource *FileBodySource

	// apiWebsocketTimeline stores the upstream websocket event timeline.
	apiWebsocketTimeline       []byte
	apiWebsocketTimelineSource *FileBodySource

	requestTimeline []byte

	// apiResponseTimestamp captures when the API response was received.
	apiResponseTimestamp time.Time
}

// WriteChunkAsync writes a response chunk asynchronously (non-blocking).
//
// Parameters:
//   - chunk: The response chunk to write
func (w *fileStreamingLogWriter) WriteChunkAsync(chunk []byte) {
	if w == nil || len(chunk) == 0 {
		return
	}

	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed || w.chunkChan == nil || w.responseBodyTruncated {
		return
	}
	if len(chunk) > maxStreamingResponseBodyBytes-w.responseBodyBytes {
		w.responseBodyTruncated = true
		log.Warn("request log streaming response exceeded capture limit; response body omitted")
		return
	}
	chunkCopy := bytes.Clone(chunk)
	select {
	case w.chunkChan <- chunkCopy:
		w.responseBodyBytes += len(chunkCopy)
	default:
		log.Warn("request log streaming buffer full, dropped response chunk")
	}
}

// WriteStatus buffers the response status and headers for later writing.
//
// Parameters:
//   - status: The response status code
//   - headers: The response headers
//
// Returns:
//   - error: An error if the writer has already been closed
func (w *fileStreamingLogWriter) WriteStatus(status int, headers map[string][]string) error {
	if w == nil || status == 0 {
		return nil
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
		return errStreamingLogWriterClosed
	}
	w.responseStatus = status
	w.responseHeaders = cloneHeaders(headers)
	w.statusWritten = true
	return nil
}

// WriteAPIRequest buffers the upstream API request details for later writing.
//
// Parameters:
//   - apiRequest: The API request data (typically includes URL, headers, body sent upstream)
//
// Returns:
//   - error: An error if the writer has already been closed
func (w *fileStreamingLogWriter) WriteAPIRequest(apiRequest []byte) error {
	if w == nil || len(apiRequest) == 0 {
		return nil
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
		return errStreamingLogWriterClosed
	}
	w.apiRequest = cloneBoundedLogPayload(apiRequest)
	return nil
}

// WriteAPIResponse buffers the upstream API response details for later writing.
//
// Parameters:
//   - apiResponse: The API response data
//
// Returns:
//   - error: An error if the writer has already been closed
func (w *fileStreamingLogWriter) WriteAPIResponse(apiResponse []byte) error {
	if w == nil || len(apiResponse) == 0 {
		return nil
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
		return errStreamingLogWriterClosed
	}
	w.apiResponse = cloneBoundedLogPayload(apiResponse)
	return nil
}

func (w *fileStreamingLogWriter) WriteAPIRequestSource(source *FileBodySource) error {
	if w == nil || source == nil || !source.HasPayload() {
		return nil
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
		return errStreamingLogWriterClosed
	}
	w.apiRequestSource = source
	return nil
}

func (w *fileStreamingLogWriter) WriteAPIResponseSource(source *FileBodySource) error {
	if w == nil || source == nil || !source.HasPayload() {
		return nil
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
		return errStreamingLogWriterClosed
	}
	w.apiResponseSource = source
	return nil
}

func (w *fileStreamingLogWriter) WriteAPIWebsocketTimeline(apiWebsocketTimeline []byte) error {
	if w == nil || len(apiWebsocketTimeline) == 0 {
		return nil
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
		return errStreamingLogWriterClosed
	}
	w.apiWebsocketTimeline = cloneBoundedLogPayload(apiWebsocketTimeline)
	return nil
}

func (w *fileStreamingLogWriter) WriteAPIWebsocketTimelineSource(source *FileBodySource) error {
	if w == nil || source == nil || !source.HasPayload() {
		return nil
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
		return errStreamingLogWriterClosed
	}
	w.apiWebsocketTimelineSource = source
	return nil
}

func (w *fileStreamingLogWriter) SetFirstChunkTimestamp(timestamp time.Time) {
	if w == nil || timestamp.IsZero() {
		return
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if !w.closed {
		w.apiResponseTimestamp = timestamp
	}
}

func (w *fileStreamingLogWriter) WriteRequestTimeline(requestTimeline []byte) error {
	if w == nil || len(requestTimeline) == 0 {
		return nil
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
		return errStreamingLogWriterClosed
	}
	w.requestTimeline = cloneBoundedLogPayload(requestTimeline)
	return nil
}

// Close finalizes the log file and cleans up resources.
// It writes all buffered data to the file in the correct order:
// API REQUEST -> API RESPONSE -> RESPONSE (status, headers, body chunks)
//
// Returns:
//   - error: An error if closing fails, nil otherwise
func (w *fileStreamingLogWriter) Close() error {
	if w == nil {
		return nil
	}
	w.closeOnce.Do(
		func() {
			w.mu.Lock()
			w.closed = true
			chunkChan := w.chunkChan
			closeChan := w.closeChan
			w.mu.Unlock()

			if chunkChan != nil {
				close(chunkChan)
			}
			if closeChan != nil {
				<-closeChan
			}
			if asyncErr := w.asyncError(); asyncErr != nil {
				w.closeErr = asyncErr
				if w.logger != nil {
					w.logger.recordRequestLogPublishFailed(w.requestID, asyncErr)
				}
				return
			}

			if w.logFilePath == "" {
				if w.requestID != "" {
					w.closeErr = errors.New("request log final path is unavailable")
					if w.logger != nil {
						w.logger.recordRequestLogPublishFailed(w.requestID, w.closeErr)
					}
				}
				return
			}

			published, publishedPath, errWrite := writeRequestLogAtomicallyWithResult(w.logger, w.logFilePath, w.writeFinalLog)
			w.closeErr = errWrite
			if w.logger == nil {
				return
			}
			if published {
				w.logger.recordRequestLogAvailable(
					w.requestID,
					RequestLogLocationLocal,
					filepath.Base(publishedPath),
				)
				return
			}
			w.logger.recordRequestLogPublishFailed(w.requestID, errWrite)
		},
	)
	return errors.Join(
		w.closeErr,
		w.cleanupTempFiles(),
		cleanupFileBodySources(
			w.apiRequestSource,
			w.apiResponseSource,
			w.apiWebsocketTimelineSource,
		),
	)
}

// asyncWriter runs in a goroutine to buffer chunks from the channel.
// It continuously reads chunks from the channel and appends them to a temp file for later assembly.
func (w *fileStreamingLogWriter) asyncWriter() {
	var asyncErr error
	defer func() {
		w.mu.Lock()
		w.asyncErr = asyncErr
		w.mu.Unlock()
		close(w.closeChan)
	}()

	for chunk := range w.chunkChan {
		if w.responseBodyFile == nil {
			continue
		}
		if errWrite := writeAll(w.responseBodyFile, chunk); errWrite != nil {
			asyncErr = errors.Join(asyncErr, errWrite)
			asyncErr = errors.Join(asyncErr, w.responseBodyFile.Close())
			w.responseBodyFile = nil
		}
	}

	if w.responseBodyFile == nil {
		return
	}
	asyncErr = errors.Join(asyncErr, w.responseBodyFile.Close())
	w.responseBodyFile = nil
}

func (w *fileStreamingLogWriter) asyncError() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.asyncErr
}

func (w *fileStreamingLogWriter) writeFinalLog(logFile io.Writer) (err error) {
	timeline, routeTrace := splitRequestTimelinePayload(w.requestTimeline)
	if errWrite := writeRequestInfoWithBody(
		logFile, w.url, w.method, w.requestHeaders, w.requestBody, w.timestamp,
	); errWrite != nil {
		return errWrite
	}
	if errWrite := writeLogPayload(logFile, timeline); errWrite != nil {
		return errWrite
	}
	if errWrite := writeAPISectionWithSource(
		logFile, "=== API WEBSOCKET TIMELINE ===\n", "=== API WEBSOCKET TIMELINE", w.apiWebsocketTimeline,
		w.apiWebsocketTimelineSource, time.Time{},
	); errWrite != nil {
		return errWrite
	}
	if errWrite := writePreformattedAPISectionWithSource(
		logFile, "=== API REQUEST ===\n", "=== API REQUEST", w.apiRequest, w.apiRequestSource, time.Time{},
	); errWrite != nil {
		return errWrite
	}
	if errWrite := writePreformattedAPISectionWithSource(
		logFile, "=== API RESPONSE ===\n", "=== API RESPONSE", w.apiResponse, w.apiResponseSource,
		w.apiResponseTimestamp,
	); errWrite != nil {
		return errWrite
	}

	if w.responseBodyTruncated {
		if errWrite := writeResponseSection(
			logFile, w.responseStatus, w.statusWritten, w.responseHeaders, nil, nil, false,
		); errWrite != nil {
			return errWrite
		}
		if _, errWrite := logFile.Write(truncatedInMemoryRequestLog); errWrite != nil {
			return errWrite
		}
		return writeTrailingRouteTrace(logFile, routeTrace)
	}

	responseBodyFile, errOpen := os.Open(w.responseBodyPath)
	if errOpen != nil {
		return errOpen
	}
	defer func() {
		err = errors.Join(err, responseBodyFile.Close())
	}()

	if errWrite := writeResponseSection(
		logFile, w.responseStatus, w.statusWritten, w.responseHeaders, responseBodyFile, nil, false,
	); errWrite != nil {
		return errWrite
	}
	return writeTrailingRouteTrace(logFile, routeTrace)
}

func (w *fileStreamingLogWriter) cleanupTempFiles() error {
	w.mu.Lock()
	defer w.mu.Unlock()

	path := w.responseBodyPath
	if path == "" {
		return nil
	}
	lease := w.responseBodyLease
	reserved := w.tempSlotReserved

	var cleanupErr error
	if lease != nil {
		if errRemove := os.Remove(path); errRemove != nil && !os.IsNotExist(errRemove) {
			cleanupErr = fmt.Errorf("failed to remove response body temp file: %w", errRemove)
			cleanupErr = errors.Join(cleanupErr, lease.release(false))
		} else {
			cleanupErr = lease.release(true)
		}
		w.responseBodyLease = nil
	} else {
		cleanupErr = removeRequestLogTempFile(path, "response body")
	}
	if cleanupErr != nil {
		if w.logger != nil {
			if reserved {
				w.logger.retainReservedTempFile(path)
			} else {
				w.logger.retainTempFileForCleanup(path)
			}
		}
		w.tempSlotReserved = false
		return cleanupErr
	}

	if w.logger != nil {
		w.logger.forgetTempFile(path)
		if reserved {
			w.logger.releaseActiveTempFileSlot()
		}
	}
	w.responseBodyPath = ""
	w.tempSlotReserved = false
	return nil
}

type homeStreamingLogWriter struct {
	mu             sync.Mutex
	closeOnce      sync.Once
	closed         bool
	closeErr       error
	clientProvider func() homeRequestLogClient
	logger         *FileRequestLogger

	url       string
	method    string
	timestamp time.Time

	requestHeaders map[string][]string
	requestID      string
	requestBody    []byte

	chunkChan chan []byte
	doneChan  chan struct{}

	responseStatus             int
	statusWritten              bool
	responseHeaders            map[string][]string
	responseBody               BoundedLogBuffer
	responseBodyBytes          int
	responseBodyTruncated      bool
	apiRequest                 []byte
	apiRequestSource           *FileBodySource
	apiResponse                []byte
	apiResponseSource          *FileBodySource
	apiWebsocketTimeline       []byte
	apiWebsocketTimelineSource *FileBodySource
	requestTimeline            []byte
	apiResponseTimestamp       time.Time
	firstChunkTimestamp        time.Time
}

func newHomeStreamingLogWriter(
	url, method string,
	headers map[string][]string,
	body []byte,
	requestID string,
	clientProvider func() homeRequestLogClient,
	logger *FileRequestLogger,
) *homeStreamingLogWriter {
	writer := &homeStreamingLogWriter{
		clientProvider: clientProvider,
		logger:         logger,
		url:            url,
		method:         method,
		timestamp:      time.Now(),
		requestHeaders: cloneHeaders(headers),
		requestID:      strings.TrimSpace(requestID),
		requestBody:    cloneBoundedLogPayload(body),
		chunkChan:      make(chan []byte, 100),
		doneChan:       make(chan struct{}),
	}
	go writer.asyncWriter()
	return writer
}

func (w *homeStreamingLogWriter) asyncWriter() {
	defer close(w.doneChan)
	for chunk := range w.chunkChan {
		if len(chunk) == 0 {
			continue
		}
		w.responseBody.Append(chunk)
	}
}

func (w *homeStreamingLogWriter) WriteChunkAsync(chunk []byte) {
	if w == nil || len(chunk) == 0 {
		return
	}

	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed || w.chunkChan == nil || w.responseBodyTruncated {
		return
	}
	if len(chunk) > maxStreamingResponseBodyBytes-w.responseBodyBytes {
		w.responseBodyTruncated = true
		log.Warn("home request log streaming response exceeded capture limit; response body omitted")
		return
	}
	chunkCopy := bytes.Clone(chunk)
	select {
	case w.chunkChan <- chunkCopy:
		w.responseBodyBytes += len(chunkCopy)
	default:
		log.Warn("home request log streaming buffer full, dropped response chunk")
	}
}

func (w *homeStreamingLogWriter) WriteStatus(status int, headers map[string][]string) error {
	if w == nil || status == 0 {
		return nil
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
		return errStreamingLogWriterClosed
	}
	w.responseStatus = status
	w.statusWritten = true
	w.responseHeaders = cloneHeaders(headers)
	return nil
}

func (w *homeStreamingLogWriter) WriteAPIRequest(apiRequest []byte) error {
	if w == nil || len(apiRequest) == 0 {
		return nil
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
		return errStreamingLogWriterClosed
	}
	w.apiRequest = cloneBoundedLogPayload(apiRequest)
	return nil
}

func (w *homeStreamingLogWriter) WriteAPIResponse(apiResponse []byte) error {
	if w == nil || len(apiResponse) == 0 {
		return nil
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
		return errStreamingLogWriterClosed
	}
	w.apiResponse = cloneBoundedLogPayload(apiResponse)
	return nil
}

func (w *homeStreamingLogWriter) WriteAPIRequestSource(source *FileBodySource) error {
	if w == nil || source == nil || !source.HasPayload() {
		return nil
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
		return errStreamingLogWriterClosed
	}
	w.apiRequestSource = source
	return nil
}

func (w *homeStreamingLogWriter) WriteAPIResponseSource(source *FileBodySource) error {
	if w == nil || source == nil || !source.HasPayload() {
		return nil
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
		return errStreamingLogWriterClosed
	}
	w.apiResponseSource = source
	return nil
}

func (w *homeStreamingLogWriter) WriteAPIWebsocketTimeline(apiWebsocketTimeline []byte) error {
	if w == nil || len(apiWebsocketTimeline) == 0 {
		return nil
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
		return errStreamingLogWriterClosed
	}
	w.apiWebsocketTimeline = cloneBoundedLogPayload(apiWebsocketTimeline)
	return nil
}

func (w *homeStreamingLogWriter) WriteAPIWebsocketTimelineSource(source *FileBodySource) error {
	if w == nil || source == nil || !source.HasPayload() {
		return nil
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
		return errStreamingLogWriterClosed
	}
	w.apiWebsocketTimelineSource = source
	return nil
}

func (w *homeStreamingLogWriter) SetFirstChunkTimestamp(timestamp time.Time) {
	if w == nil || timestamp.IsZero() {
		return
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if !w.closed {
		w.firstChunkTimestamp = timestamp
		w.apiResponseTimestamp = timestamp
	}
}

func (w *homeStreamingLogWriter) WriteRequestTimeline(requestTimeline []byte) error {
	if w == nil || len(requestTimeline) == 0 {
		return nil
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
		return errStreamingLogWriterClosed
	}
	w.requestTimeline = cloneBoundedLogPayload(requestTimeline)
	return nil
}

func (w *homeStreamingLogWriter) Close() error {
	if w == nil {
		return nil
	}
	w.closeOnce.Do(
		func() {
			w.closeErr = w.close()
			if w.logger == nil {
				return
			}
			if w.closeErr != nil {
				w.logger.recordRequestLogPublishFailed(w.requestID, w.closeErr)
				return
			}
			w.logger.recordRequestLogAvailable(w.requestID, RequestLogLocationHome, "")
		},
	)
	return errors.Join(
		w.closeErr,
		cleanupFileBodySources(
			w.apiRequestSource,
			w.apiResponseSource,
			w.apiWebsocketTimelineSource,
		),
	)
}

func (w *homeStreamingLogWriter) close() error {
	w.mu.Lock()
	w.closed = true
	chunkChan := w.chunkChan
	doneChan := w.doneChan
	w.mu.Unlock()

	if chunkChan != nil {
		close(chunkChan)
	}
	if doneChan != nil {
		<-doneChan
	}
	client := currentHomeRequestLogClient()
	if w.clientProvider != nil {
		client = w.clientProvider()
	}
	if client == nil {
		return fmt.Errorf("home request log client unavailable")
	}
	if !client.HeartbeatOK() {
		return fmt.Errorf("home request log heartbeat unhealthy")
	}

	buf := NewBoundedLogBuffer()
	timeline, routeTrace := splitRequestTimelinePayload(w.requestTimeline)
	if errWrite := writeRequestInfoWithBody(
		buf, w.url, w.method, w.requestHeaders, w.requestBody, w.timestamp,
	); errWrite != nil {
		return errWrite
	}
	if errWrite := writeLogPayload(buf, timeline); errWrite != nil {
		return errWrite
	}
	if errWrite := writeAPISectionWithSource(
		buf, "=== API WEBSOCKET TIMELINE ===\n", "=== API WEBSOCKET TIMELINE", w.apiWebsocketTimeline,
		w.apiWebsocketTimelineSource, time.Time{},
	); errWrite != nil {
		return errWrite
	}
	if errWrite := writePreformattedAPISectionWithSource(
		buf, "=== API REQUEST ===\n", "=== API REQUEST", w.apiRequest, w.apiRequestSource, time.Time{},
	); errWrite != nil {
		return errWrite
	}
	if errWrite := writePreformattedAPISectionWithSource(
		buf, "=== API RESPONSE ===\n", "=== API RESPONSE", w.apiResponse, w.apiResponseSource,
		w.apiResponseTimestamp,
	); errWrite != nil {
		return errWrite
	}
	responseBody := w.responseBody.Bytes()
	var responseReader io.Reader = bytes.NewReader(responseBody)
	if w.responseBodyTruncated {
		responseReader = nil
	}
	if errWrite := writeResponseSection(
		buf, w.responseStatus, w.statusWritten, w.responseHeaders, responseReader, nil, false,
	); errWrite != nil {
		return errWrite
	}
	if w.responseBodyTruncated {
		if _, errWrite := buf.Write(truncatedInMemoryRequestLog); errWrite != nil {
			return errWrite
		}
	}
	if len(routeTrace) > 0 && buf.Truncated() {
		buf = NewBoundedLogBuffer()
		buf.Append(truncatedInMemoryRequestLog)
	}
	if errWrite := writeTrailingRouteTrace(buf, routeTrace); errWrite != nil {
		return errWrite
	}

	payload := homeRequestLogPayload{
		Headers:    cloneMaskedHeaders(w.requestHeaders),
		RequestID:  w.requestID,
		RequestLog: buf.String(),
	}
	raw, errMarshal := json.Marshal(&payload)
	if errMarshal != nil {
		return errMarshal
	}
	return client.RPushRequestLog(context.Background(), raw)
}

// noOpStreamingLogWriter is a no-operation implementation for when logging is disabled.
// It implements the StreamingLogWriter interface but performs no actual logging operations.
type noOpStreamingLogWriter struct{}

// WriteChunkAsync is a no-op implementation that does nothing.
//
// Parameters:
//   - chunk: The response chunk (ignored)
func (w *noOpStreamingLogWriter) WriteChunkAsync(_ []byte) {}

// WriteStatus is a no-op implementation that does nothing and always returns nil.
//
// Parameters:
//   - status: The response status code (ignored)
//   - headers: The response headers (ignored)
//
// Returns:
//   - error: Always returns nil
func (w *noOpStreamingLogWriter) WriteStatus(_ int, _ map[string][]string) error {
	return nil
}

// WriteAPIRequest is a no-op implementation that does nothing and always returns nil.
//
// Parameters:
//   - apiRequest: The API request data (ignored)
//
// Returns:
//   - error: Always returns nil
func (w *noOpStreamingLogWriter) WriteAPIRequest(_ []byte) error {
	return nil
}

// WriteAPIResponse is a no-op implementation that does nothing and always returns nil.
//
// Parameters:
//   - apiResponse: The API response data (ignored)
//
// Returns:
//   - error: Always returns nil
func (w *noOpStreamingLogWriter) WriteAPIResponse(_ []byte) error {
	return nil
}

func (w *noOpStreamingLogWriter) WriteAPIWebsocketTimeline(_ []byte) error {
	return nil
}

func (w *noOpStreamingLogWriter) SetFirstChunkTimestamp(_ time.Time) {}

func (w *noOpStreamingLogWriter) WriteRequestTimeline(_ []byte) error { return nil }

// Close is a no-op implementation that does nothing and always returns nil.
//
// Returns:
//   - error: Always returns nil
func (w *noOpStreamingLogWriter) Close() error { return nil }
