package api

import (
	"bytes"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	internalmiddleware "github.com/Pyrokine/CLIProxyAPI/v6/internal/api/middleware"
	internalinterfaces "github.com/Pyrokine/CLIProxyAPI/v6/internal/interfaces"
	internallogging "github.com/Pyrokine/CLIProxyAPI/v6/internal/logging"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/api/handlers"
	sdklogging "github.com/Pyrokine/CLIProxyAPI/v6/sdk/logging"
	"github.com/gin-gonic/gin"
)

type sdkRequestLoggerSpy struct {
	mu sync.Mutex

	enabled           bool
	logCalls          int
	snapshot          sdklogging.RequestLogger
	snapshotCalls     int
	enabledValues     []bool
	homeEnabledValues []bool
	maxFilesValues    []int
	cleanupCalls      int
	cleanupErr        error
	cleanupPanic      any
	sourceDir         string
	sourcePrefixes    []string
	streamWriter      sdklogging.StreamingLogWriter

	allSourcesCalls            int
	websocketTimelineSource    *internallogging.FileBodySource
	apiRequestSource           *internallogging.FileBodySource
	apiResponseSource          *internallogging.FileBodySource
	apiWebsocketTimelineSource *internallogging.FileBodySource
	requestTimeline            []byte
	apiResponseErrors          []*handlers.ErrorMessage
	force                      bool
}

func (l *sdkRequestLoggerSpy) LogRequest(
	_, _ string,
	_ map[string][]string,
	_ []byte,
	_ int,
	_ map[string][]string,
	_, _, _, _, _ []byte,
	_ []*handlers.ErrorMessage,
	_ string,
	_, _ time.Time,
) error {
	l.mu.Lock()
	l.logCalls++
	l.mu.Unlock()
	return nil
}

func (l *sdkRequestLoggerSpy) LogRequestWithOptionsAndAllSources(
	_, _ string,
	_ map[string][]string,
	_ []byte,
	_ int,
	_ map[string][]string,
	_, _ []byte,
	websocketTimelineSource *internallogging.FileBodySource,
	_ []byte,
	apiRequestSource *internallogging.FileBodySource,
	_ []byte,
	apiResponseSource *internallogging.FileBodySource,
	_ []byte,
	apiWebsocketTimelineSource *internallogging.FileBodySource,
	requestTimeline []byte,
	apiResponseErrors []*handlers.ErrorMessage,
	force bool,
	_ string,
	_, _ time.Time,
) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.allSourcesCalls++
	l.websocketTimelineSource = websocketTimelineSource
	l.apiRequestSource = apiRequestSource
	l.apiResponseSource = apiResponseSource
	l.apiWebsocketTimelineSource = apiWebsocketTimelineSource
	l.requestTimeline = bytes.Clone(requestTimeline)
	l.apiResponseErrors = append([]*handlers.ErrorMessage(nil), apiResponseErrors...)
	l.force = force
	return nil
}

func (l *sdkRequestLoggerSpy) LogStreamingRequest(
	_, _ string,
	_ map[string][]string,
	_ []byte,
	_ string,
) (sdklogging.StreamingLogWriter, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.streamWriter, nil
}

func (l *sdkRequestLoggerSpy) IsEnabled() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.enabled
}

func (l *sdkRequestLoggerSpy) Snapshot() sdklogging.RequestLogger {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.snapshotCalls++
	return l.snapshot
}

func (l *sdkRequestLoggerSpy) NewFileBodySource(prefix string) (*internallogging.FileBodySource, error) {
	l.mu.Lock()
	l.sourcePrefixes = append(l.sourcePrefixes, prefix)
	dir := l.sourceDir
	l.mu.Unlock()
	return internallogging.NewFileBodySourceInDir(dir, prefix)
}

func (l *sdkRequestLoggerSpy) RetryPendingCleanup() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.cleanupCalls++
	if l.cleanupPanic != nil {
		panic(l.cleanupPanic)
	}
	return l.cleanupErr
}

