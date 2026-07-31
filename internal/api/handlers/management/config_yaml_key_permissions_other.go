//go:build !windows

package management

import (
	"os"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/util"
)

func secureConfigYAMLRedactionKeyFile(file *os.File, directory bool) error {
	return util.SecurePrivateFile(file, directory)
}
