package logging

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

type blockingBuffer struct {
	bytes.Buffer
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

type sourcePayloadFailWriter struct {
	bytes.Buffer
	err error
}

func (w *sourcePayloadFailWriter) Write(data []byte) (int, error) {
	if bytes.Contains(data, []byte("source-prefix")) {
		n := min(3, len(data))
		_, _ = w.Buffer.Write(data[:n])
		return n, w.err
	}
	return w.Buffer.Write(data)
}

func (w *blockingBuffer) Write(data []byte) (int, error) {
	w.once.Do(
		func() {
			close(w.entered)
			<-w.release
		},
	)
	return w.Buffer.Write(data)
}

func TestFileBodySourceWritePayloadToSerializesWithCleanup(t *testing.T) {
	source, errSource := NewFileBodySourceInDir(t.TempDir(), "cleanup-race")
	if errSource != nil {
		t.Fatalf("NewFileBodySourceInDir: %v", errSource)
	}
	if errAppend := source.AppendBytes([]byte(`{"model":"gpt-5","api_key":"source-secret"}`)); errAppend != nil {
		t.Fatalf("AppendBytes: %v", errAppend)
	}
	writer := &blockingBuffer{entered: make(chan struct{}), release: make(chan struct{})}
	writeDone := make(chan error, 1)
	go func() {
		writeDone <- source.WritePayloadTo(writer)
	}()
	<-writer.entered

	cleanupDone := make(chan error, 1)
	go func() {
		cleanupDone <- source.Cleanup()
	}()
	select {
	case errCleanup := <-cleanupDone:
		t.Fatalf("Cleanup completed during WritePayloadTo: %v", errCleanup)
	case <-time.After(20 * time.Millisecond):
	}
	close(writer.release)
	if errWrite := <-writeDone; errWrite != nil {
		t.Fatalf("WritePayloadTo: %v", errWrite)
	}
	if errCleanup := <-cleanupDone; errCleanup != nil {
		t.Fatalf("Cleanup: %v", errCleanup)
	}
	if got := writer.String(); !strings.Contains(got, "gpt-5") || strings.Contains(got, "source-secret") {
		t.Fatalf("unexpected payload: %s", got)
	}
	if errWrite := source.WritePayloadTo(&bytes.Buffer{}); errWrite == nil || !strings.Contains(
		errWrite.Error(), "cleaned",
	) {
		t.Fatalf("WritePayloadTo after cleanup error = %v, want cleaned error", errWrite)
	}
}

func TestFileBodySourceAppendPartSerializesWithCleanup(t *testing.T) {
	source, errSource := NewFileBodySourceInDir(t.TempDir(), "append-cleanup-race")
	if errSource != nil {
		t.Fatalf("NewFileBodySourceInDir: %v", errSource)
	}
	entered := make(chan struct{})
	release := make(chan struct{})
	appendDone := make(chan error, 1)
	go func() {
		appendDone <- source.appendPart(
			[]byte("source payload"),
			func(writer io.Writer, data []byte, prependNewline bool) error {
				close(entered)
				<-release
				return writeLogPart(writer, data, prependNewline)
			},
		)
	}()
	<-entered

	cleanupDone := make(chan error, 1)
	go func() {
		cleanupDone <- source.Cleanup()
	}()
	select {
	case errCleanup := <-cleanupDone:
		t.Fatalf("Cleanup completed during AppendPart: %v", errCleanup)
	case <-time.After(20 * time.Millisecond):
	}
	close(release)
	if errAppend := <-appendDone; errAppend != nil {
		t.Fatalf("AppendPart: %v", errAppend)
	}
	if errCleanup := <-cleanupDone; errCleanup != nil {
		t.Fatalf("Cleanup: %v", errCleanup)
	}
	if source.HasPayload() {
		t.Fatal("source still has payload after cleanup")
	}
}

func TestFileBodySourceAppendBytesFailureKeepsConfirmedPayload(t *testing.T) {
	source, errSource := NewFileBodySourceInDir(t.TempDir(), "append-bytes-failure")
	if errSource != nil {
		t.Fatalf("NewFileBodySourceInDir: %v", errSource)
	}
	defer func() { _ = source.Cleanup() }()
	if errAppend := source.AppendBytes([]byte("confirmed")); errAppend != nil {
		t.Fatalf("AppendBytes confirmed: %v", errAppend)
	}

	injectedErr := errors.New("injected partial write")
	errAppend := source.appendBytes(
		[]byte("discarded"),
		func(writer io.Writer, data []byte) error {
			if _, errWrite := writer.Write(data[:3]); errWrite != nil {
				return errWrite
			}
			return injectedErr
		},
	)
	if !errors.Is(errAppend, injectedErr) {
		t.Fatalf("appendBytes error = %v, want injected error", errAppend)
	}
	if errAppend = source.AppendBytes([]byte("-next")); errAppend != nil {
		t.Fatalf("AppendBytes after failure: %v", errAppend)
	}
	got, errBytes := source.Bytes()
	if errBytes != nil {
		t.Fatalf("Bytes: %v", errBytes)
	}
	if string(got) != "confirmed-next" {
		t.Fatalf("payload after partial failure = %q, want %q", got, "confirmed-next")
	}
}

func TestFileBodySourceAppendPartFailureExcludesPartialPart(t *testing.T) {
	source, errSource := NewFileBodySourceInDir(t.TempDir(), "append-part-failure")
	if errSource != nil {
		t.Fatalf("NewFileBodySourceInDir: %v", errSource)
	}
	defer func() { _ = source.Cleanup() }()
	if errAppend := source.AppendPart([]byte("first")); errAppend != nil {
		t.Fatalf("AppendPart first: %v", errAppend)
	}

	injectedErr := errors.New("injected partial part")
	errAppend := source.appendPart(
		[]byte("discarded"),
		func(writer io.Writer, data []byte, _ bool) error {
			if _, errWrite := writer.Write(data[:3]); errWrite != nil {
				return errWrite
			}
			return injectedErr
		},
	)
	if !errors.Is(errAppend, injectedErr) {
		t.Fatalf("appendPart error = %v, want injected error", errAppend)
	}
	if errAppend = source.AppendPart([]byte("third")); errAppend != nil {
		t.Fatalf("AppendPart third: %v", errAppend)
	}
	got, errBytes := source.Bytes()
	if errBytes != nil {
		t.Fatalf("Bytes: %v", errBytes)
	}
	if strings.Contains(string(got), "dis") || !strings.Contains(string(got), "first") ||
		!strings.Contains(string(got), "third") {
		t.Fatalf("payload includes failed part or loses confirmed parts: %q", got)
	}
}

func TestRequestLogSourcePrefixPrecedesMemorySuffix(t *testing.T) {
	tests := []struct {
		name  string
		write func(io.Writer, []byte, *FileBodySource) error
	}{
		{
			name: "formatted section",
			write: func(writer io.Writer, payload []byte, source *FileBodySource) error {
				return writeAPISectionWithSource(
					writer,
					"=== API WEBSOCKET TIMELINE ===\n",
					"=== API WEBSOCKET TIMELINE",
					payload,
					source,
					time.Time{},
				)
			},
		},
		{
			name: "preformatted section",
			write: func(writer io.Writer, payload []byte, source *FileBodySource) error {
				return writePreformattedAPISectionWithSource(
					writer,
					"=== API RESPONSE ===\n",
					"=== API RESPONSE",
					payload,
					source,
					time.Time{},
				)
			},
		},
	}
	for _, test := range tests {
		t.Run(
			test.name, func(t *testing.T) {
				source, errSource := NewFileBodySourceInDir(t.TempDir(), "source-prefix")
				if errSource != nil {
					t.Fatalf("NewFileBodySourceInDir: %v", errSource)
				}
				defer func() { _ = source.Cleanup() }()
				if errAppend := source.AppendBytes([]byte("source-prefix")); errAppend != nil {
					t.Fatalf("AppendBytes: %v", errAppend)
				}
				var output bytes.Buffer
				if errWrite := test.write(&output, []byte("memory-suffix"), source); errWrite != nil {
					t.Fatalf("write source section: %v", errWrite)
				}
				sourceIndex := strings.Index(output.String(), "source-prefix")
				memoryIndex := strings.Index(output.String(), "memory-suffix")
				if sourceIndex < 0 || memoryIndex <= sourceIndex {
					t.Fatalf("source and memory order = %q", output.String())
				}
			},
		)
	}
}

func TestRequestLogSourceOutputFailureStopsFinalWrite(t *testing.T) {
	source, errSource := NewFileBodySourceInDir(t.TempDir(), "source-output-failure")
	if errSource != nil {
		t.Fatalf("NewFileBodySourceInDir: %v", errSource)
	}
	defer func() { _ = source.Cleanup() }()
	if errAppend := source.AppendBytes([]byte("source-prefix")); errAppend != nil {
		t.Fatalf("AppendBytes: %v", errAppend)
	}
	injectedErr := errors.New("injected output failure")
	writer := &sourcePayloadFailWriter{err: injectedErr}
	errWrite := writeAPISectionWithSource(
		writer,
		"=== API REQUEST ===\n",
		"=== API REQUEST",
		[]byte("memory-suffix"),
		source,
		time.Time{},
	)
	if !errors.Is(errWrite, injectedErr) {
		t.Fatalf("writeAPISectionWithSource error = %v, want injected error", errWrite)
	}
	if strings.Contains(writer.String(), "[source prefix unavailable: read failed]") ||
		strings.Contains(writer.String(), "memory-suffix") {
		t.Fatalf("output failure was treated as source read failure: %q", writer.String())
	}
}

func TestRequestLogSourceReadFailureKeepsMemorySuffix(t *testing.T) {
	tests := []struct {
		name  string
		write func(io.Writer, []byte, *FileBodySource) error
	}{
		{
			name: "formatted section",
			write: func(writer io.Writer, payload []byte, source *FileBodySource) error {
				return writeAPISectionWithSource(
					writer,
					"=== API WEBSOCKET TIMELINE ===\n",
					"=== API WEBSOCKET TIMELINE",
					payload,
					source,
					time.Time{},
				)
			},
		},
		{
			name: "preformatted section",
			write: func(writer io.Writer, payload []byte, source *FileBodySource) error {
				return writePreformattedAPISectionWithSource(
					writer,
					"=== API RESPONSE ===\n",
					"=== API RESPONSE",
					payload,
					source,
					time.Time{},
				)
			},
		},
	}
	for _, test := range tests {
		t.Run(
			test.name, func(t *testing.T) {
				source, errSource := NewFileBodySourceInDir(t.TempDir(), "unreadable-source")
				if errSource != nil {
					t.Fatalf("NewFileBodySourceInDir: %v", errSource)
				}
				defer func() { _ = source.Cleanup() }()
				if errAppend := source.AppendBytes([]byte("source-prefix")); errAppend != nil {
					t.Fatalf("AppendBytes: %v", errAppend)
				}
				paths := source.Paths()
				if len(paths) != 1 {
					t.Fatalf("source paths = %v, want one path", paths)
				}
				if errRemove := os.Remove(paths[0]); errRemove != nil {
					t.Fatalf("remove source part: %v", errRemove)
				}

				var output bytes.Buffer
				if errWrite := test.write(&output, []byte("memory-suffix"), source); errWrite != nil {
					t.Fatalf("write source section: %v", errWrite)
				}
				got := output.String()
				markerIndex := strings.Index(got, "[source prefix unavailable: read failed]")
				memoryIndex := strings.Index(got, "memory-suffix")
				if markerIndex < 0 || memoryIndex <= markerIndex {
					t.Fatalf("source failure output = %q", got)
				}
			},
		)
	}
}

func TestPendingRequestLogLeaseIsNotCleanedBeforeStaleAge(t *testing.T) {
	logsDir := t.TempDir()
	file, errCreate := os.CreateTemp(logsDir, ".response-body-lease-pending-")
	if errCreate != nil {
		t.Fatalf("CreateTemp: %v", errCreate)
	}
	path := file.Name()
	if errClose := file.Close(); errClose != nil {
		t.Fatalf("Close: %v", errClose)
	}
	if errCleanup := cleanupPendingRequestLogLease(path); errCleanup != nil {
		t.Fatalf("cleanup fresh pending lease: %v", errCleanup)
	}
	if _, errStat := os.Stat(path); errStat != nil {
		t.Fatalf("fresh pending lease was removed: %v", errStat)
	}
	staleTime := time.Now().Add(-2 * requestLogPendingLeaseStaleAge)
	if errTime := os.Chtimes(path, staleTime, staleTime); errTime != nil {
		t.Fatalf("age pending lease: %v", errTime)
	}
	if errCleanup := cleanupPendingRequestLogLease(path); errCleanup != nil {
		t.Fatalf("cleanup stale pending lease: %v", errCleanup)
	}
	if _, errStat := os.Stat(path); !os.IsNotExist(errStat) {
		t.Fatalf("stale pending lease still exists, stat error = %v", errStat)
	}
}

func TestRequestLogLeaseReleaseRemovesRetainedStagingLink(t *testing.T) {
	logsDir := t.TempDir()
	file, errCreate := os.CreateTemp(logsDir, ".response-body-lease-pending-")
	if errCreate != nil {
		t.Fatalf("CreateTemp: %v", errCreate)
	}
	stagingPath := file.Name()
	if errLock := lockRequestLogFile(file); errLock != nil {
		t.Fatalf("lockRequestLogFile: %v", errLock)
	}
	canonicalPath := filepath.Join(logsDir, "response-body-retained.lock")
	if errLink := os.Link(stagingPath, canonicalPath); errLink != nil {
		t.Fatalf("Link: %v", errLink)
	}
	identity, errStat := file.Stat()
	if errStat != nil {
		t.Fatalf("Stat: %v", errStat)
	}
	registerActiveRequestLogLease(stagingPath, identity)
	registerActiveRequestLogLease(canonicalPath, identity)
	lease := &requestLogFileLease{
		file:        file,
		path:        canonicalPath,
		stagingPath: stagingPath,
		identity:    identity,
	}
	if errRelease := lease.release(true); errRelease != nil {
		t.Fatalf("release: %v", errRelease)
	}
	for _, path := range []string{stagingPath, canonicalPath} {
		if _, errStat := os.Stat(path); !os.IsNotExist(errStat) {
			t.Fatalf("lease path %q still exists, stat error = %v", path, errStat)
		}
		if isActiveRequestLogLease(path, identity) {
			t.Fatalf("lease path %q remains active in registry", path)
		}
	}
}

func TestRequestLogLeaseRegistryRecognizesPathAliases(t *testing.T) {
	logsDir := t.TempDir()
	lease, errLease := createRequestLogTempLease(logsDir, "response-body-")
	if errLease != nil {
		t.Fatalf("createRequestLogTempLease: %v", errLease)
	}
	defer func() { _ = lease.release(true) }()
	aliasRoot := t.TempDir()
	aliasDir := filepath.Join(aliasRoot, "logs-link")
	if errLink := os.Symlink(logsDir, aliasDir); errLink != nil {
		t.Skipf("create directory symlink: %v", errLink)
	}
	aliasPath := filepath.Join(aliasDir, filepath.Base(lease.path))
	aliasIdentity, errStat := os.Stat(aliasPath)
	if errStat != nil {
		t.Fatalf("stat lease alias: %v", errStat)
	}
	if !isActiveRequestLogLease(aliasPath, aliasIdentity) {
		t.Fatal("active lease registry did not recognize the path alias")
	}
	_, active, errAcquire := tryAcquireRequestLogFileLease(aliasPath)
	if errAcquire != nil {
		t.Fatalf("tryAcquireRequestLogFileLease: %v", errAcquire)
	}
	if !active {
		t.Fatal("active lease was not recognized through a path alias")
	}
}

func TestRequestLogTempLeasePublishesLockedCanonicalPath(t *testing.T) {
	logsDir := t.TempDir()
	lease, errLease := createRequestLogTempLease(logsDir, "response-body-")
	if errLease != nil {
		t.Fatalf("createRequestLogTempLease: %v", errLease)
	}
	defer func() { _ = lease.release(true) }()
	if strings.HasPrefix(filepath.Base(lease.path), ".") {
		t.Fatalf("published lease path is hidden pending path: %q", lease.path)
	}
	if _, errStat := os.Stat(lease.path); errStat != nil {
		t.Fatalf("published lease path is unavailable: %v", errStat)
	}
	_, active, errAcquire := tryAcquireRequestLogFileLease(lease.path)
	if errAcquire != nil {
		t.Fatalf("tryAcquireRequestLogFileLease: %v", errAcquire)
	}
	if !active {
		t.Fatal("published lease path was visible without its active lock")
	}
	entries, errRead := os.ReadDir(logsDir)
	if errRead != nil {
		t.Fatalf("ReadDir: %v", errRead)
	}
	pendingFound := false
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".response-body-lease-pending-") {
			pendingFound = true
			if lease.stagingPath == "" || filepath.Base(lease.stagingPath) != entry.Name() {
				t.Fatalf("unexpected retained staging lease: %q", entry.Name())
			}
		}
	}
	if (lease.stagingPath != "") != pendingFound {
		t.Fatalf("staging path = %q, pending found = %t", lease.stagingPath, pendingFound)
	}
}

