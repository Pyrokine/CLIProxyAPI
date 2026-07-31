//go:build windows

package management

import (
	"fmt"
	"os"

	"golang.org/x/sys/windows"
)

func configYAMLRedactionKeyIdentity(path string) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", fmt.Errorf("open config YAML redaction key target: %w", err)
	}
	defer func() { _ = file.Close() }()

	buffer := make([]uint16, 256)
	for {
		length, errPath := windows.GetFinalPathNameByHandle(
			windows.Handle(file.Fd()), &buffer[0], uint32(len(buffer)), 0,
		)
		if errPath != nil {
			return "", fmt.Errorf("resolve config YAML redaction key target identity: %w", errPath)
		}
		if length < uint32(len(buffer)) {
			return windows.UTF16ToString(buffer[:length]), nil
		}
		buffer = make([]uint16, length+1)
	}
}
