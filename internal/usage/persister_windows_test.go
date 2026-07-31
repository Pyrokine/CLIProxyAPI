//go:build windows

package usage

import (
	"os"
	"testing"

	"golang.org/x/sys/windows"
)

func TestIsCrossDeviceErrorRecognizesWindowsNotSameDevice(t *testing.T) {
	err := &os.LinkError{
		Op:  "rename",
		Old: `C:\usage`,
		New: `D:\usage`,
		Err: windows.ERROR_NOT_SAME_DEVICE,
	}
	if !isCrossDeviceError(err) {
		t.Fatalf("isCrossDeviceError(%v) = false", err)
	}
}
