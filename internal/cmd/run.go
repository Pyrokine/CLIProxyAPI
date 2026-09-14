// Package cmd provides command-line interface functionality for the CLI Proxy API server.
// It includes authentication flows for various AI service providers, service startup,
// and other command-line operations.
package cmd

import (
	"context"
	"errors"
	"fmt"
	"os/signal"
	"syscall"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	sdkapi "github.com/Pyrokine/CLIProxyAPI/v6/sdk/api"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy"
	log "github.com/sirupsen/logrus"
)

// StartServiceWithPluginHost builds and runs the proxy service with a shared plugin host.
func StartServiceWithPluginHost(
	cfg *config.Config,
	configPath string,
	localPassword string,
	host *cliproxy.PluginHost,
	opts ...sdkapi.ServerOption,
) error {
	builder := cliproxy.NewBuilder().
		WithConfig(cfg).
		WithConfigPath(configPath).
		WithLocalManagementPassword(localPassword).
		WithServerOptions(opts...)
	if host != nil {
		builder = builder.WithPluginHost(host)
	}

	ctxSignal, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	service, err := builder.Build()
	if err != nil {
		return fmt.Errorf("failed to build proxy service: %w", err)
	}

	err = service.Run(ctxSignal)
	if err != nil && !errors.Is(err, context.Canceled) {
		return fmt.Errorf("proxy service exited with error: %w", err)
	}
	return nil
}

// StartServiceBackgroundWithPluginHost starts the proxy service with a shared plugin host.
func StartServiceBackgroundWithPluginHost(
	cfg *config.Config,
	configPath string,
	localPassword string,
	host *cliproxy.PluginHost,
	opts ...sdkapi.ServerOption,
) (
	cancel func(),
	done <-chan error,
) {
	builder := cliproxy.NewBuilder().
		WithConfig(cfg).
		WithConfigPath(configPath).
		WithLocalManagementPassword(localPassword).
		WithServerOptions(opts...)
	if host != nil {
		builder = builder.WithPluginHost(host)
	}

	ctx, cancelFn := context.WithCancel(context.Background())
	doneCh := make(chan error, 1)

	service, err := builder.Build()
	if err != nil {
		doneCh <- fmt.Errorf("failed to build proxy service: %w", err)
		close(doneCh)
		return cancelFn, doneCh
	}

	go func() {
		defer close(doneCh)
		errRun := service.Run(ctx)
		if errors.Is(errRun, context.Canceled) {
			errRun = nil
		}
		doneCh <- errRun
	}()

	return cancelFn, doneCh
}

// WaitForCloudDeploy waits indefinitely for shutdown signals in cloud deploy mode
// when no configuration file is available.
func WaitForCloudDeploy() {
	// Clarify that we are intentionally idle for configuration and not running the API server.
	log.Info(
		"Cloud deploy mode: No config found; standing by for configuration. API server is not started. Press Ctrl+C to exit.",
	)

	ctxSignal, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	// Block until shutdown signal is received
	<-ctxSignal.Done()
	log.Info("Cloud deploy mode: Shutdown signal received; exiting")
}
