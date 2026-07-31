//go:build windows

package usage

import (
	"errors"

	"golang.org/x/sys/windows"
)

func isNativeCrossDeviceError(err error) bool {
	return errors.Is(err, windows.ERROR_NOT_SAME_DEVICE)
}