func TestWriteRequestLogAtomicallyEnforcesFinalArtifactCapacity(t *testing.T) {
	logsDir := t.TempDir()
	logger := &FileRequestLogger{logsDir: logsDir}
	for i := range maxPendingRequestLogTempFiles {
		if !logger.reserveFinalTempFileSlot() {
			t.Fatalf("reserve final temp slot %d returned false", i)
		}
	}
	defer func() {
		for range maxPendingRequestLogTempFiles {
			logger.releaseActiveFinalTempFileSlot()
		}
	}()
	errWrite := writeRequestLogAtomically(
		logger,
		filepath.Join(logsDir, "request.log"),
		func(io.Writer) error { return nil },
	)
	if errWrite == nil || !strings.Contains(errWrite.Error(), "publish cleanup capacity reached") {
		t.Fatalf("writeRequestLogAtomically error = %v", errWrite)
	}
	assertNoRequestLogPublishArtifacts(t, logsDir)
}

func TestWriteRequestLogAtomicallyPublishesCompleteFile(t *testing.T) {
	logsDir := t.TempDir()
	path := filepath.Join(logsDir, "request.log")
	if errWrite := writeRequestLogAtomically(
		nil, path, func(writer io.Writer) error {
			_, err := writer.Write([]byte("new-content"))
			return err
		},
	); errWrite != nil {
		t.Fatalf("writeRequestLogAtomically: %v", errWrite)
	}
	got, errRead := os.ReadFile(path)
	if errRead != nil {
		t.Fatalf("read published request log: %v", errRead)
	}
	if string(got) != "new-content" {
		t.Fatalf("published request log = %q", got)
	}
	assertNoRequestLogPublishArtifacts(t, logsDir)
}