func (l *sdkRequestLoggerSpy) SetEnabled(enabled bool) {
	l.mu.Lock()
	l.enabledValues = append(l.enabledValues, enabled)
	l.mu.Unlock()
}

func (l *sdkRequestLoggerSpy) SetHomeEnabled(enabled bool) {
	l.mu.Lock()
	l.homeEnabledValues = append(l.homeEnabledValues, enabled)
	l.mu.Unlock()
}

func (l *sdkRequestLoggerSpy) SetErrorLogsMaxFiles(maxFiles int) {
	l.mu.Lock()
	l.maxFilesValues = append(l.maxFilesValues, maxFiles)
	l.mu.Unlock()
}

type sdkBasicRequestLoggerSpy struct {
	mu                   sync.Mutex
	logCalls             int
	websocketTimeline    []byte
	apiRequest           []byte
	apiResponse          []byte
	apiWebsocketTimeline []byte
}

func (l *sdkBasicRequestLoggerSpy) LogRequest(
	_, _ string,
	_ map[string][]string,
	_ []byte,
	_ int,
	_ map[string][]string,
	_ []byte,
	websocketTimeline []byte,
	apiRequest []byte,
	apiResponse []byte,
	apiWebsocketTimeline []byte,
	_ []*handlers.ErrorMessage,
	_ string,
	_, _ time.Time,
) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.logCalls++
	l.websocketTimeline = bytes.Clone(websocketTimeline)
	l.apiRequest = bytes.Clone(apiRequest)
	l.apiResponse = bytes.Clone(apiResponse)
	l.apiWebsocketTimeline = bytes.Clone(apiWebsocketTimeline)
	return nil
}

func (*sdkBasicRequestLoggerSpy) LogStreamingRequest(
	_, _ string,
	_ map[string][]string,
	_ []byte,
	_ string,
) (sdklogging.StreamingLogWriter, error) {
	return nil, nil
}

func (*sdkBasicRequestLoggerSpy) IsEnabled() bool {
	return true
}

type sdkLegacyToggleLogger struct {
	*sdkBasicRequestLoggerSpy
	mu      sync.Mutex
	enabled bool
}

func (l *sdkLegacyToggleLogger) LogRequest(
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
	if !l.IsEnabled() {
		return nil
	}
	return l.sdkBasicRequestLoggerSpy.LogRequest(
		url, method, requestHeaders, body, statusCode, responseHeaders, response, websocketTimeline,
		apiRequest, apiResponse, apiWebsocketTimeline, apiResponseErrors, requestID,
		requestTimestamp, apiResponseTimestamp,
	)
}

func (l *sdkLegacyToggleLogger) IsEnabled() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.enabled
}

func (l *sdkLegacyToggleLogger) SetEnabled(enabled bool) {
	l.mu.Lock()
	l.enabled = enabled
	l.mu.Unlock()
}

type sdkOptionsRequestLoggerSpy struct {
	*sdkBasicRequestLoggerSpy
	mu           sync.Mutex
	optionsCalls int
	force        bool
}

func (l *sdkOptionsRequestLoggerSpy) LogRequestWithOptions(
	_, _ string,
	_ map[string][]string,
	_ []byte,
	_ int,
	_ map[string][]string,
	_, _, _, _, _ []byte,
	_ []*handlers.ErrorMessage,
	force bool,
	_ string,
	_, _ time.Time,
) error {
	l.mu.Lock()
	l.optionsCalls++
	l.force = force
	l.mu.Unlock()
	return nil
}

type sdkStreamingWriterSpy struct{}

