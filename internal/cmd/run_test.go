package cmd

import (
	"net"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
)

func TestStartServiceBackgroundReturnsListenerStartupError(t *testing.T) {
	listener, errListen := net.Listen("tcp", "127.0.0.1:0")
	if errListen != nil {
		t.Fatal(errListen)
	}
	defer func() { _ = listener.Close() }()

	cfg := &config.Config{
		Host:    "127.0.0.1",
		Port:    listener.Addr().(*net.TCPAddr).Port,
		AuthDir: t.TempDir(),
	}
	cfg.Plugins.Dir = t.TempDir()
	cancel, done := StartServiceBackgroundWithPluginHost(
		cfg,
		filepath.Join(t.TempDir(), "config.yaml"),
		"",
		nil,
	)
	defer cancel()
	select {
	case errStart := <-done:
		if errStart == nil || !strings.Contains(errStart.Error(), "failed to start HTTP server") {
			t.Fatalf("background service error = %v, want listener startup error", errStart)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("background service did not report listener startup error")
	}
}

func TestStartServiceWithPluginHostReturnsListenerStartupError(t *testing.T) {
	listener, errListen := net.Listen("tcp", "127.0.0.1:0")
	if errListen != nil {
		t.Fatal(errListen)
	}
	defer func() { _ = listener.Close() }()

	cfg := &config.Config{
		Host:    "127.0.0.1",
		Port:    listener.Addr().(*net.TCPAddr).Port,
		AuthDir: t.TempDir(),
	}
	cfg.Plugins.Dir = t.TempDir()
	errStart := StartServiceWithPluginHost(
		cfg,
		filepath.Join(t.TempDir(), "config.yaml"),
		"",
		nil,
	)
	if errStart == nil || !strings.Contains(errStart.Error(), "failed to start HTTP server") {
		t.Fatalf("StartServiceWithPluginHost() error = %v, want listener startup error", errStart)
	}
}