func TestWriteRequestLogAtomicallyDoesNotReplaceExistingFile(t *testing.T) {
	logsDir := t.TempDir()
	path := filepath.Join(logsDir, "request.log")
	if errWrite := os.WriteFile(path, []byte("old-content"), 0o600); errWrite != nil {
		t.Fatalf("write old request log: %v", errWrite)
	}
	published, errWrite := writeRequestLogAtomicallyWithResult(
		nil, path, func(writer io.Writer) error {
			_, err := writer.Write([]byte("new-content"))
			return err
		},
	)
	if errWrite == nil || !errors.Is(errWrite, os.ErrExist) {
		t.Fatalf("writeRequestLogAtomicallyWithResult error = %v, want file exists", errWrite)
	}
	if published {
		t.Fatal("writeRequestLogAtomicallyWithResult reported an existing file as published")
	}
	got, errRead := os.ReadFile(path)
	if errRead != nil {
		t.Fatalf("read existing request log: %v", errRead)
	}
	if string(got) != "old-content" {
		t.Fatalf("existing request log = %q", got)
	}
	assertNoRequestLogPublishArtifacts(t, logsDir)
}

func TestWriteRequestLogAtomicallyKeepsLeaseWhenTempCleanupFails(t *testing.T) {
	logsDir := t.TempDir()
	logger := &FileRequestLogger{logsDir: logsDir}
	path := filepath.Join(logsDir, "request.log")
	injectedErr := errors.New("injected request log write failure")
	injectedCleanupErr := errors.New("injected request log cleanup failure")
	var tempPath string
	failCleanup := true
	originalRemove := removeRequestLogPath
	removeRequestLogPath = func(removePath string) error {
		if failCleanup && removePath == tempPath {
			return injectedCleanupErr
		}
		return os.Remove(removePath)
	}
	defer func() {
		removeRequestLogPath = originalRemove
	}()
	errWrite := writeRequestLogAtomically(
		logger, path, func(writer io.Writer) error {
			file, ok := writer.(*os.File)
			if !ok {
				return fmt.Errorf("unexpected writer type %T", writer)
			}
			tempPath = file.Name()
			if _, err := writer.Write([]byte("partial-content")); err != nil {
				return err
			}
			return injectedErr
		},
	)
	if !errors.Is(errWrite, injectedErr) {
		t.Fatalf("writeRequestLogAtomically error = %v, want injected error", errWrite)
	}
	leasePath := strings.TrimSuffix(tempPath, ".tmp") + responseBodyLeaseSuffix
	if _, errStat := os.Stat(leasePath); errStat != nil {
		t.Fatalf("publish lease was removed after temp cleanup failure: %v", errStat)
	}
	if pending := logger.pendingFinalTempFileCount(); pending != 1 {
		t.Fatalf("pending final temp files = %d, want 1", pending)
	}
	if errRetry := logger.retryPendingTempFileCleanup(); errRetry == nil {
		t.Fatal("pending cleanup retry returned nil while temp deletion still fails")
	}
	if _, errStat := os.Stat(leasePath); errStat != nil {
		t.Fatalf("pending cleanup retry removed lease before temp: %v", errStat)
	}
	failCleanup = false
	if errRetry := logger.retryPendingTempFileCleanup(); errRetry != nil {
		t.Fatalf("retry pending cleanup: %v", errRetry)
	}
	if pending := logger.pendingFinalTempFileCount(); pending != 0 {
		t.Fatalf("pending final temp files = %d, want 0", pending)
	}
	if _, errStat := os.Stat(leasePath); !os.IsNotExist(errStat) {
		t.Fatalf("publish lease still exists, stat error = %v", errStat)
	}
}

func TestWriteRequestLogAtomicallyPreservesExistingFileOnFailure(t *testing.T) {
	logsDir := t.TempDir()
	path := filepath.Join(logsDir, "request.log")
	if errWrite := os.WriteFile(path, []byte("old-content"), 0o600); errWrite != nil {
		t.Fatalf("write old request log: %v", errWrite)
	}
	injectedErr := errors.New("injected request log write failure")
	errWrite := writeRequestLogAtomically(
		nil, path, func(writer io.Writer) error {
			if _, err := writer.Write([]byte("partial-content")); err != nil {
				return err
			}
			return injectedErr
		},
	)
	if !errors.Is(errWrite, injectedErr) {
		t.Fatalf("writeRequestLogAtomically error = %v, want injected error", errWrite)
	}
	got, errRead := os.ReadFile(path)
	if errRead != nil {
		t.Fatalf("read existing request log: %v", errRead)
	}
	if string(got) != "old-content" {
		t.Fatalf("existing request log = %q", got)
	}
	assertNoRequestLogPublishArtifacts(t, logsDir)
}

func assertNoRequestLogPublishArtifacts(t *testing.T, logsDir string) {
	t.Helper()
	entries, errRead := os.ReadDir(logsDir)
	if errRead != nil {
		t.Fatalf("ReadDir: %v", errRead)
	}
	for _, entry := range entries {
		if strings.Contains(entry.Name(), "request-log-final-") {
			t.Fatalf("request log publish artifact remains: %q", entry.Name())
		}
	}
}