func (*sdkStreamingWriterSpy) WriteChunkAsync([]byte)                     {}
func (*sdkStreamingWriterSpy) WriteStatus(int, map[string][]string) error { return nil }
func (*sdkStreamingWriterSpy) WriteAPIRequest([]byte) error               { return nil }
func (*sdkStreamingWriterSpy) WriteAPIResponse([]byte) error              { return nil }
func (*sdkStreamingWriterSpy) WriteAPIWebsocketTimeline([]byte) error     { return nil }
func (*sdkStreamingWriterSpy) SetFirstChunkTimestamp(time.Time)           {}
func (*sdkStreamingWriterSpy) WriteRequestTimeline([]byte) error          { return nil }
func (*sdkStreamingWriterSpy) Close() error                               { return nil }
func (*sdkStreamingWriterSpy) WriteAPIRequestSource(*internallogging.FileBodySource) error {
	return nil
}
func (*sdkStreamingWriterSpy) WriteAPIResponseSource(*internallogging.FileBodySource) error {
	return nil
}
func (*sdkStreamingWriterSpy) WriteAPIWebsocketTimelineSource(*internallogging.FileBodySource) error {
	return nil
}

func TestRequestLoggerAdapterForwardsDynamicSetters(t *testing.T) {
	logger := &sdkRequestLoggerSpy{}
	adapter := requestLoggerAdapter{logger: logger}

	adapter.SetEnabled(false)
	adapter.SetHomeEnabled(true)
	adapter.SetErrorLogsMaxFiles(17)

	logger.mu.Lock()
	defer logger.mu.Unlock()
	if len(logger.enabledValues) != 1 || logger.enabledValues[0] {
		t.Fatalf("enabled values = %v", logger.enabledValues)
	}
	if len(logger.homeEnabledValues) != 1 || !logger.homeEnabledValues[0] {
		t.Fatalf("home enabled values = %v", logger.homeEnabledValues)
	}
	if len(logger.maxFilesValues) != 1 || logger.maxFilesValues[0] != 17 {
		t.Fatalf("max files values = %v", logger.maxFilesValues)
	}
}

func TestRequestLoggerAdapterForwardsPendingCleanupRetry(t *testing.T) {
	cleanupErr := errors.New("cleanup failed")
	logger := &sdkRequestLoggerSpy{cleanupErr: cleanupErr}
	adapter := requestLoggerAdapter{logger: logger}

	errRetry := adapter.RetryPendingCleanup()
	if !errors.Is(errRetry, cleanupErr) {
		t.Fatalf("RetryPendingCleanup error = %v, want cleanup error", errRetry)
	}
	logger.mu.Lock()
	cleanupCalls := logger.cleanupCalls
	logger.mu.Unlock()
	if cleanupCalls != 1 {
		t.Fatalf("cleanup retry calls = %d, want 1", cleanupCalls)
	}
}

func TestRequestLoggerAdapterCleanupPanicReturnsError(t *testing.T) {
	logger := &sdkRequestLoggerSpy{cleanupPanic: "SDK cleanup panic"}
	adapter := requestLoggerAdapter{logger: logger}

	errRetry := adapter.RetryPendingCleanup()
	if errRetry == nil || !strings.Contains(errRetry.Error(), "SDK cleanup panic") {
		t.Fatalf("RetryPendingCleanup error = %v", errRetry)
	}
	logger.mu.Lock()
	cleanupCalls := logger.cleanupCalls
	logger.mu.Unlock()
	if cleanupCalls != 1 {
		t.Fatalf("cleanup retry calls = %d, want 1", cleanupCalls)
	}
}

func TestRequestLoggerAdapterForwardsForceToOptionsOnlyLogger(t *testing.T) {
	logger := &sdkOptionsRequestLoggerSpy{sdkBasicRequestLoggerSpy: &sdkBasicRequestLoggerSpy{}}
	adapter := requestLoggerAdapter{logger: logger}

	if errLog := adapter.LogRequestWithOptionsAndTimeline(
		"/v1/messages", "POST", nil, nil, 500, nil,
		nil, nil, nil, nil, nil, []byte("timeline"), nil, true,
		"request-id", time.Time{}, time.Time{},
	); errLog != nil {
		t.Fatalf("LogRequestWithOptionsAndTimeline: %v", errLog)
	}
	logger.mu.Lock()
	optionsCalls := logger.optionsCalls
	force := logger.force
	logger.mu.Unlock()
	if optionsCalls != 1 || !force {
		t.Fatalf("options calls = %d, force = %t", optionsCalls, force)
	}
}

