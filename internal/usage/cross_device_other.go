//go:build !windows

package usage

func isNativeCrossDeviceError(_ error) bool {
	return false
}