func TestFileBodySourceCleanupCanRetry(t *testing.T) {
	source, sourceDir, partPath, nestedPath := newCleanupFailureSource(t)

	errCleanup := source.Cleanup()
	if errCleanup == nil {
		t.Fatal("Cleanup returned nil error for non-empty part directory")
	}
	if !source.HasPayload() {
		t.Fatal("source discarded failed cleanup state")
	}

	removeCleanupFailure(t, partPath, nestedPath)
	if errCleanup = source.Cleanup(); errCleanup != nil {
		t.Fatalf("retry Cleanup: %v", errCleanup)
	}
	if source.HasPayload() {
		t.Fatal("source still has payload after successful retry")
	}
	if _, errStat := os.Stat(sourceDir); !os.IsNotExist(errStat) {
		t.Fatalf("source directory %q still exists, stat error = %v", sourceDir, errStat)
	}
}

func TestFileBodySourceCleanupKeepsOwnerLeaseUntilDirectoryIsReady(t *testing.T) {
	source, errSource := NewFileBodySourceInDir(t.TempDir(), "owner-last")
	if errSource != nil {
		t.Fatalf("NewFileBodySourceInDir: %v", errSource)
	}
	if errAppend := source.AppendBytes([]byte("source payload")); errAppend != nil {
		t.Fatalf("AppendBytes: %v", errAppend)
	}
	paths := source.Paths()
	if len(paths) != 1 {
		t.Fatalf("source paths = %v, want one path", paths)
	}
	sourceDir := filepath.Dir(paths[0])
	blockerPath := filepath.Join(sourceDir, "unexpected-entry")
	if errWrite := os.WriteFile(blockerPath, []byte("block cleanup"), 0o600); errWrite != nil {
		t.Fatalf("write cleanup blocker: %v", errWrite)
	}
	if errCleanup := source.Cleanup(); errCleanup == nil {
		t.Fatal("Cleanup returned nil with unexpected directory entry")
	}
	ownerPath := filepath.Join(sourceDir, fileBodySourceOwnerLeaseName)
	if _, errStat := os.Stat(ownerPath); errStat != nil {
		t.Fatalf("owner lease was removed before directory cleanup: %v", errStat)
	}
	if errRemove := os.Remove(blockerPath); errRemove != nil {
		t.Fatalf("remove cleanup blocker: %v", errRemove)
	}
	if errCleanup := source.Cleanup(); errCleanup != nil {
		t.Fatalf("retry Cleanup: %v", errCleanup)
	}
	if _, errStat := os.Stat(sourceDir); !os.IsNotExist(errStat) {
		t.Fatalf("source directory still exists, stat error = %v", errStat)
	}
}

func TestStreamingLogWriterCloseReturnsSourceCleanupError(t *testing.T) {
	original := currentHomeRequestLogClient
	defer func() {
		currentHomeRequestLogClient = original
	}()
	currentHomeRequestLogClient = func() homeRequestLogClient {
		return &stubHomeRequestLogClient{heartbeatOK: true}
	}

	tests := []struct {
		name      string
		newWriter func(*FileBodySource) StreamingLogWriter
	}{
		{
			name: "file",
			newWriter: func(source *FileBodySource) StreamingLogWriter {
				return &fileStreamingLogWriter{apiRequestSource: source}
			},
		},
		{
			name: "home",
			newWriter: func(source *FileBodySource) StreamingLogWriter {
				return &homeStreamingLogWriter{apiRequestSource: source}
			},
		},
	}
	for _, test := range tests {
		t.Run(
			test.name, func(t *testing.T) {
				source, sourceDir, partPath, nestedPath := newCleanupFailureSource(t)
				writer := test.newWriter(source)
				errClose := writer.Close()
				if errClose == nil {
					t.Fatal("Close returned nil cleanup error")
				}
				if !strings.Contains(errClose.Error(), partPath) {
					t.Fatalf("Close error = %v, want failed source path", errClose)
				}
				if !source.HasPayload() {
					t.Fatal("writer discarded failed cleanup state")
				}

				removeCleanupFailure(t, partPath, nestedPath)
				errClose = writer.Close()
				if test.name == "file" && errClose != nil {
					t.Fatalf("retry Close: %v", errClose)
				}
				if source.HasPayload() {
					t.Fatal("source still has payload after cleanup retry")
				}
				if _, errStat := os.Stat(sourceDir); !os.IsNotExist(errStat) {
					t.Fatalf("source directory %q still exists, stat error = %v", sourceDir, errStat)
				}
			},
		)
	}
}

func TestFileRequestLoggerPreservesLeaseLessResponseTempFiles(t *testing.T) {
	logsDir := t.TempDir()
	paths := []string{
		filepath.Join(logsDir, "response-body-old.tmp"),
		filepath.Join(logsDir, "response-body-fresh.tmp"),
	}
	for _, path := range paths {
		if errWrite := os.WriteFile(path, []byte("legacy response"), 0o600); errWrite != nil {
			t.Fatalf("write lease-less response temp file: %v", errWrite)
		}
	}
	oldTime := time.Now().Add(-2 * time.Hour)
	if errTime := os.Chtimes(paths[0], oldTime, oldTime); errTime != nil {
		t.Fatalf("age response temp file: %v", errTime)
	}

	logger := NewFileRequestLogger(true, logsDir, "", 0)
	if logger.pendingTempFileCount() != 0 {
		t.Fatalf("pending temp files = %d, want 0", logger.pendingTempFileCount())
	}
	for _, path := range paths {
		if _, errStat := os.Stat(path); errStat != nil {
			t.Fatalf("lease-less response temp path %q was removed: %v", path, errStat)
		}
	}
}

func TestFileRequestLoggerDoesNotRecoverActiveResponseTempFile(t *testing.T) {
	logsDir := t.TempDir()
	logger := NewFileRequestLogger(true, logsDir, "", 0)
	writer, errWriter := logger.LogStreamingRequest(
		"/v1/responses",
		"POST",
		nil,
		[]byte(`{"stream":true}`),
		"active-response",
	)
	if errWriter != nil {
		t.Fatalf("LogStreamingRequest: %v", errWriter)
	}
	fileWriter, ok := writer.(*fileStreamingLogWriter)
	if !ok {
		t.Fatalf("streaming writer has unexpected type %T", writer)
	}
	responsePath := fileWriter.responseBodyPath
	leasePath := fileWriter.responseBodyLease.path

	_ = NewFileRequestLogger(true, logsDir, "", 0)
	if _, errStat := os.Stat(responsePath); errStat != nil {
		t.Fatalf("active response temp file was removed: %v", errStat)
	}
	if _, errStat := os.Stat(leasePath); errStat != nil {
		t.Fatalf("active response lease was removed: %v", errStat)
	}
	if errClose := writer.Close(); errClose != nil {
		t.Fatalf("Close: %v", errClose)
	}
}

func TestFileRequestLoggerKeepsActiveResponseLeaseWithoutTemp(t *testing.T) {
	logsDir := t.TempDir()
	lease, errLease := createRequestLogTempLease(logsDir, "response-body-")
	if errLease != nil {
		t.Fatalf("createRequestLogTempLease: %v", errLease)
	}
	leasePath := lease.path
	defer func() { _ = lease.release(true) }()

	_ = NewFileRequestLogger(true, logsDir, "", 0)
	if _, errStat := os.Stat(leasePath); errStat != nil {
		t.Fatalf("active response lease without temp was removed: %v", errStat)
	}
}

func TestFileRequestLoggerRecoversOrphanResponseLeaseWithoutTemp(t *testing.T) {
	logsDir := t.TempDir()
	lease, errLease := createRequestLogTempLease(logsDir, "response-body-")
	if errLease != nil {
		t.Fatalf("createRequestLogTempLease: %v", errLease)
	}
	leasePath := lease.path
	if errRelease := lease.release(false); errRelease != nil {
		t.Fatalf("release orphan lease: %v", errRelease)
	}

	_ = NewFileRequestLogger(true, logsDir, "", 0)
	if _, errStat := os.Stat(leasePath); !os.IsNotExist(errStat) {
		t.Fatalf("orphan response lease still exists, stat error = %v", errStat)
	}
}