func TestRequestLoggerAdapterUsesManagedDirectoryForLegacySources(t *testing.T) {
	adapter := requestLoggerAdapter{logger: &sdkBasicRequestLoggerSpy{}}
	source, errSource := adapter.NewFileBodySource("legacy-source")
	if errSource != nil {
		t.Fatalf("NewFileBodySource: %v", errSource)
	}
	defer func() { _ = source.Cleanup() }()
	if errAppend := source.AppendBytes([]byte("payload")); errAppend != nil {
		t.Fatalf("AppendBytes: %v", errAppend)
	}
	paths := source.Paths()
	if len(paths) != 1 {
		t.Fatalf("source paths = %v, want one path", paths)
	}
	baseDir := filepath.Dir(filepath.Dir(paths[0]))
	if filepath.Base(baseDir) != "cliproxyapi-request-log" {
		t.Fatalf("legacy source base directory = %q", baseDir)
	}
}

func TestRequestLoggerAdapterSnapshotUsesFrozenLogger(t *testing.T) {
	frozen := &sdkRequestLoggerSpy{enabled: true}
	replacement := &sdkRequestLoggerSpy{enabled: false}
	live := &sdkRequestLoggerSpy{enabled: true, snapshot: frozen}
	adapter := requestLoggerAdapter{logger: live}

	snapshot := adapter.Snapshot()
	if snapshot == nil || !snapshot.IsEnabled() {
		t.Fatal("snapshot did not preserve the enabled frozen logger")
	}
	live.mu.Lock()
	live.snapshot = replacement
	live.mu.Unlock()
	if errLog := snapshot.LogRequest(
		"/v1/messages", "POST", nil, nil, 200, nil,
		nil, nil, nil, nil, nil, nil, "request-id", time.Time{}, time.Time{},
	); errLog != nil {
		t.Fatalf("LogRequest: %v", errLog)
	}

	live.mu.Lock()
	liveCalls := live.logCalls
	snapshotCalls := live.snapshotCalls
	live.mu.Unlock()
	frozen.mu.Lock()
	frozenCalls := frozen.logCalls
	frozen.mu.Unlock()
	replacement.mu.Lock()
	replacementCalls := replacement.logCalls
	replacement.mu.Unlock()
	if snapshotCalls != 1 || liveCalls != 0 || frozenCalls != 1 || replacementCalls != 0 {
		t.Fatalf(
			"snapshot calls = %d, live calls = %d, frozen calls = %d, replacement calls = %d",
			snapshotCalls, liveCalls, frozenCalls, replacementCalls,
		)
	}
}

func TestRequestLoggerAdapterLegacySnapshotFreezesEnabledState(t *testing.T) {
	tests := []struct {
		name        string
		entry       bool
		replacement bool
		wantCalls   int
	}{
		{name: "enabled remains enabled", entry: true, replacement: false, wantCalls: 1},
		{name: "disabled remains disabled", entry: false, replacement: true, wantCalls: 0},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			logger := &sdkLegacyToggleLogger{
				sdkBasicRequestLoggerSpy: &sdkBasicRequestLoggerSpy{},
				enabled:                  test.entry,
			}
			adapter := newRequestLoggerAdapter(logger)
			snapshot := adapter.Snapshot()
			if snapshot == nil {
				t.Fatal("Snapshot returned nil")
			}

			adapter.SetEnabled(test.replacement)
			if got := snapshot.IsEnabled(); got != test.entry {
				t.Fatalf("snapshot enabled = %t, want %t", got, test.entry)
			}
			if got := adapter.IsEnabled(); got != test.replacement {
				t.Fatalf("live adapter enabled = %t, want %t", got, test.replacement)
			}
			if snapshot.IsEnabled() {
				if errLog := snapshot.LogRequest(
					"/v1/messages", "POST", nil, nil, 200, nil,
					nil, nil, nil, nil, nil, nil, "request-id", time.Time{}, time.Time{},
				); errLog != nil {
					t.Fatalf("LogRequest: %v", errLog)
				}
			}
			logger.sdkBasicRequestLoggerSpy.mu.Lock()
			logCalls := logger.sdkBasicRequestLoggerSpy.logCalls
			logger.sdkBasicRequestLoggerSpy.mu.Unlock()
			if logCalls != test.wantCalls {
				t.Fatalf("snapshot log calls = %d, want %d", logCalls, test.wantCalls)
			}
		})
	}
}

