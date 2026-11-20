package management

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func TestTailLogCursorUsesConsumedBoundaryDuringAppend(t *testing.T) {
	for range 20 {
		path := filepath.Join(t.TempDir(), "main.log")
		line := func(n int) string { return fmt.Sprintf("[2026-11-05 12:00:00] [info] event-%06d\n", n) }
		var initial strings.Builder
		for i := range 1000 {
			initial.WriteString(line(i))
		}
		if err := os.WriteFile(path, []byte(initial.String()), 0600); err != nil {
			t.Fatal(err)
		}
		file, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0600)
		if err != nil {
			t.Fatal(err)
		}
		appendDone := make(chan error, 1)
		go func() {
			var appendErr error
			defer func(closeFile func() error) {
				appendDone <- errors.Join(appendErr, closeFile())
			}(file.Close)
			for i := 1000; i < 1500; i++ {
				if _, err := file.WriteString(line(i)); err != nil {
					appendErr = err
					return
				}
			}
		}()
		result, readErr := tailLogFiles([]string{path}, 3, 0)
		appendErr := <-appendDone
		if readErr != nil || appendErr != nil {
			t.Fatalf("read=%v append=%v", readErr, appendErr)
		}
		if len(result.lines) == 0 {
			t.Fatal("tail empty")
		}
		last := result.lines[len(result.lines)-1]
		index := strings.LastIndex(last, "event-")
		if index < 0 {
			t.Fatalf("unexpected line %q", last)
		}
		ordinal, err := strconv.Atoi(strings.TrimSpace(last[index+6:]))
		if err != nil {
			t.Fatal(err)
		}
		cursor, err := decodeLogCursor(result.nextCursor)
		if err != nil {
			t.Fatal(err)
		}
		want := int64((ordinal + 1) * len(line(0)))
		if cursor.Offset != want {
			t.Fatalf("cursor offset=%d consumed boundary=%d last=%q", cursor.Offset, want, last)
		}
		follow, reset, err := readLogFilesFromCursor(filepath.Dir(path), []string{path}, result.nextCursor, 0)
		if err != nil {
			t.Fatal(err)
		}
		if reset || len(follow.lines) != 1499-ordinal {
			t.Fatalf("follow count=%d want=%d reset=%v error=%v", len(follow.lines), 1499-ordinal, reset, err)
		}
	}
}
