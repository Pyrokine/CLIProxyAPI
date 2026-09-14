package logging

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestRequestLogWritersKeepRouteTraceInPreviewTail(t *testing.T) {
	responseBody := []byte(strings.Repeat("response-line\n", 400))
	requestTimeline := []byte(
		"=== REQUEST TIMELINE ===\n2026-01-01T00:00:00Z | +0ms | request.received\n\n" +
			"=== ROUTE TRACE ===\n{\"sequence\":1,\"action\":\"route.decision\"}\n\n",
	)

	t.Run(
		"non-streaming", func(t *testing.T) {
			var output bytes.Buffer
			logger := &FileRequestLogger{}
			errWrite := logger.writeNonStreamingLog(
				&output,
				"/v1/messages",
				"POST",
				nil,
				nil,
				nil,
				nil,
				nil,
				nil,
				nil,
				nil,
				nil,
				nil,
				requestTimeline,
				nil,
				200,
				nil,
				responseBody,
				nil,
				time.Now(),
				time.Time{},
			)
			if errWrite != nil {
				t.Fatalf("writeNonStreamingLog() error = %v", errWrite)
			}
			assertRouteTraceInPreviewTail(t, output.String(), 300)
		},
	)

	t.Run(
		"file streaming", func(t *testing.T) {
			responsePath := filepath.Join(t.TempDir(), "response.tmp")
			if errWrite := os.WriteFile(responsePath, responseBody, 0o600); errWrite != nil {
				t.Fatalf("WriteFile() error = %v", errWrite)
			}
			writer := &fileStreamingLogWriter{
				url:              "/v1/messages",
				method:           "POST",
				timestamp:        time.Now(),
				responseBodyPath: responsePath,
				responseStatus:   200,
				statusWritten:    true,
				requestTimeline:  requestTimeline,
			}
			var output bytes.Buffer
			if errWrite := writer.writeFinalLog(&output); errWrite != nil {
				t.Fatalf("writeFinalLog() error = %v", errWrite)
			}
			assertRouteTraceInPreviewTail(t, output.String(), 300)
		},
	)

	t.Run(
		"home streaming", func(t *testing.T) {
			client := &stubHomeRequestLogClient{heartbeatOK: true}
			writer := newHomeStreamingLogWriter(
				"/v1/messages",
				"POST",
				nil,
				nil,
				"request-id",
				func() homeRequestLogClient { return client },
				nil,
			)
			if errStatus := writer.WriteStatus(200, nil); errStatus != nil {
				t.Fatalf("WriteStatus() error = %v", errStatus)
			}
			writer.WriteChunkAsync(responseBody)
			if errTimeline := writer.WriteRequestTimeline(requestTimeline); errTimeline != nil {
				t.Fatalf("WriteRequestTimeline() error = %v", errTimeline)
			}
			if errClose := writer.Close(); errClose != nil {
				t.Fatalf("Close() error = %v", errClose)
			}
			if len(client.pushed) != 1 {
				t.Fatalf("pushed logs = %d, want 1", len(client.pushed))
			}
			var payload homeRequestLogPayload
			if errUnmarshal := json.Unmarshal(client.pushed[0], &payload); errUnmarshal != nil {
				t.Fatalf("Unmarshal() error = %v", errUnmarshal)
			}
			assertRouteTraceInPreviewTail(t, payload.RequestLog, 300)
		},
	)

	t.Run(
		"home bounded response", func(t *testing.T) {
			client := &stubHomeRequestLogClient{heartbeatOK: true}
			writer := newHomeStreamingLogWriter(
				"/v1/messages",
				"POST",
				nil,
				nil,
				"bounded-request-id",
				func() homeRequestLogClient { return client },
				nil,
			)
			if errStatus := writer.WriteStatus(200, nil); errStatus != nil {
				t.Fatalf("WriteStatus() error = %v", errStatus)
			}
			writer.WriteChunkAsync(bytes.Repeat([]byte("x"), maxInMemoryRequestLogBytes+1))
			if errTimeline := writer.WriteRequestTimeline(requestTimeline); errTimeline != nil {
				t.Fatalf("WriteRequestTimeline() error = %v", errTimeline)
			}
			if errClose := writer.Close(); errClose != nil {
				t.Fatalf("Close() error = %v", errClose)
			}
			if len(client.pushed) != 1 {
				t.Fatalf("pushed logs = %d, want 1", len(client.pushed))
			}
			var payload homeRequestLogPayload
			if errUnmarshal := json.Unmarshal(client.pushed[0], &payload); errUnmarshal != nil {
				t.Fatalf("Unmarshal() error = %v", errUnmarshal)
			}
			if !strings.Contains(payload.RequestLog, string(truncatedInMemoryRequestLog)) {
				t.Fatalf("bounded request log does not report truncation: %s", payload.RequestLog)
			}
			if !strings.Contains(payload.RequestLog, "=== ROUTE TRACE ===") {
				t.Fatalf("bounded request log omits route trace: %s", payload.RequestLog)
			}
		},
	)
}

func TestSplitRequestTimelinePayloadUsesStandaloneLastMarker(t *testing.T) {
	payload := []byte(
		"=== REQUEST TIMELINE ===\nmarker-like detail: === ROUTE TRACE ===\n\n" +
			"=== ROUTE TRACE ===\n{\"action\":\"route.decision\"}\n",
	)
	timeline, routeTrace := splitRequestTimelinePayload(payload)
	if strings.Contains(string(timeline), "{\"action\":\"route.decision\"}") {
		t.Fatalf("timeline retained route trace: %s", timeline)
	}
	if !strings.HasPrefix(string(routeTrace), "=== ROUTE TRACE ===\n") {
		t.Fatalf("route trace = %q", routeTrace)
	}
}

func assertRouteTraceInPreviewTail(t *testing.T, output string, maxLines int) {
	t.Helper()
	markerIndex := strings.LastIndex(output, "=== ROUTE TRACE ===")
	responseIndex := strings.LastIndex(output, "response-line")
	if markerIndex < 0 || markerIndex <= responseIndex {
		t.Fatalf("route trace is not after the response body: marker=%d response=%d", markerIndex, responseIndex)
	}
	lines := strings.Split(output, "\n")
	start := max(len(lines)-maxLines, 0)
	if !strings.Contains(strings.Join(lines[start:], "\n"), "=== ROUTE TRACE ===") {
		t.Fatalf("last %d lines omit route trace", maxLines)
	}
}
