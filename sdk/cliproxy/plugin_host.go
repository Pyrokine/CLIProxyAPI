package cliproxy

import (
	"context"
	"flag"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/pluginhost"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/config"
)

// PluginHost owns dynamic plugin runtime state for embedders that need to
// share one plugin host across command-line setup and service startup.
type PluginHost struct {
	host *pluginhost.Host
}

// NewPluginHost creates a dynamic plugin host for advanced embedding flows.
func NewPluginHost() *PluginHost {
	return &PluginHost{host: pluginhost.New()}
}

func (h *PluginHost) internal() *pluginhost.Host {
	if h == nil {
		return nil
	}
	return h.host
}

// ApplyConfig reloads dynamic plugins from the supplied configuration.
func (h *PluginHost) ApplyConfig(ctx context.Context, cfg *config.Config) error {
	if host := h.internal(); host != nil {
		return host.ApplyConfig(ctx, cfg)
	}
	return nil
}

// RegisterCommandLineFlags registers plugin-provided command-line flags.
func (h *PluginHost) RegisterCommandLineFlags(ctx context.Context, flagSet *flag.FlagSet) {
	if host := h.internal(); host != nil {
		host.RegisterCommandLineFlags(ctx, flagSet)
	}
}

// HasTriggeredCommandLineFlags reports whether any plugin command-line flag was set.
func (h *PluginHost) HasTriggeredCommandLineFlags() bool {
	if host := h.internal(); host != nil {
		return host.HasTriggeredCommandLineFlags()
	}
	return false
}

// ExecuteCommandLine executes a plugin command-line action when a registered flag was set.
func (h *PluginHost) ExecuteCommandLine(
	ctx context.Context,
	executable string,
	args []string,
	configFilePath string,
	flagSet *flag.FlagSet,
) (int, bool) {
	if host := h.internal(); host != nil {
		return host.ExecuteCommandLine(ctx, executable, args, configFilePath, flagSet)
	}
	return 0, false
}

// PluginRegistered reports whether a plugin is active in the current runtime snapshot.
func (h *PluginHost) PluginRegistered(id string) bool {
	if host := h.internal(); host != nil {
		return host.PluginRegistered(id)
	}
	return false
}

// PluginBusy reports whether a plugin is currently handling in-flight callbacks.
func (h *PluginHost) PluginBusy(id string) bool {
	if host := h.internal(); host != nil {
		return host.PluginBusy(id)
	}
	return false
}

// PluginRuntimeIdentity returns the lifecycle identity for conditional plugin unload.
func (h *PluginHost) PluginRuntimeIdentity(id string) pluginhost.PluginRuntimeIdentity {
	if host := h.internal(); host != nil {
		return host.PluginRuntimeIdentity(id)
	}
	return pluginhost.PluginRuntimeIdentity{}
}

// UnloadPlugin requests unloading a dynamic plugin by id.
func (h *PluginHost) UnloadPlugin(id string) bool {
	unloaded, _ := h.UnloadPluginWithError(id)
	return unloaded
}

// UnloadPluginWithError unloads a plugin and reports deferred shutdown errors.
func (h *PluginHost) UnloadPluginWithError(id string) (bool, error) {
	if host := h.internal(); host != nil {
		return host.UnloadPluginWithError(id)
	}
	return false, nil
}

// UnloadPluginIfCurrentWithError unloads a plugin only while identity still owns the runtime.
func (h *PluginHost) UnloadPluginIfCurrentWithError(
	identity pluginhost.PluginRuntimeIdentity,
) (bool, error) {
	if host := h.internal(); host != nil {
		return host.UnloadPluginIfCurrentWithError(identity)
	}
	return false, nil
}