func TestRequestLoggerAdapterForwardsSourcesAndErrors(t *testing.T) {
	logger := &sdkRequestLoggerSpy{enabled: true, sourceDir: t.TempDir()}
	adapter := requestLoggerAdapter{logger: logger}

	createdSource, errSource := adapter.NewFileBodySource("adapter-source")
	if errSource != nil {
		t.Fatalf("NewFileBodySource: %v", errSource)
	}
	defer func() { _ = createdSource.Cleanup() }()

	sources := make([]*internallogging.FileBodySource, 4)
	for i := range sources {
		source, errCreate := internallogging.NewFileBodySourceInDir(t.TempDir(), "source")
		if errCreate != nil {
			t.Fatalf("NewFileBodySourceInDir(%d): %v", i, errCreate)
		}
		sources[i] = source
		defer func(source *internallogging.FileBodySource) { _ = source.Cleanup() }(source)
	}
	requestTimeline := []byte("request timeline")
	apiError := &internalinterfaces.ErrorMessage{
		StatusCode: 429,
		Error:      errors.New("rate limited"),
		Addon:      http.Header{"Retry-After": []string{"1"}},
	}
	if errLog := adapter.LogRequestWithOptionsAndAllSources(
		"/v1/messages", "POST", nil, nil, 429, nil,
		nil, nil, sources[0], nil, sources[1], nil, sources[2], nil, sources[3],
		requestTimeline, []*internalinterfaces.ErrorMessage{nil, apiError}, true, "request-id", time.Time{}, time.Time{},
	); errLog != nil {
		t.Fatalf("LogRequestWithOptionsAndAllSources: %v", errLog)
	}

	logger.mu.Lock()
	defer logger.mu.Unlock()
	if len(logger.sourcePrefixes) != 1 || logger.sourcePrefixes[0] != "adapter-source" {
		t.Fatalf("source prefixes = %v", logger.sourcePrefixes)
	}
	if logger.allSourcesCalls != 1 || logger.websocketTimelineSource != sources[0] ||
		logger.apiRequestSource != sources[1] || logger.apiResponseSource != sources[2] ||
		logger.apiWebsocketTimelineSource != sources[3] {
		t.Fatal("all source pointers were not forwarded in order")
	}
	if !bytes.Equal(logger.requestTimeline, requestTimeline) || !logger.force {
		t.Fatalf("timeline = %q, force = %t", string(logger.requestTimeline), logger.force)
	}
	if len(logger.apiResponseErrors) != 1 || logger.apiResponseErrors[0].StatusCode != 429 ||
		logger.apiResponseErrors[0].Error == nil || logger.apiResponseErrors[0].Error.Error() != "rate limited" ||
		logger.apiResponseErrors[0].Addon.Get("Retry-After") != "1" {
		t.Fatalf("API response errors = %#v", logger.apiResponseErrors)
	}
}

