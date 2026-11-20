package logging

import (
	"bytes"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestFileBodySource_RecreatesPartDirAfterManualCleanup(t *testing.T) {
	logsDir := t.TempDir()
	source, errSource := NewFileBodySourceInDir(logsDir, "websocket-timeline-test")
	if errSource != nil {
		t.Fatalf("NewFileBodySourceInDir: %v", errSource)
	}
	if errAppend := source.AppendPart([]byte("before manual cleanup")); errAppend != nil {
		t.Fatalf("AppendPart before cleanup: %v", errAppend)
	}
	if errRemove := os.RemoveAll(logsDir); errRemove != nil {
		t.Fatalf("RemoveAll logs dir: %v", errRemove)
	}
	if errAppend := source.AppendPart([]byte("after manual cleanup")); errAppend != nil {
		t.Fatalf("AppendPart after cleanup: %v", errAppend)
	}

	raw, errBytes := source.Bytes()
	if errBytes != nil {
		t.Fatalf("Bytes after cleanup: %v", errBytes)
	}
	if bytes.Contains(raw, []byte("before manual cleanup")) {
		t.Fatalf("expected manually removed part to be skipped, got %q", string(raw))
	}
	if !bytes.Contains(raw, []byte("after manual cleanup")) {
		t.Fatalf("expected recreated part content, got %q", string(raw))
	}

	partPaths := source.Paths()
	if errCleanup := source.Cleanup(); errCleanup != nil {
		t.Fatalf("Cleanup: %v", errCleanup)
	}
	assertFileBodySourceCleaned(t, partPaths)
}

func TestFileRequestLogger_LogRequestWithSourcesWritesLocalLogAndCleansParts(t *testing.T) {
	logsDir := t.TempDir()
	logger := NewFileRequestLogger(true, logsDir, "", 0)

	timelineSource, errSource := logger.NewFileBodySource("websocket-timeline-test")
	if errSource != nil {
		t.Fatalf("logger.NewFileBodySource: %v", errSource)
	}
	if errAppend := timelineSource.AppendPart(
		[]byte(("Timestamp: 2026-05-25T12:00:00Z\nEvent: " +
			"websocket.request\n{}")),
	); errAppend !=
		nil {
		t.Fatalf("AppendPart request: %v", errAppend)
	}
	if errAppend := timelineSource.AppendPart(
		[]byte(("Timestamp: 2026-05-25T12:00:01Z\nEvent: " +
			"websocket.response\n{}")),
	); errAppend !=
		nil {
		t.Fatalf("AppendPart response: %v", errAppend)
	}
	partPaths := timelineSource.Paths()
	for _, path := range partPaths {
		if !strings.HasPrefix(path, logsDir+string(os.PathSeparator)) {
			t.Fatalf("part path %s is not under logs dir %s", path, logsDir)
		}
	}

	errLog := logger.LogRequestWithOptionsAndAllSources(
		"/v1/responses/ws", http.MethodGet,
		map[string][]string{"Upgrade": {"websocket"}}, nil,
		http.StatusSwitchingProtocols, map[string][]string{"Upgrade": {"websocket"}},
		nil, nil, timelineSource, nil, nil, nil, nil, nil, nil, nil, nil,
		false, "ws-req-1", time.Now(), time.Now(),
	)
	if errLog != nil {
		t.Fatalf("LogRequestWithOptionsAndSources error: %v", errLog)
	}

	assertFileBodySourceCleaned(t, partPaths)

	entries, errRead := os.ReadDir(logsDir)
	if errRead != nil {
		t.Fatalf("failed to read logs dir: %v", errRead)
	}
	var logPath string
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		logPath = logsDir + string(os.PathSeparator) + entry.Name()
		break
	}
	if logPath == "" {
		t.Fatal("expected local request log file")
	}
	raw, errReadLog := os.ReadFile(logPath)
	if errReadLog != nil {
		t.Fatalf("read log file: %v", errReadLog)
	}
	if !bytes.Contains(raw, []byte("=== WEBSOCKET TIMELINE ===")) {
		t.Fatalf("websocket timeline section missing: %s", string(raw))
	}
	if !bytes.Contains(raw, []byte("Event: websocket.request")) || !bytes.Contains(
		raw, []byte("Event: websocket.response"),
	) {
		t.Fatalf("merged websocket events missing: %s", string(raw))
	}
}

