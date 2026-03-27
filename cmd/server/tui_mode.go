package main

import (
	cryptorand "crypto/rand"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/cmd"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/logging"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/tui"
	log "github.com/sirupsen/logrus"
)

// runTUIMode starts the terminal management UI.
// In standalone mode an embedded local server is launched first; otherwise the proxy
// server is expected to be already running.
func runTUIMode(cfg *config.Config, configFilePath, password string, standalone bool) {
	if standalone {
		runTUIStandalone(cfg, configFilePath, password)
	} else {
		if errRun := tui.Run(cfg.Port, password, nil, os.Stdout); errRun != nil {
			_, _ = fmt.Fprintf(os.Stderr, "TUI error: %v\n", errRun)
		}
	}
}

func runTUIStandalone(cfg *config.Config, configFilePath, password string) {
	hook := tui.NewLogHook(2000)
	hook.SetFormatter(&logging.LogFormatter{})
	log.AddHook(hook)

	origStdout := os.Stdout
	origStderr := os.Stderr
	origLogOutput := log.StandardLogger().Out
	log.SetOutput(io.Discard)

	devNull, errOpenDevNull := os.Open(os.DevNull)
	if errOpenDevNull == nil {
		os.Stdout = devNull
		os.Stderr = devNull
	}

	restoreIO := func() {
		os.Stdout = origStdout
		os.Stderr = origStderr
		log.SetOutput(origLogOutput)
		if devNull != nil {
			_ = devNull.Close()
		}
	}

	b := make([]byte, 16)
	_, _ = cryptorand.Read(b)
	localMgmtPassword := fmt.Sprintf("tui-%x", b)
	if password == "" {
		password = localMgmtPassword
	}

	cancel, done := cmd.StartServiceBackground(cfg, configFilePath, password)

	client := tui.NewClient(cfg.Port, password)
	ready := false
	backoff := 100 * time.Millisecond
	for range 30 {
		if _, errGetConfig := client.GetConfig(); errGetConfig == nil {
			ready = true
			break
		}
		time.Sleep(backoff)
		if backoff < time.Second {
			backoff = time.Duration(float64(backoff) * 1.5)
		}
	}

	if !ready {
		restoreIO()
		cancel()
		<-done
		_, _ = fmt.Fprintf(os.Stderr, "TUI error: embedded server is not ready\n")
		return
	}

	if errRun := tui.Run(cfg.Port, password, hook, origStdout); errRun != nil {
		restoreIO()
		_, _ = fmt.Fprintf(os.Stderr, "TUI error: %v\n", errRun)
	} else {
		restoreIO()
	}

	cancel()
	<-done
}