func TestRequestLoggerAdapterMergesSourcesForBasicLogger(t *testing.T) {
	logger := &sdkBasicRequestLoggerSpy{}
	adapter := requestLoggerAdapter{logger: logger}

	memoryPayloads := [][]byte{
		[]byte("memory-websocket"),
		[]byte("memory-request"),
		[]byte("memory-response"),
		[]byte("memory-api-websocket"),
	}
	sourcePayloads := [][]byte{
		[]byte("source-websocket"),
		[]byte("source-request"),
		[]byte("source-response"),
		[]byte("source-api-websocket"),
	}
	sources := make([]*internallogging.FileBodySource, len(sourcePayloads))
	partPaths := make([]string, len(sourcePayloads))
	for i, payload := range sourcePayloads {
		source, errSource := internallogging.NewFileBodySourceInDir(t.TempDir(), "adapter-fallback")
		if errSource != nil {
			t.Fatalf("NewFileBodySourceInDir(%d): %v", i, errSource)
		}
		if errAppend := source.AppendBytes(payload); errAppend != nil {
			t.Fatalf("AppendBytes(%d): %v", i, errAppend)
		}
		paths := source.Paths()
		if len(paths) != 1 {
			t.Fatalf("source paths(%d) = %v", i, paths)
		}
		sources[i] = source
		partPaths[i] = paths[0]
	}

	if errLog := adapter.LogRequestWithOptionsAndAllSources(
		"/v1/responses", "POST", nil, nil, 200, nil, nil,
		memoryPayloads[0], sources[0], memoryPayloads[1], sources[1], memoryPayloads[2], sources[2],
		memoryPayloads[3], sources[3], nil, nil, false, "request-id", time.Time{}, time.Time{},
	); errLog != nil {
		t.Fatalf("LogRequestWithOptionsAndAllSources: %v", errLog)
	}

	logger.mu.Lock()
	captured := [][]byte{
		bytes.Clone(logger.websocketTimeline),
		bytes.Clone(logger.apiRequest),
		bytes.Clone(logger.apiResponse),
		bytes.Clone(logger.apiWebsocketTimeline),
	}
	logCalls := logger.logCalls
	logger.mu.Unlock()
	if logCalls != 1 {
		t.Fatalf("log calls = %d, want 1", logCalls)
	}
	for i := range captured {
		memoryIndex := bytes.Index(captured[i], memoryPayloads[i])
		sourceIndex := bytes.Index(captured[i], sourcePayloads[i])
		if sourceIndex < 0 || memoryIndex <= sourceIndex {
			t.Fatalf("merged payload %d has wrong order: %q", i, string(captured[i]))
		}
		if _, errStat := os.Stat(partPaths[i]); !os.IsNotExist(errStat) {
			t.Fatalf("source part %q still exists, stat error = %v", partPaths[i], errStat)
		}
	}
}