func TestFileRequestLoggerOrphanLeaseCleanupReleasesPendingReservation(t *testing.T) {
	logsDir := t.TempDir()
	logger := NewFileRequestLogger(false, logsDir, "", 0)
	lease, errLease := createRequestLogTempLease(logsDir, "response-body-")
	if errLease != nil {
		t.Fatalf("createRequestLogTempLease: %v", errLease)
	}
	tempPath := strings.TrimSuffix(lease.path, responseBodyLeaseSuffix) + ".tmp"
	if !logger.retainTempFileForCleanup(tempPath) {
		t.Fatal("retainTempFileForCleanup returned false")
	}
	if errRelease := lease.release(false); errRelease != nil {
		t.Fatalf("release orphan lease: %v", errRelease)
	}

	if errCleanup := logger.cleanupDiscoveredResponseLease(lease.path); errCleanup != nil {
		t.Fatalf("cleanupDiscoveredResponseLease: %v", errCleanup)
	}
	if pending := logger.pendingTempFileCount(); pending != 0 {
		t.Fatalf("pending temp files = %d, want 0", pending)
	}
	if _, errStat := os.Stat(lease.path); !os.IsNotExist(errStat) {
		t.Fatalf("orphan response lease still exists, stat error = %v", errStat)
	}
}

func TestFileRequestLoggerRecoversFreshOrphanResponseTempFile(t *testing.T) {
	logsDir := t.TempDir()
	lease, errLease := createRequestLogTempLease(logsDir, "response-body-")
	if errLease != nil {
		t.Fatalf("createRequestLogTempLease: %v", errLease)
	}
	responsePath := strings.TrimSuffix(lease.path, responseBodyLeaseSuffix) + ".tmp"
	leasePath := lease.path
	if errWrite := os.WriteFile(responsePath, []byte("orphan response"), 0o600); errWrite != nil {
		t.Fatalf("write orphan response: %v", errWrite)
	}
	if errRelease := lease.release(false); errRelease != nil {
		t.Fatalf("release orphan lease: %v", errRelease)
	}

	_ = NewFileRequestLogger(true, logsDir, "", 0)
	if _, errStat := os.Stat(responsePath); !os.IsNotExist(errStat) {
		t.Fatalf("orphan response temp path still exists, stat error = %v", errStat)
	}
	if _, errStat := os.Stat(leasePath); !os.IsNotExist(errStat) {
		t.Fatalf("orphan response lease still exists, stat error = %v", errStat)
	}
}

func TestFileRequestLoggerDoesNotRecoverActiveFileBodySource(t *testing.T) {
	logsDir := t.TempDir()
	source, errSource := NewFileBodySourceInDir(logsDir, "active-source")
	if errSource != nil {
		t.Fatalf("NewFileBodySourceInDir: %v", errSource)
	}
	defer func() { _ = source.Cleanup() }()
	if errAppend := source.AppendBytes([]byte("active payload")); errAppend != nil {
		t.Fatalf("AppendBytes: %v", errAppend)
	}
	sourceDir := source.dir

	_ = NewFileRequestLogger(true, logsDir, "", 0)
	if _, errStat := os.Stat(sourceDir); errStat != nil {
		t.Fatalf("active source directory was removed: %v", errStat)
	}
}

func TestFileRequestLoggerRecoversFreshOrphanFileBodySource(t *testing.T) {
	logsDir := t.TempDir()
	source, errSource := NewFileBodySourceInDir(logsDir, "orphan-source")
	if errSource != nil {
		t.Fatalf("NewFileBodySourceInDir: %v", errSource)
	}
	defer func() { _ = source.Cleanup() }()
	if errAppend := source.AppendBytes([]byte("orphan payload")); errAppend != nil {
		t.Fatalf("AppendBytes: %v", errAppend)
	}
	sourceDir := source.dir
	if errRelease := source.ownerLease.release(false); errRelease != nil {
		t.Fatalf("release source owner lease: %v", errRelease)
	}

	_ = NewFileRequestLogger(true, logsDir, "", 0)
	if _, errStat := os.Stat(sourceDir); !os.IsNotExist(errStat) {
		t.Fatalf("orphan source directory still exists, stat error = %v", errStat)
	}
}

func TestFileRequestLoggerScansCleanupDirectoryInBatches(t *testing.T) {
	logsDir := t.TempDir()
	for i := range requestLogCleanupScanBatchSize + 10 {
		path := filepath.Join(logsDir, fmt.Sprintf("ordinary-%03d.log", i))
		if errWrite := os.WriteFile(path, []byte("log"), 0o600); errWrite != nil {
			t.Fatalf("write ordinary log %d: %v", i, errWrite)
		}
	}
	logger := NewFileRequestLogger(false, logsDir, "", 0)
	logger.cleanupRunMu.Lock()
	scanPending := logger.cleanupScanDir != nil
	logger.cleanupRunMu.Unlock()
	if !scanPending {
		t.Fatal("cleanup scan consumed more than one batch")
	}
	if errCleanup := logger.retryPendingCleanup(true); errCleanup != nil {
		t.Fatalf("retryPendingCleanup: %v", errCleanup)
	}
	logger.cleanupRunMu.Lock()
	scanPending = logger.cleanupScanDir != nil
	logger.cleanupRunMu.Unlock()
	if scanPending {
		t.Fatal("cleanup scan did not close directory at end of batch sequence")
	}
}

