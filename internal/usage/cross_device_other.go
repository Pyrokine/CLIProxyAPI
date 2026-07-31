//go:build !windows

package usage

func isNativeCrossDeviceError(error) bool {
	return false
}