func TestRequestLoggerAdapterCallsBasicLoggerWhenCleanupFails(t *testing.T) {
	pendingBefore := internallogging.PendingFileBodySourceCleanupCount()
	logger := &sdkBasicRequestLoggerSpy{}
	adapter := requestLoggerAdapter{logger: logger}
	source, errSource := internallogging.NewFileBodySourceInDir(t.TempDir(), "adapter-cleanup-failure")
	if errSource != nil {
		t.Fatalf("NewFileBodySourceInDir: %v", errSource)
	}
	if errAppend := source.AppendBytes([]byte("source-prefix")); errAppend != nil {
		t.Fatalf("AppendBytes: %v", errAppend)
	}
	paths := source.Paths()
	if len(paths) != 1 {
		t.Fatalf("source paths = %v, want one path", paths)
	}
	sourceDir := filepath.Dir(paths[0])
	blockingDir := filepath.Join(sourceDir, "cleanup-blocker")
	if errMkdir := os.Mkdir(blockingDir, 0o700); errMkdir != nil {
		t.Fatalf("create cleanup blocker: %v", errMkdir)
	}
	blockingPath := filepath.Join(blockingDir, "nested")
	if errWrite := os.WriteFile(blockingPath, []byte("block cleanup"), 0o600); errWrite != nil {
		t.Fatalf("write cleanup blocker: %v", errWrite)
	}

	errLog := adapter.LogRequestWithOptionsAndAllSources(
		"/v1/responses", "POST", nil, nil, 200, nil, nil,
		nil, nil, []byte("memory-suffix"), source, nil, nil, nil, nil,
		nil, nil, false, "request-id", time.Time{}, time.Time{},
	)
	if errLog == nil {
		t.Fatal("LogRequestWithOptionsAndAllSources returned nil cleanup error")
	}
	if pending := internallogging.PendingFileBodySourceCleanupCount(); pending != pendingBefore+1 {
		t.Fatalf("pending source cleanup count = %d, want %d", pending, pendingBefore+1)
	}
	logger.mu.Lock()
	logCalls := logger.logCalls
	captured := bytes.Clone(logger.apiRequest)
	logger.mu.Unlock()
	if logCalls != 1 {
		t.Fatalf("log calls = %d, want 1", logCalls)
	}
	if sourceIndex := bytes.Index(captured, []byte("source-prefix")); sourceIndex < 0 {
		t.Fatalf("captured API request = %q, want source prefix", captured)
	}
	if memoryIndex := bytes.Index(captured, []byte("memory-suffix")); memoryIndex < 0 {
		t.Fatalf("captured API request = %q, want memory suffix", captured)
	}
	if errRemove := os.Remove(blockingPath); errRemove != nil {
		t.Fatalf("remove cleanup blocker file: %v", errRemove)
	}
	if errRemove := os.Remove(blockingDir); errRemove != nil {
		t.Fatalf("remove cleanup blocker directory: %v", errRemove)
	}
	if errCleanup := source.Cleanup(); errCleanup != nil {
		t.Fatalf("retry source cleanup: %v", errCleanup)
	}
}

func TestRequestLoggerAdapterPreservesMemoryLogWhenBasicSourceReadFails(t *testing.T) {
	logger := &sdkBasicRequestLoggerSpy{}
	adapter := requestLoggerAdapter{logger: logger}
	sources := make([]*internallogging.FileBodySource, 4)
	partPaths := make([]string, len(sources))
	for i := range sources {
		source, errSource := internallogging.NewFileBodySourceInDir(t.TempDir(), "adapter-failure")
		if errSource != nil {
			t.Fatalf("NewFileBodySourceInDir(%d): %v", i, errSource)
		}
		if errAppend := source.AppendBytes([]byte("source-payload")); errAppend != nil {
			t.Fatalf("AppendBytes(%d): %v", i, errAppend)
		}
		paths := source.Paths()
		if len(paths) != 1 {
			t.Fatalf("source paths(%d) = %v", i, paths)
		}
		sources[i] = source
		partPaths[i] = paths[0]
	}
	if errRemove := os.Remove(partPaths[1]); errRemove != nil {
		t.Fatalf("remove failing source part: %v", errRemove)
	}

	errLog := adapter.LogRequestWithOptionsAndAllSources(
		"/v1/responses", "POST", nil, nil, 200, nil, nil,
		[]byte("memory-websocket"), sources[0], []byte("memory-request"), sources[1],
		[]byte("memory-response"), sources[2], []byte("memory-api-websocket"), sources[3],
		nil, nil, false, "request-id", time.Time{}, time.Time{},
	)
	if errLog == nil || !strings.Contains(errLog.Error(), "merge SDK request log API request source") {
		t.Fatalf("LogRequestWithOptionsAndAllSources error = %v", errLog)
	}
	logger.mu.Lock()
	logCalls := logger.logCalls
	capturedRequest := bytes.Clone(logger.apiRequest)
	logger.mu.Unlock()
	if logCalls != 1 {
		t.Fatalf("log calls = %d, want 1", logCalls)
	}
	if !bytes.Contains(capturedRequest, []byte("[source prefix unavailable: read failed]")) ||
		!bytes.Contains(capturedRequest, []byte("memory-request")) {
		t.Fatalf("captured API request = %q", capturedRequest)
	}
	for i, source := range sources {
		if source.HasPayload() {
			t.Fatalf("source %d still has payload after failure", i)
		}
		if _, errStat := os.Stat(partPaths[i]); !os.IsNotExist(errStat) {
			t.Fatalf("source part %q still exists, stat error = %v", partPaths[i], errStat)
		}
	}
}