func TestFileRequestLoggerContinuesCleanupScanInBackground(t *testing.T) {
	logsDir := t.TempDir()
	for i := range requestLogCleanupScanBatchSize*2 + 10 {
		path := filepath.Join(logsDir, fmt.Sprintf("ordinary-%03d.log", i))
		if errWrite := os.WriteFile(path, []byte("log"), 0o600); errWrite != nil {
			t.Fatalf("write ordinary log %d: %v", i, errWrite)
		}
	}
	logger := NewFileRequestLogger(false, logsDir, "", 0)
	deadline := time.Now().Add(2 * time.Second)
	for {
		logger.cleanupRunMu.Lock()
		done := logger.cleanupScanDir == nil
		logger.cleanupRunMu.Unlock()
		if done && !logger.cleanupScanScheduled.Load() {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("background cleanup scan did not finish")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestFileRequestLoggerNewPendingCleanupKeepsExistingRetryDeadline(t *testing.T) {
	logger := &FileRequestLogger{}
	firstPath := filepath.Join(t.TempDir(), "first.tmp")
	secondPath := filepath.Join(t.TempDir(), "second.tmp")
	if !logger.retainTempFileForCleanup(firstPath) {
		t.Fatal("retain first temp file returned false")
	}
	deadline := time.Now().Add(requestLogCleanupRetryInterval)
	logger.cleanupMu.Lock()
	logger.nextCleanupAttempt = deadline
	logger.cleanupMu.Unlock()
	if !logger.retainTempFileForCleanup(secondPath) {
		t.Fatal("retain second temp file returned false")
	}
	logger.cleanupMu.Lock()
	got := logger.nextCleanupAttempt
	logger.cleanupMu.Unlock()
	if !got.Equal(deadline) {
		t.Fatalf("retry deadline = %s, want %s", got, deadline)
	}
}

func TestFileBodySourceCleanupQueueNewPendingKeepsExistingRetryDeadline(t *testing.T) {
	queue := &pendingFileBodySourceCleanup
	queue.cleanupRunMu.Lock()
	defer queue.cleanupRunMu.Unlock()
	queue.mu.Lock()
	originalSources := queue.sources
	originalDeadline := queue.nextCleanupAttempt
	queue.sources = make(map[*FileBodySource]struct{})
	queue.nextCleanupAttempt = time.Time{}
	queue.mu.Unlock()
	defer func() {
		queue.mu.Lock()
		queue.sources = originalSources
		queue.nextCleanupAttempt = originalDeadline
		queue.mu.Unlock()
	}()

	queue.Add(&FileBodySource{})
	deadline := time.Now().Add(requestLogCleanupRetryInterval)
	queue.mu.Lock()
	queue.nextCleanupAttempt = deadline
	queue.mu.Unlock()
	queue.Add(&FileBodySource{})
	queue.mu.Lock()
	got := queue.nextCleanupAttempt
	queue.mu.Unlock()
	if !got.Equal(deadline) {
		t.Fatalf("retry deadline = %s, want %s", got, deadline)
	}
}

func TestFileRequestLoggerRepeatsDiskCleanupDiscovery(t *testing.T) {
	logsDir := t.TempDir()
	logger := NewFileRequestLogger(false, logsDir, "", 0)

	responseLease, errLease := createRequestLogTempLease(logsDir, "response-body-")
	if errLease != nil {
		t.Fatalf("create response lease: %v", errLease)
	}
	responsePath := strings.TrimSuffix(responseLease.path, responseBodyLeaseSuffix) + ".tmp"
	if errWrite := os.WriteFile(responsePath, []byte("late orphan response"), 0o600); errWrite != nil {
		t.Fatalf("write late orphan response: %v", errWrite)
	}
	if errRelease := responseLease.release(false); errRelease != nil {
		t.Fatalf("release response lease: %v", errRelease)
	}
	source, errSource := NewFileBodySourceInDir(logsDir, "late-orphan")
	if errSource != nil {
		t.Fatalf("NewFileBodySourceInDir: %v", errSource)
	}
	defer func() { _ = source.Cleanup() }()
	if errAppend := source.AppendBytes([]byte("late orphan source")); errAppend != nil {
		t.Fatalf("AppendBytes: %v", errAppend)
	}
	sourceDir := source.dir
	if errRelease := source.ownerLease.release(false); errRelease != nil {
		t.Fatalf("release source lease: %v", errRelease)
	}
	logger.cleanupMu.Lock()
	logger.nextCleanupAttempt = time.Time{}
	logger.cleanupMu.Unlock()

	if errRetry := logger.RetryPendingCleanup(); errRetry != nil {
		t.Fatalf("RetryPendingCleanup: %v", errRetry)
	}
	if _, errStat := os.Stat(responsePath); !os.IsNotExist(errStat) {
		t.Fatalf("late orphan response temp path still exists, stat error = %v", errStat)
	}
	if _, errStat := os.Stat(sourceDir); !os.IsNotExist(errStat) {
		t.Fatalf("late orphan source directory still exists, stat error = %v", errStat)
	}
}

func TestLegacySDKRequestLogCleanupRecoversCrashOrphan(t *testing.T) {
	originalCleanup := legacySDKRequestLogCleanup
	legacySDKRequestLogCleanup = &FileRequestLogger{logsDir: t.TempDir()}
	defer func() {
		_ = legacySDKRequestLogCleanup.closeCleanupScanDir()
		legacySDKRequestLogCleanup = originalCleanup
	}()

	source, errSource := NewLegacySDKFileBodySource("legacy-orphan")
	if errSource != nil {
		t.Fatalf("NewLegacySDKFileBodySource: %v", errSource)
	}
	defer func() { _ = source.Cleanup() }()
	if errAppend := source.AppendBytes([]byte("legacy payload")); errAppend != nil {
		t.Fatalf("AppendBytes: %v", errAppend)
	}
	sourceDir := source.dir
	if errRelease := source.ownerLease.release(false); errRelease != nil {
		t.Fatalf("release source owner lease: %v", errRelease)
	}
	legacySDKRequestLogCleanup.cleanupMu.Lock()
	legacySDKRequestLogCleanup.nextCleanupAttempt = time.Time{}
	legacySDKRequestLogCleanup.cleanupMu.Unlock()

	if errCleanup := RetryLegacySDKRequestLogCleanup(); errCleanup != nil {
		t.Fatalf("RetryLegacySDKRequestLogCleanup: %v", errCleanup)
	}
	if _, errStat := os.Stat(sourceDir); !os.IsNotExist(errStat) {
		t.Fatalf("legacy orphan source directory still exists, stat error = %v", errStat)
	}
}

func TestFileRequestLoggerSettersRetryPendingTempFileCleanup(t *testing.T) {
	tests := []struct {
		name string
		call func(*FileRequestLogger)
	}{
		{name: "SetEnabled", call: func(logger *FileRequestLogger) { logger.SetEnabled(false) }},
		{name: "SetHomeEnabled", call: func(logger *FileRequestLogger) { logger.SetHomeEnabled(true) }},
	}
	for _, test := range tests {
		t.Run(
			test.name, func(t *testing.T) {
				logsDir := t.TempDir()
				logger := NewFileRequestLogger(true, logsDir, "", 0)
				pendingPath := filepath.Join(logsDir, "response-body-pending.tmp")
				if errWrite := os.WriteFile(pendingPath, []byte("pending response"), 0o600); errWrite != nil {
					t.Fatalf("write pending response temp file: %v", errWrite)
				}
				logger.retainTempFileForCleanup(pendingPath)

				test.call(logger)

				if logger.pendingTempFileCount() != 0 {
					t.Fatalf("pending temp files = %d, want 0", logger.pendingTempFileCount())
				}
				if _, errStat := os.Stat(pendingPath); !os.IsNotExist(errStat) {
					t.Fatalf("pending temp path %q still exists, stat error = %v", pendingPath, errStat)
				}
			},
		)
	}
}

func TestFileRequestLoggerNewPendingCleanupBypassesRetryInterval(t *testing.T) {
	logsDir := t.TempDir()
	logger := NewFileRequestLogger(false, logsDir, "", 0)
	if errRetry := logger.RetryPendingCleanup(); errRetry != nil {
		t.Fatalf("initial RetryPendingCleanup: %v", errRetry)
	}
	pendingPath := filepath.Join(logsDir, "response-body-disabled-pending.tmp")
	if errWrite := os.WriteFile(pendingPath, []byte("pending response"), 0o600); errWrite != nil {
		t.Fatalf("write pending response temp file: %v", errWrite)
	}
	if !logger.retainTempFileForCleanup(pendingPath) {
		t.Fatal("retainTempFileForCleanup returned false")
	}

	if errRetry := logger.RetryPendingCleanup(); errRetry != nil {
		t.Fatalf("RetryPendingCleanup after new pending file: %v", errRetry)
	}
	if _, errStat := os.Stat(pendingPath); !os.IsNotExist(errStat) {
		t.Fatalf("pending temp path still exists, stat error = %v", errStat)
	}
}

func TestFileRequestLoggerStopsCreatingSpoolsAtCleanupBacklogLimit(t *testing.T) {
	logsDir := t.TempDir()
	logger := NewFileRequestLogger(true, logsDir, "", 0)
	nestedPaths := make([]string, 0, maxPendingRequestLogTempFiles)
	pendingPaths := make([]string, 0, maxPendingRequestLogTempFiles)
	for i := range maxPendingRequestLogTempFiles {
		path := filepath.Join(logsDir, fmt.Sprintf("response-body-pending-%03d.tmp", i))
		nestedPaths = append(nestedPaths, createNonEmptyDirectory(t, path))
		pendingPaths = append(pendingPaths, path)
		logger.retainTempFileForCleanup(path)
	}

	writer, errWriter := logger.LogStreamingRequest(
		"/v1/responses",
		"POST",
		nil,
		[]byte(`{"stream":true}`),
		"request-id",
	)
	if writer != nil {
		t.Fatalf("writer = %T, want nil", writer)
	}
	if errWriter == nil || !strings.Contains(errWriter.Error(), "cleanup capacity reached") {
		t.Fatalf("LogStreamingRequest error = %v, want cleanup capacity error", errWriter)
	}

	for i := range pendingPaths {
		if errRemove := os.Remove(nestedPaths[i]); errRemove != nil {
			t.Fatalf("remove nested path %d: %v", i, errRemove)
		}
		if errRemove := os.Remove(pendingPaths[i]); errRemove != nil {
			t.Fatalf("remove pending path %d: %v", i, errRemove)
		}
	}
	if errCleanup := logger.retryPendingTempFileCleanup(); errCleanup != nil {
		t.Fatalf("retry pending cleanup: %v", errCleanup)
	}
}

func TestFileRequestLoggerCountsActiveSpoolsTowardCapacity(t *testing.T) {
	logsDir := t.TempDir()
	logger := NewFileRequestLogger(true, logsDir, "", 0)
	writers := make([]StreamingLogWriter, 0, maxPendingRequestLogTempFiles)
	for i := range maxPendingRequestLogTempFiles {
		writer, errWriter := logger.LogStreamingRequest(
			"/v1/responses",
			"POST",
			nil,
			[]byte(`{"stream":true}`),
			fmt.Sprintf("active-%03d", i),
		)
		if errWriter != nil {
			t.Fatalf("LogStreamingRequest %d: %v", i, errWriter)
		}
		writers = append(writers, writer)
	}
	writer, errWriter := logger.LogStreamingRequest(
		"/v1/responses",
		"POST",
		nil,
		[]byte(`{"stream":true}`),
		"over-capacity",
	)
	if writer != nil {
		t.Fatalf("writer over capacity = %T, want nil", writer)
	}
	if errWriter == nil || !strings.Contains(errWriter.Error(), "cleanup capacity reached") {
		t.Fatalf("LogStreamingRequest over capacity error = %v", errWriter)
	}
	for i, activeWriter := range writers {
		if errClose := activeWriter.Close(); errClose != nil {
			t.Fatalf("Close writer %d: %v", i, errClose)
		}
	}
}

func TestFileBodySourceCountsActiveSourcesTowardCapacity(t *testing.T) {
	baseDir := t.TempDir()
	sources := make([]*FileBodySource, 0, maxPendingFileBodySourceCount)
	for i := range maxPendingFileBodySourceCount {
		source, errSource := NewFileBodySourceInDir(baseDir, fmt.Sprintf("active-%03d", i))
		if errSource != nil {
			t.Fatalf("NewFileBodySourceInDir %d: %v", i, errSource)
		}
		sources = append(sources, source)
	}
	source, errSource := NewFileBodySourceInDir(baseDir, "over-capacity")
	if source != nil {
		t.Fatalf("source over capacity = %v, want nil", source)
	}
	if errSource == nil || !strings.Contains(errSource.Error(), "cleanup capacity reached") {
		t.Fatalf("NewFileBodySourceInDir over capacity error = %v", errSource)
	}
	for i, activeSource := range sources {
		if errCleanup := activeSource.Cleanup(); errCleanup != nil {
			t.Fatalf("Cleanup source %d: %v", i, errCleanup)
		}
	}
}

func TestFileStreamingLogWriterDoesNotCreateRequestBodyTempFile(t *testing.T) {
	logsDir := t.TempDir()
	logger := NewFileRequestLogger(true, logsDir, "", 0)
	writer, errWriter := logger.LogStreamingRequest(
		"/v1/responses",
		"POST",
		nil,
		[]byte(`{"model":"gpt-5","stream":true}`),
		"request-id",
	)
	if errWriter != nil {
		t.Fatalf("LogStreamingRequest: %v", errWriter)
	}
	fileWriter, ok := writer.(*fileStreamingLogWriter)
	if !ok {
		t.Fatalf("streaming writer has unexpected type %T", writer)
	}
	if len(fileWriter.requestBody) == 0 {
		t.Fatal("request body was not retained by streaming writer")
	}
	entries, errRead := os.ReadDir(logsDir)
	if errRead != nil {
		t.Fatalf("read logs directory: %v", errRead)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), "request-body-") {
			t.Fatalf("request body temp file was created: %s", entry.Name())
		}
	}
	if errClose := writer.Close(); errClose != nil {
		t.Fatalf("Close: %v", errClose)
	}
}

func TestFileStreamingLogWriterCloseRetriesResponseTempFileCleanup(t *testing.T) {
	baseDir := t.TempDir()
	responsePath := filepath.Join(baseDir, "response-body.tmp")
	responseNested := createNonEmptyDirectory(t, responsePath)
	logger := NewFileRequestLogger(true, baseDir, "", 0)
	writer := &fileStreamingLogWriter{
		logger:           logger,
		responseBodyPath: responsePath,
	}

	errClose := writer.Close()
	if errClose == nil {
		t.Fatal("Close returned nil temp cleanup error")
	}
	if writer.responseBodyPath != responsePath {
		t.Fatalf("failed cleanup path was discarded: %q", writer.responseBodyPath)
	}
	logger.cleanupMu.Lock()
	_, retained := logger.pendingTempFiles[responsePath]
	logger.cleanupMu.Unlock()
	if !retained {
		t.Fatal("logger did not retain failed cleanup path")
	}

	if errRemove := os.Remove(responseNested); errRemove != nil {
		t.Fatalf("remove response nested file: %v", errRemove)
	}
	if errRetry := logger.retryPendingTempFileCleanup(); errRetry != nil {
		t.Fatalf("retry pending cleanup: %v", errRetry)
	}
	logger.cleanupMu.Lock()
	_, retained = logger.pendingTempFiles[responsePath]
	logger.cleanupMu.Unlock()
	if retained {
		t.Fatal("logger retained successfully cleaned path")
	}
	if errClose = writer.Close(); errClose != nil {
		t.Fatalf("retry Close: %v", errClose)
	}
	if writer.responseBodyPath != "" {
		t.Fatalf("successful cleanup path remains: %q", writer.responseBodyPath)
	}
	if _, errStat := os.Stat(responsePath); !os.IsNotExist(errStat) {
		t.Fatalf("temp path %q still exists, stat error = %v", responsePath, errStat)
	}
}

func TestStreamingLogWritersRejectMetadataAfterClose(t *testing.T) {
	type sourceStreamingWriter interface {
		StreamingLogWriter
		WriteAPIRequestSource(*FileBodySource) error
		WriteAPIResponseSource(*FileBodySource) error
		WriteAPIWebsocketTimelineSource(*FileBodySource) error
	}

	writers := []struct {
		name   string
		writer sourceStreamingWriter
	}{
		{name: "file", writer: &fileStreamingLogWriter{closed: true}},
		{name: "home", writer: &homeStreamingLogWriter{closed: true}},
	}
	for _, test := range writers {
		t.Run(
			test.name, func(t *testing.T) {
				source, errSource := NewFileBodySourceInDir(t.TempDir(), "closed-writer")
				if errSource != nil {
					t.Fatalf("NewFileBodySourceInDir: %v", errSource)
				}
				defer func() { _ = source.Cleanup() }()
				if errAppend := source.AppendBytes([]byte("source-payload")); errAppend != nil {
					t.Fatalf("AppendBytes: %v", errAppend)
				}

				operations := []struct {
					name string
					call func() error
				}{
					{name: "status", call: func() error { return test.writer.WriteStatus(200, nil) }},
					{name: "API request", call: func() error { return test.writer.WriteAPIRequest([]byte("request")) }},
					{
						name: "API response",
						call: func() error { return test.writer.WriteAPIResponse([]byte("response")) },
					},
					{
						name: "API websocket timeline",
						call: func() error { return test.writer.WriteAPIWebsocketTimeline([]byte("timeline")) },
					},
					{
						name: "request timeline",
						call: func() error { return test.writer.WriteRequestTimeline([]byte("timeline")) },
					},
					{
						name: "API request source",
						call: func() error { return test.writer.WriteAPIRequestSource(source) },
					},
					{
						name: "API response source",
						call: func() error { return test.writer.WriteAPIResponseSource(source) },
					},
					{
						name: "API websocket timeline source",
						call: func() error { return test.writer.WriteAPIWebsocketTimelineSource(source) },
					},
				}
				for _, operation := range operations {
					if errWrite := operation.call(); !errors.Is(errWrite, errStreamingLogWriterClosed) {
						t.Fatalf("%s error = %v, want closed writer error", operation.name, errWrite)
					}
				}
				if !source.HasPayload() {
					t.Fatal("rejected source ownership was not retained by caller")
				}
			},
		)
	}
}

func TestWriteMaskedPayloadFromReaderPreservesJSONContextAcrossLines(t *testing.T) {
	payload := `{
  "api_key": {
    "nested": "sk-reader-secret"
  },
  "model": "gpt-5"
}`
	var out bytes.Buffer
	if err := writeMaskedPayloadFromReader(&out, strings.NewReader(payload)); err != nil {
		t.Fatalf("writeMaskedPayloadFromReader: %v", err)
	}
	got := out.String()
	if strings.Contains(got, "sk-reader-secret") || strings.Contains(got, "nested") {
		t.Fatalf("writeMaskedPayloadFromReader kept structured secret: %q", got)
	}
	if !strings.Contains(got, "gpt-5") {
		t.Fatalf("writeMaskedPayloadFromReader discarded non-sensitive content: %q", got)
	}
}

func TestWriteMaskedPayloadFromReaderRedactsPreformattedAPIBodies(t *testing.T) {
	payload := "=== API REQUEST 1 ===\n" +
		"Body:\n{\"api_key\":{\"nested\":\"sk-api-request-secret\"},\"model\":\"gpt-5\"}\n\n" +
		"=== API RESPONSE 1 ===\n" +
		"Body:\n{\"access_token\":{\"nested\":\"sk-api-response-secret\"},\"status\":\"ok\"}\n"
	var out bytes.Buffer
	if err := writeMaskedPayloadFromReader(&out, strings.NewReader(payload)); err != nil {
		t.Fatalf("writeMaskedPayloadFromReader: %v", err)
	}
	got := out.String()
	for _, secret := range []string{"sk-api-request-secret", "sk-api-response-secret", "nested"} {
		if strings.Contains(got, secret) {
			t.Fatalf("writeMaskedPayloadFromReader kept preformatted secret %q: %s", secret, got)
		}
	}
	for _, visible := range []string{"=== API REQUEST 1 ===", "=== API RESPONSE 1 ===", "gpt-5", `"status": "ok"`} {
		if !strings.Contains(got, visible) {
			t.Fatalf("writeMaskedPayloadFromReader removed %q: %s", visible, got)
		}
	}
}

func TestWriteMaskedPayloadFromReaderOmitsOversizedPayload(t *testing.T) {
	var out bytes.Buffer
	payload := strings.NewReader(strings.Repeat("x", maxMaskedPayloadBytes+1))
	if err := writeMaskedPayloadFromReader(&out, payload); err != nil {
		t.Fatalf("writeMaskedPayloadFromReader: %v", err)
	}
	if got := out.String(); got != "[payload omitted: exceeds safe redaction limit]" {
		t.Fatalf("writeMaskedPayloadFromReader oversized payload = %q", got)
	}
}

func TestFileBodySourcePreservesJSONContextAcrossParts(t *testing.T) {
	source, err := NewFileBodySourceInDir(t.TempDir(), "redaction")
	if err != nil {
		t.Fatalf("NewFileBodySourceInDir: %v", err)
	}
	defer func() { _ = source.Cleanup() }()

	if errAppend := source.AppendPart([]byte(`{"api_key": {`)); errAppend != nil {
		t.Fatalf("AppendPart first: %v", errAppend)
	}
	if errAppend := source.AppendPart([]byte(`"nested": "sk-part-secret"}, "model": "gpt-5"}`)); errAppend != nil {
		t.Fatalf("AppendPart second: %v", errAppend)
	}

	var out bytes.Buffer
	if errWrite := source.WritePayloadTo(&out); errWrite != nil {
		t.Fatalf("WritePayloadTo: %v", errWrite)
	}
	got := out.String()
	if strings.Contains(got, "sk-part-secret") || strings.Contains(got, "nested") {
		t.Fatalf("WritePayloadTo kept cross-part secret: %q", got)
	}
	if !strings.Contains(got, "gpt-5") {
		t.Fatalf("WritePayloadTo discarded non-sensitive cross-part content: %q", got)
	}
}

func TestFileBodySourceRedactsPreformattedBodiesAcrossParts(t *testing.T) {
	source, err := NewFileBodySourceInDir(t.TempDir(), "api-sections")
	if err != nil {
		t.Fatalf("NewFileBodySourceInDir: %v", err)
	}
	defer func() { _ = source.Cleanup() }()

	if errAppend := source.AppendPart([]byte("=== API REQUEST 1 ===\nBody:\n{\"api_key\":{\"nested\":\"sk-cross-part-request-secret\"},\"model\":\"gpt-5\"}")); errAppend != nil {
		t.Fatalf("AppendPart request: %v", errAppend)
	}
	if errAppend := source.AppendPart([]byte("=== API RESPONSE 1 ===\nBody:\n{\"refresh_token\":{\"nested\":\"sk-cross-part-response-secret\"},\"status\":\"ok\"}")); errAppend != nil {
		t.Fatalf("AppendPart response: %v", errAppend)
	}

	var out bytes.Buffer
	if errWrite := source.WritePayloadTo(&out); errWrite != nil {
		t.Fatalf("WritePayloadTo: %v", errWrite)
	}
	got := out.String()
	for _, secret := range []string{"sk-cross-part-request-secret", "sk-cross-part-response-secret", "nested"} {
		if strings.Contains(got, secret) {
			t.Fatalf("WritePayloadTo kept preformatted secret %q: %s", secret, got)
		}
	}
	for _, visible := range []string{"=== API REQUEST 1 ===", "=== API RESPONSE 1 ===", "gpt-5", `"status": "ok"`} {
		if !strings.Contains(got, visible) {
			t.Fatalf("WritePayloadTo removed %q: %s", visible, got)
		}
	}
}

func TestFileBodySourceAppliesPayloadLimitAcrossParts(t *testing.T) {
	source, err := NewFileBodySourceInDir(t.TempDir(), "redaction-limit")
	if err != nil {
		t.Fatalf("NewFileBodySourceInDir: %v", err)
	}
	defer func() { _ = source.Cleanup() }()

	partSize := maxMaskedPayloadBytes/2 + 1
	for i := range 2 {
		if errAppend := source.AppendPart([]byte(strings.Repeat("x", partSize))); errAppend != nil {
			t.Fatalf("AppendPart %d: %v", i, errAppend)
		}
	}

	var capturedSize int64
	for _, path := range source.Paths() {
		info, errStat := os.Stat(path)
		if errStat != nil {
			t.Fatalf("Stat source part: %v", errStat)
		}
		capturedSize += info.Size()
	}
	if capturedSize > maxMaskedPayloadBytes {
		t.Fatalf("captured source bytes = %d, limit = %d", capturedSize, maxMaskedPayloadBytes)
	}
	if !source.truncated {
		t.Fatal("source did not record capture truncation")
	}

	var out bytes.Buffer
	if errWrite := source.WritePayloadTo(&out); errWrite != nil {
		t.Fatalf("WritePayloadTo: %v", errWrite)
	}
	if got := out.String(); got != "[payload omitted: exceeds safe redaction limit]" {
		t.Fatalf("WritePayloadTo oversized parts = %q", got)
	}
}

func newCleanupFailureSource(t *testing.T) (*FileBodySource, string, string, string) {
	t.Helper()
	source, errSource := NewFileBodySourceInDir(t.TempDir(), "cleanup-retry")
	if errSource != nil {
		t.Fatalf("NewFileBodySourceInDir: %v", errSource)
	}
	if errAppend := source.AppendBytes([]byte("source-payload")); errAppend != nil {
		t.Fatalf("AppendBytes: %v", errAppend)
	}
	paths := source.Paths()
	if len(paths) != 1 {
		t.Fatalf("source paths = %v, want one path", paths)
	}
	partPath := paths[0]
	sourceDir := source.dir
	if errRemove := os.Remove(partPath); errRemove != nil {
		t.Fatalf("remove source part: %v", errRemove)
	}
	if errMkdir := os.Mkdir(partPath, 0o700); errMkdir != nil {
		t.Fatalf("create source part directory: %v", errMkdir)
	}
	nestedPath := filepath.Join(partPath, "nested")
	if errWrite := os.WriteFile(nestedPath, []byte("keep directory non-empty"), 0o600); errWrite != nil {
		t.Fatalf("write nested source file: %v", errWrite)
	}
	return source, sourceDir, partPath, nestedPath
}

func createNonEmptyDirectory(t *testing.T, path string) string {
	t.Helper()
	if errMkdir := os.Mkdir(path, 0o700); errMkdir != nil {
		t.Fatalf("create directory %q: %v", path, errMkdir)
	}
	nestedPath := filepath.Join(path, "nested")
	if errWrite := os.WriteFile(nestedPath, []byte("keep directory non-empty"), 0o600); errWrite != nil {
		t.Fatalf("write nested file %q: %v", nestedPath, errWrite)
	}
	return nestedPath
}

func removeCleanupFailure(t *testing.T, partPath string, nestedPath string) {
	t.Helper()
	if errRemove := os.Remove(nestedPath); errRemove != nil {
		t.Fatalf("remove nested source file: %v", errRemove)
	}
	if errRemove := os.Remove(partPath); errRemove != nil {
		t.Fatalf("remove source part directory: %v", errRemove)
	}
}
