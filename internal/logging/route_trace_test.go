package logging

import (
	"bytes"
	"strings"
	"testing"

	cliproxyexecutor "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
)

func TestRouteTraceBoundsAndSanitizesEvents(t *testing.T) {
	trace := NewRouteTrace()
	trace.Add(
		cliproxyexecutor.RouteTraceEvent{
			Attempt:         1,
			PreviousAttempt: 1,
			Provider:        " codex ",
			Action:          " candidate.selected ",
			HTTPStatus:      999,
			RetryAfterMS:    -1,
		},
	)
	for index := 1; index < maxRouteTraceEvents+2; index++ {
		trace.Add(cliproxyexecutor.RouteTraceEvent{Action: "candidate.excluded"})
	}

	snapshot := trace.Snapshot()
	if len(snapshot.Events) != maxRouteTraceEvents {
		t.Fatalf("events = %d, want %d", len(snapshot.Events), maxRouteTraceEvents)
	}
	if snapshot.TruncatedEvents != 2 {
		t.Fatalf("truncated events = %d, want 2", snapshot.TruncatedEvents)
	}
	first := snapshot.Events[0]
	if first.Sequence != 1 || first.Timestamp.IsZero() {
		t.Fatalf("first event sequence/timestamp = %d/%v", first.Sequence, first.Timestamp)
	}
	if first.Provider != "codex" || first.Action != "candidate.selected" {
		t.Fatalf("first event = %#v", first)
	}
	if first.PreviousAttempt != 0 || first.HTTPStatus != 0 || first.RetryAfterMS != 0 {
		t.Fatalf("invalid optional fields were not cleared: %#v", first)
	}
	if last := snapshot.Events[len(snapshot.Events)-1]; last.Sequence != maxRouteTraceEvents {
		t.Fatalf("last sequence = %d, want %d", last.Sequence, maxRouteTraceEvents)
	}
}

func TestRouteTraceRejectsSensitiveOrUnclassifiedFields(t *testing.T) {
	trace := NewRouteTrace()
	trace.Add(
		cliproxyexecutor.RouteTraceEvent{
			Provider:    strings.Repeat("a", 64) + "token",
			Model:       "https://proxy.example/model",
			TargetModel: "metadata={secret}",
			AuthIndex:   "credential.json",
			Action:      "attempt.completed",
			Reason:      "raw upstream error private-token",
			ResultCode:  "private_error_code",
		},
	)
	trace.Add(cliproxyexecutor.RouteTraceEvent{Action: "private.custom.event"})
	trace.addTruncated(3)

	snapshot := trace.Snapshot()
	if len(snapshot.Events) != 1 {
		t.Fatalf("events = %#v", snapshot.Events)
	}
	event := snapshot.Events[0]
	if event.Provider != "" || event.Model != "" || event.TargetModel != "" || event.AuthIndex != "" ||
		event.Reason != "" || event.ResultCode != "" {
		t.Fatalf("sensitive fields were retained: %#v", event)
	}
	if snapshot.TruncatedEvents != 3 {
		t.Fatalf("truncated events = %d, want 3", snapshot.TruncatedEvents)
	}
}

func TestWriteRouteTraceWritesJSONLines(t *testing.T) {
	trace := NewRouteTrace()
	trace.Add(
		cliproxyexecutor.RouteTraceEvent{
			Attempt:      1,
			Provider:     "xai",
			Model:        "grok-4",
			AuthIndex:    "0123456789abcdef",
			Action:       "attempt.completed",
			ResultCode:   "success",
			HTTPStatus:   200,
			FinalTarget:  true,
			RetryAfterMS: 250,
		},
	)

	var output bytes.Buffer
	if errWrite := WriteRouteTrace(&output, trace.Snapshot()); errWrite != nil {
		t.Fatal(errWrite)
	}
	text := output.String()
	for _, expected := range []string{
		"=== ROUTE TRACE ===",
		`"sequence":1`,
		`"provider":"xai"`,
		`"auth_index":"0123456789abcdef"`,
		`"final_target":true`,
	} {
		if !strings.Contains(text, expected) {
			t.Fatalf("route trace missing %q: %s", expected, text)
		}
	}
}
