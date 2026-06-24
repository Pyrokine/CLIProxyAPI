//go:build !unix

package management

import "os"

func openLogFileNoFollow(path string) (*os.File, error) {
	return os.Open(path)
}

func isLogSymlinkError(error) bool {
	return false
}
