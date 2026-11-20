package logging

import (
	"math"
	"strconv"
	"testing"
)

func TestLogDirectoryCapacityCannotOverflowIntoDisabledCleanup(t *testing.T) {
	maxMB := int64(math.MaxInt64 / (1024 * 1024))
	if strconv.IntSize < 64 {
		t.Skip("overflow cases require a 64-bit int")
	}
	for _, size := range []int64{1, maxMB, maxMB + 1, math.MaxInt64, 0, -1} {
		t.Run(
			strconv.FormatInt(size, 10), func(t *testing.T) {
				configureLogDirCleanerLocked(t.TempDir(), int(size), "")
				t.Cleanup(stopLogDirCleanerLocked)
				if active := logDirCleanerCancel != nil; active != (size > 0) {
					t.Fatalf("configured MB=%d, cleanup active=%t", size, active)
				}
			},
		)
	}
}