func TestRequestLoggerAdapterLegacyLoggerUsesMiddlewareFileSources(t *testing.T) {
	tests := []struct {
		name    string
		payload []byte
		want    []byte
	}{
		{
			name:    "materializes source payload",
			payload: []byte(`{"model":"gpt-5"}`),
			want:    []byte("gpt-5"),
		},
		{
			name:    "omits oversized source payload",
			payload: bytes.Repeat([]byte("x"), (8<<20)+1),
			want:    []byte("[payload omitted: exceeds safe redaction limit]"),
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			gin.SetMode(gin.TestMode)
			logger := &sdkBasicRequestLoggerSpy{}
			adapter := requestLoggerAdapter{logger: logger}
			engine := gin.New()
			engine.Use(internalmiddleware.RequestLoggingMiddleware(adapter))

			var source *internallogging.FileBodySource
			var partPath string
			engine.POST("/v1/messages", func(c *gin.Context) {
				value, exists := c.Get(internallogging.APIRequestSourceContextKey)
				if !exists {
					t.Fatal("API request source missing")
				}
				var ok bool
				source, ok = value.(*internallogging.FileBodySource)
				if !ok || source == nil {
					t.Fatalf("API request source has unexpected type %T", value)
				}
				if errAppend := source.AppendBytes(test.payload); errAppend != nil {
					t.Fatalf("AppendBytes: %v", errAppend)
				}
				paths := source.Paths()
				if len(paths) != 1 {
					t.Fatalf("source paths = %v, want one path", paths)
				}
				partPath = paths[0]
				c.String(http.StatusOK, "ok")
			})

			recorder := httptest.NewRecorder()
			request := httptest.NewRequest(
				http.MethodPost,
				"/v1/messages",
				bytes.NewBufferString(`{"model":"claude"}`),
			)
			engine.ServeHTTP(recorder, request)

			if recorder.Code != http.StatusOK {
				t.Fatalf("status = %d, want %d", recorder.Code, http.StatusOK)
			}
			logger.mu.Lock()
			logCalls := logger.logCalls
			captured := bytes.Clone(logger.apiRequest)
			logger.mu.Unlock()
			if logCalls != 1 {
				t.Fatalf("log calls = %d, want 1", logCalls)
			}
			if !bytes.Contains(captured, test.want) {
				t.Fatalf("captured API request does not contain %q", string(test.want))
			}
			if source == nil || source.HasPayload() {
				t.Fatal("middleware source was not cleaned")
			}
			if _, errStat := os.Stat(partPath); !os.IsNotExist(errStat) {
				t.Fatalf("source part %q still exists, stat error = %v", partPath, errStat)
			}
		})
	}
}

func TestRequestLoggerAdapterPreservesStreamingSourceCapabilities(t *testing.T) {
	logger := &sdkRequestLoggerSpy{
		enabled:      true,
		streamWriter: &sdkStreamingWriterSpy{},
	}
	adapter := requestLoggerAdapter{logger: logger}

	writer, errWriter := adapter.LogStreamingRequest("/v1/responses", "POST", nil, nil, "request-id")
	if errWriter != nil {
		t.Fatalf("LogStreamingRequest: %v", errWriter)
	}
	if _, ok := writer.(interface {
		WriteAPIRequestSource(*internallogging.FileBodySource) error
		WriteAPIResponseSource(*internallogging.FileBodySource) error
		WriteAPIWebsocketTimelineSource(*internallogging.FileBodySource) error
	}); !ok {
		t.Fatal("streaming source capabilities were lost by the adapter")
	}
}
