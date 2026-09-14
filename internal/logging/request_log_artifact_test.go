package logging

import (
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

type recordingRequestLogArtifactSink struct {
	mu        sync.Mutex
	artifacts []RequestLogArtifact
}

func (s *recordingRequestLogArtifactSink) SubmitRequestLogArtifact(artifact RequestLogArtifact) bool {
	s.mu.Lock()
	s.artifacts = append(s.artifacts, artifact)
	s.mu.Unlock()
	return true
}

func (s *recordingRequestLogArtifactSink) artifactsFor(requestID string) []RequestLogArtifact {
	s.mu.Lock()
	defer s.mu.Unlock()
	var artifacts []RequestLogArtifact
	for _, artifact := range s.artifacts {
		if artifact.RequestID == requestID {
			artifacts = append(artifacts, artifact)
		}
	}
	return artifacts
}

type failingHomeRequestLogClient struct {
	err error
}

func (*failingHomeRequestLogClient) HeartbeatOK() bool { return true }

func (c *failingHomeRequestLogClient) RPushRequestLog(context.Context, []byte) error {
	return c.err
}

func logRequestForArtifactTest(logger *FileRequestLogger, requestID string) error {
	return logger.LogRequest(
		"/v1/responses",
		http.MethodPost,
		map[string][]string{"Content-Type": {"application/json"}},
		[]byte(`{"model":"test-model"}`),
		http.StatusBadGateway,
		map[string][]string{"Content-Type": {"application/json"}},
		[]byte(`{"error":"upstream failure"}`),
		nil,
		nil,
		nil,
		nil,
		nil,
		requestID,
		time.Now().UTC(),
		time.Now().UTC(),
	)
}

func TestFileRequestLoggerRecordsAvailableLocalArtifact(t *testing.T) {
	logsDir := t.TempDir()
	logger := NewFileRequestLogger(true, logsDir, "", 0)
	sink := &recordingRequestLogArtifactSink{}
	logger.SetRequestLogArtifactSink(sink)

	const requestID = "requestlocal"
	if err := logRequestForArtifactTest(logger, requestID); err != nil {
		t.Fatalf("LogRequest: %v", err)
	}
	artifacts := sink.artifactsFor(requestID)
	if len(artifacts) != 1 {
		t.Fatalf("artifacts = %#v, want one available artifact", artifacts)
	}
	artifact := artifacts[0]
	if artifact.State != RequestLogAvailable || artifact.Location != RequestLogLocationLocal || artifact.FileName == "" {
		t.Fatalf("artifact = %#v, want available local artifact", artifact)
	}
	if _, err := os.Stat(filepath.Join(logsDir, artifact.FileName)); err != nil {
		t.Fatalf("published request log is unavailable: %v", err)
	}
}

func TestFileRequestLoggerRecordsPublishFailedArtifact(t *testing.T) {
	root := t.TempDir()
	logsPath := filepath.Join(root, "logs-file")
	if err := os.WriteFile(logsPath, []byte("not a directory"), 0o600); err != nil {
		t.Fatalf("write blocking path: %v", err)
	}
	logger := NewFileRequestLogger(true, logsPath, "", 0)
	sink := &recordingRequestLogArtifactSink{}
	logger.SetRequestLogArtifactSink(sink)

	const requestID = "requestfailed"
	if err := logRequestForArtifactTest(logger, requestID); err == nil {
		t.Fatal("LogRequest returned nil error for an invalid log directory")
	}
	artifacts := sink.artifactsFor(requestID)
	if len(artifacts) != 1 || artifacts[0].State != RequestLogPublishFailed || artifacts[0].Reason == "" {
		t.Fatalf("artifacts = %#v, want one publish_failed artifact with a reason", artifacts)
	}
}

func TestFileRequestLoggerRecordsExpiredArtifactAfterRetention(t *testing.T) {
	logsDir := t.TempDir()
	logger := NewFileRequestLogger(false, logsDir, "", 0)
	sink := &recordingRequestLogArtifactSink{}
	logger.SetRequestLogArtifactSink(sink)

	oldPath := filepath.Join(logsDir, "error-v1-responses-2026-08-30T100000-requestold.log")
	newPath := filepath.Join(logsDir, "error-v1-responses-2026-08-30T100001-requestnew.log")
	for _, path := range []string{oldPath, newPath} {
		if err := os.WriteFile(path, []byte("request log"), 0o600); err != nil {
			t.Fatalf("write request log %s: %v", path, err)
		}
	}
	oldTime := time.Date(2026, 8, 30, 10, 0, 0, 0, time.UTC)
	newTime := oldTime.Add(time.Second)
	if err := os.Chtimes(oldPath, oldTime, oldTime); err != nil {
		t.Fatalf("set old request log time: %v", err)
	}
	if err := os.Chtimes(newPath, newTime, newTime); err != nil {
		t.Fatalf("set new request log time: %v", err)
	}

	if err := logger.cleanupOldErrorLogs(1); err != nil {
		t.Fatalf("cleanupOldErrorLogs: %v", err)
	}
	if _, err := os.Stat(oldPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("old request log still exists: %v", err)
	}
	if _, err := os.Stat(newPath); err != nil {
		t.Fatalf("new request log was removed: %v", err)
	}
	artifacts := sink.artifactsFor("requestold")
	if len(artifacts) != 1 || artifacts[0].State != RequestLogExpired {
		t.Fatalf("artifacts = %#v, want one expired artifact", artifacts)
	}
}

func TestStreamingRequestLogRecordsTerminalArtifacts(t *testing.T) {
	t.Run(
		"local available", func(t *testing.T) {
			logger := NewFileRequestLogger(true, t.TempDir(), "", 0)
			sink := &recordingRequestLogArtifactSink{}
			logger.SetRequestLogArtifactSink(sink)
			writer, err := logger.LogStreamingRequest(
				"/v1/responses",
				http.MethodPost,
				nil,
				[]byte(`{"stream":true}`),
				"streamlocal",
			)
			if err != nil {
				t.Fatalf("LogStreamingRequest: %v", err)
			}
			writer.WriteChunkAsync([]byte("data: {}\n\n"))
			if err = writer.Close(); err != nil {
				t.Fatalf("Close: %v", err)
			}
			artifacts := sink.artifactsFor("streamlocal")
			if len(artifacts) != 1 || artifacts[0].State != RequestLogAvailable ||
				artifacts[0].Location != RequestLogLocationLocal {
				t.Fatalf("artifacts = %#v, want available local artifact", artifacts)
			}
		},
	)

	t.Run(
		"home available", func(t *testing.T) {
			logger := NewFileRequestLogger(true, t.TempDir(), "", 0)
			logger.SetHomeEnabled(true)
			logger.homeClientProvider = func() homeRequestLogClient {
				return &stubHomeRequestLogClient{heartbeatOK: true}
			}
			sink := &recordingRequestLogArtifactSink{}
			logger.SetRequestLogArtifactSink(sink)
			writer, err := logger.LogStreamingRequest(
				"/v1/responses",
				http.MethodPost,
				nil,
				[]byte(`{"stream":true}`),
				"streamhome",
			)
			if err != nil {
				t.Fatalf("LogStreamingRequest: %v", err)
			}
			writer.WriteChunkAsync([]byte("data: {}\n\n"))
			if err = writer.Close(); err != nil {
				t.Fatalf("Close: %v", err)
			}
			artifacts := sink.artifactsFor("streamhome")
			if len(artifacts) != 1 || artifacts[0].State != RequestLogAvailable ||
				artifacts[0].Location != RequestLogLocationHome {
				t.Fatalf("artifacts = %#v, want available Home artifact", artifacts)
			}
		},
	)

	t.Run(
		"home publish failed", func(t *testing.T) {
			logger := NewFileRequestLogger(true, t.TempDir(), "", 0)
			logger.SetHomeEnabled(true)
			logger.homeClientProvider = func() homeRequestLogClient {
				return &failingHomeRequestLogClient{err: errors.New("home push failed")}
			}
			sink := &recordingRequestLogArtifactSink{}
			logger.SetRequestLogArtifactSink(sink)
			writer, err := logger.LogStreamingRequest(
				"/v1/responses",
				http.MethodPost,
				nil,
				[]byte(`{"stream":true}`),
				"streamfailed",
			)
			if err != nil {
				t.Fatalf("LogStreamingRequest: %v", err)
			}
			writer.WriteChunkAsync([]byte("data: {}\n\n"))
			if err = writer.Close(); err == nil {
				t.Fatal("Close returned nil error for failed Home publish")
			}
			artifacts := sink.artifactsFor("streamfailed")
			if len(artifacts) != 1 || artifacts[0].State != RequestLogPublishFailed || artifacts[0].Reason == "" {
				t.Fatalf("artifacts = %#v, want publish_failed artifact", artifacts)
			}
		},
	)
}
