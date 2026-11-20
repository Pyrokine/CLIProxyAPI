package auth_test

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

func TestPublishedErrorFourFieldLiteralCompiles(t *testing.T) {
	directory := t.TempDir()
	probe := filepath.Join(directory, "main.go")
	source := `package main

import (
    "fmt"
    "net/http"

    auth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
)

var legacy = auth.Error{"legacy", "original SDK message", true, 429}
var marker = auth.ErrorCodeTransientTransport

func main() {
    err := auth.Error{"code", "message", false, http.StatusRequestTimeout}
    if err.Code != "code" || err.Message != "message" || err.Retryable || err.HTTPStatus != http.StatusRequestTimeout {
        panic(fmt.Sprintf("unexpected error fields: %#v", err))
    }
}
`
	if err := os.WriteFile(probe, []byte(source), 0o600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()
	//lint:ignore SA1019 源码兼容测试沿用当前测试二进制的 Go 工具链
	//noinspection GoDeprecation
	goTool := filepath.Join(runtime.GOROOT(), "bin", "go")
	command := exec.CommandContext(
		ctx, goTool, "build", "-mod=readonly", "-o",
		filepath.Join(directory, "probe"), probe,
	)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("published SDK consumer did not compile: %v\n%s", err, output)
	}
	t.Run(
		"original_fields", func(t *testing.T) {
			if output, err := exec.CommandContext(ctx, filepath.Join(directory, "probe")).CombinedOutput(); err != nil {
				t.Fatalf("published SDK consumer changed its original fields: %v\n%s", err, output)
			}
		},
	)
}
