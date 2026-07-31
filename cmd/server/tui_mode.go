package main

import (
	cryptorand "crypto/rand"
	"errors"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/cmd"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/logging"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/tui"
	sdkapi "github.com/Pyrokine/CLIProxyAPI/v6/sdk/api"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy"
	log "github.com/sirupsen/logrus"
)

// runTUIMode starts the terminal management UI.
// In standalone mode an embedded local server is launched first; otherwise the proxy
// server is expected to be already running.
func runTUIMode(
	cfg *config.Config,
	configFilePath string,
	password string,
	standalone bool,
	host *cliproxy.PluginHost,
	opts ...sdkapi.ServerOption,
) error {
	client, errClient := tui.NewClient(cfg.Port, password, cfg.TLS)
	if errClient != nil {
		return errClient
	}
	if standalone {
		return runTUIStandalone(cfg, configFilePath, password, host, client, opts...)
	}
	return tui.Run(client, password, nil, os.Stdout)
}

func runTUIStandalone(
	cfg *config.Config,
	configFilePath string,
	password string,
	host *cliproxy.PluginHost,
	client *tui.Client,
	opts ...sdkapi.ServerOption,
) error {
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
	defer func() {
		os.Stdout = origStdout
		os.Stderr = origStderr
		log.SetOutput(origLogOutput)
		if devNull != nil {
			_ = devNull.Close()
		}
	}()

	b := make([]byte, 16)
	_, _ = cryptorand.Read(b)
	localMgmtPassword := fmt.Sprintf("tui-%x", b)
	if password == "" {
		password = localMgmtPassword
		client.SetSecretKey(password)
	}

	cancel, done := cmd.StartServiceBackgroundWithPluginHost(cfg, configFilePath, password, host, opts...)
	ready := false
	backoff := 100 * time.Millisecond
	for range 30 {
		select {
		case errRun := <-done:
			if errRun == nil {
				errRun = errors.New("embedded server stopped before becoming ready")
			}
			return errRun
		default:
		}
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
		cancel()
		errRun := <-done
		return errors.Join(errors.New("embedded server is not ready"), errRun)
	}

	errTUI := tui.Run(client, password, hook, origStdout)
	cancel()
	errRun := <-done
	return errors.Join(errTUI, errRun)
}
