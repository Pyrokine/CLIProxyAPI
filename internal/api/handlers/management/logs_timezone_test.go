package management

import (
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"
)

func utcLogLines(lines []string) []string {
	out := append([]string(nil), lines...)
	for i, line := range out {
		if len(line) >= 21 && line[0] == '[' && line[20] == ']' {
			if parsed, err := time.ParseInLocation("2006-01-02 15:04:05", line[1:20], time.Local); err == nil {
				out[i] = "[" + parsed.UTC().Format(time.RFC3339Nano) + line[20:]
			}
		}
	}
	return out
}

func TestGetLogsUTCHeadersPreserveLocalHistoryAndByteCursor(t *testing.T) {
	previous := time.Local
	time.Local = time.FixedZone("test+08", 8*60*60)
	t.Cleanup(func() { time.Local = previous })
	const old = "[2026-11-06 09:02:03] old body [unchanged]"
	const current = "[2026-11-06T01:02:03.123456789Z] new body"
	instant := time.Date(2026, 11, 6, 1, 2, 3, 0, time.UTC).Unix()
	if parseTimestamp(old) != instant || parseTimestamp(current) != instant {
		t.Fatalf("old=%d new=%d want=%d", parseTimestamp(old), parseTimestamp(current), instant)
	}
	dir := t.TempDir()
	original := old + "\n" + current + "\n"
	writeMainLog(t, dir, original)
	h := newLogsTestHandler(dir, true)
	t.Cleanup(func() { _ = h.Close() })
	response := performGetLogs(t, h, "/logs?limit=10")
	want := []string{"[2026-11-06T01:02:03Z] old body [unchanged]", current}
	if !reflect.DeepEqual(response.Lines, want) || response.LatestTimestamp != instant || response.LineCount != 2 {
		t.Fatalf("response=%+v", response)
	}
	cursor, err := decodeLogCursor(response.NextCursor)
	if err != nil || cursor.Offset != int64(len(original)) {
		t.Fatalf("cursor=%+v error=%v", cursor, err)
	}
	if data, err := os.ReadFile(filepath.Join(dir, defaultLogFileName)); err != nil || string(data) != original {
		t.Fatalf("file=%q error=%v", data, err)
	}
	before := performGetLogs(t, h, "/logs?after="+strconv.FormatInt(instant-1, 10))
	if !reflect.DeepEqual(before.Lines, want) || before.LineCount != 2 {
		t.Fatalf("after before=%+v", before)
	}
	after := performGetLogs(t, h, "/logs?after="+strconv.FormatInt(instant, 10))
	if len(after.Lines) != 0 || after.LineCount != 2 {
		t.Fatalf("after same instant=%+v", after)
	}
	const appended = "[2026-11-06T01:02:03.987654321Z] appended same second"
	appendMainLog(t, dir, appended+"\n")
	next := performGetLogs(t, h, "/logs?cursor="+response.NextCursor)
	if len(next.Lines) != 1 || !strings.Contains(
		next.Lines[0], "appended same second",
	) || next.LineCount != 1 || next.LatestTimestamp != instant {
		t.Fatalf("increment=%+v", next)
	}
	end, err := decodeLogCursor(next.NextCursor)
	if err != nil || end.Offset != int64(len(original)+len(appended)+1) {
		t.Fatalf("end=%+v error=%v", end, err)
	}
}