func TestFileBodySourceRecoveryRenewsLeaseAndRetriesFailedAppend(t *testing.T) {
	for _, appendMethod := range []struct {
		name   string
		append func(*FileBodySource, []byte) error
		want   string
	}{
		{name: "parts", append: (*FileBodySource).AppendPart, want: "after manual cleanup\n"},
		{name: "bytes", append: (*FileBodySource).AppendBytes, want: "after manual cleanup"},
	} {
		t.Run(
			appendMethod.name, func(t *testing.T) {
				logsDir := t.TempDir()
				source, errSource := NewFileBodySourceInDir(logsDir, "recovery-test")
				if errSource != nil {
					t.Fatal(errSource)
				}
				t.Cleanup(
					func() {
						if errCleanup := source.Cleanup(); errCleanup != nil {
							t.Errorf("Cleanup: %v", errCleanup)
						}
					},
				)
				if errAppend := appendMethod.append(source, []byte("before manual cleanup")); errAppend != nil {
					t.Fatal(errAppend)
				}
				previousDir, previousLease := source.dir, source.ownerLease
				previousIdentity, previousBytes := previousLease.identity, source.capturedBytes
				if errRemove := os.RemoveAll(logsDir); errRemove != nil {
					t.Fatal(errRemove)
				}
				if errBlock := os.WriteFile(logsDir, []byte("blocked"), 0o600); errBlock != nil {
					t.Fatal(errBlock)
				}
				if errAppend := appendMethod.append(source, []byte("failed append")); errAppend == nil {
					t.Fatal("expected append to fail while the parent path is a file")
				}
				if source.dir != previousDir || source.ownerLease != previousLease ||
					source.capturedBytes != previousBytes {
					t.Fatal("failed recovery changed the source ownership or confirmed byte count")
				}
				if errUnblock := os.Remove(logsDir); errUnblock != nil {
					t.Fatal(errUnblock)
				}
				if errAppend := appendMethod.append(source, []byte("after manual cleanup")); errAppend != nil {
					t.Fatal(errAppend)
				}
				raw, truncated, errRead := source.ReadRawBytes(1024)
				if errRead != nil || truncated || string(raw) != appendMethod.want {
					t.Fatalf("recovered payload = %q, truncated = %t, error = %v", raw, truncated, errRead)
				}
				if len(source.Paths()) != 1 || source.capturedBytes != int64(len(raw)) ||
					!source.cleanupSlotReserved {
					t.Fatalf(
						"recovered source paths = %v, captured = %d, reserved = %t", source.Paths(),
						source.capturedBytes, source.cleanupSlotReserved,
					)
				}
				if previousLease.file != nil || isActiveRequestLogLease(previousLease.path, previousIdentity) {
					t.Fatal("removed directory lease was not released")
				}
				lease := source.ownerLease
				identity, errStat := os.Stat(lease.path)
				if errStat != nil || lease.file == nil || !os.SameFile(
					identity, lease.identity,
				) || !isActiveRequestLogLease(lease.path, identity) {
					t.Fatalf("recreated owner lease is not active: %v", errStat)
				}
				_ = NewFileRequestLogger(true, logsDir, "", 0)
				if _, errStat := os.Stat(source.dir); errStat != nil {
					t.Fatalf("active recovered source was removed by logger cleanup: %v", errStat)
				}
				partPaths := source.Paths()
				if errCleanup := source.Cleanup(); errCleanup != nil {
					t.Fatal(errCleanup)
				}
				assertFileBodySourceCleaned(t, partPaths)
				if source.cleanupSlotReserved || isActiveRequestLogLease(lease.path, identity) {
					t.Fatal("cleanup retained the recovered source reservation or owner lease")
				}
			},
		)
	}
}

func assertFileBodySourceCleaned(t *testing.T, partPaths []string) {
	t.Helper()

	dirs := make(map[string]struct{}, len(partPaths))
	for _, path := range partPaths {
		if _, errStat := os.Stat(path); !os.IsNotExist(errStat) {
			t.Fatalf("expected part %s to be removed, stat err=%v", path, errStat)
		}
		dirs[filepath.Dir(path)] = struct{}{}
	}
	for dir := range dirs {
		if _, errStat := os.Stat(dir); !os.IsNotExist(errStat) {
			t.Fatalf("expected part dir %s to be removed, stat err=%v", dir, errStat)
		}
	}
}
