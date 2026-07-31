//go:build windows

package xai

import (
	"os"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/util"
)

func secureTokenFile(file *os.File, directory bool) error {
	return util.SecurePrivateFile(file, directory)
}
