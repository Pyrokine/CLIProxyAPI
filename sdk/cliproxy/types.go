// Package cliproxy provides the core service implementation for the CLI Proxy API.
// It includes service lifecycle management, authentication handling, file watching,
// and integration with various AI service providers through a unified interface.
package cliproxy

import (
	"context"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/redisqueue"
	coreauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/config"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/pluginapi"
)

// TokenClientProvider loads clients backed by stored authentication tokens.
// It provides an interface for loading authentication tokens from various sources
// and creating clients for AI service providers.
type TokenClientProvider interface {
	// Load loads token-based clients from the configured source.
	//
	// Parameters:
	//   - ctx: The context for the loading operation
	//   - cfg: The application configuration
	//
	// Returns:
	//   - *TokenClientResult: The result containing loaded clients
	//   - error: An error if loading fails
	Load(ctx context.Context, cfg *config.Config) (*TokenClientResult, error)
}

// TokenClientResult represents clients generated from persisted tokens.
// It contains metadata about the loading operation and the number of successful authentications.
type TokenClientResult struct {
	// SuccessfulAuthed is the number of successfully authenticated clients.
	SuccessfulAuthed int
}

// APIKeyClientProvider loads clients backed directly by configured API keys.
// It provides an interface for loading API key-based clients for various AI service providers.
type APIKeyClientProvider interface {
	// Load loads API key-based clients from the configuration.
	//
	// Parameters:
	//   - ctx: The context for the loading operation
	//   - cfg: The application configuration
	//
	// Returns:
	//   - *APIKeyClientResult: The result containing loaded clients
	//   - error: An error if loading fails
	Load(ctx context.Context, cfg *config.Config) (*APIKeyClientResult, error)
}

// APIKeyClientResult is returned by APIKeyClientProvider.Load()
type APIKeyClientResult struct {
	// GeminiKeyCount is the number of Gemini API keys loaded
	GeminiKeyCount int

	// VertexCompatKeyCount is the number of Vertex-compatible API keys loaded
	VertexCompatKeyCount int

	// ClaudeKeyCount is the number of Claude API keys loaded
	ClaudeKeyCount int

	// CodexKeyCount is the number of Codex API keys loaded
	CodexKeyCount int

	// XAIKeyCount is the number of xAI API keys loaded
	XAIKeyCount int

	// OpenAICompatCount is the number of OpenAI compatibility API keys loaded
	OpenAICompatCount int
}

type AuthUpdateAction string

const (
	AuthUpdateActionAdd    AuthUpdateAction = "add"
	AuthUpdateActionModify AuthUpdateAction = "modify"
	AuthUpdateActionDelete AuthUpdateAction = "delete"
)

type AuthUpdate struct {
	Action            AuthUpdateAction
	ID                string
	Auth              *coreauth.Auth
	AllowAuthRecreate bool
	revision          uint64
}

// Revision returns the watcher-local ordering assigned to this update.
func (u AuthUpdate) Revision() uint64 {
	return u.revision
}

// SetRevision assigns watcher-local ordering to a custom watcher update.
func (u *AuthUpdate) SetRevision(revision uint64) {
	if u != nil {
		u.revision = revision
	}
}

type PluginAuthParser interface {
	ParseAuth(context.Context, pluginapi.AuthParseRequest) (*coreauth.Auth, bool, error)
}

// PluginMultiAuthParser expands one source into multiple plugin runtime auths.
type PluginMultiAuthParser interface {
	ParseAuths(context.Context, pluginapi.AuthParseRequest) ([]*coreauth.Auth, bool, error)
}

// WatcherFactory creates a watcher for configuration and token changes.
// The reload callback receives the updated configuration when changes are detected.
//
// Parameters:
//   - configPath: The path to the configuration file to watch
//   - authDir: The directory containing authentication tokens to watch
//   - reload: The callback function to call when changes are detected
//
// Returns:
//   - *WatcherWrapper: A watcher wrapper instance
//   - error: An error if watcher creation fails
type WatcherFactory func(configPath, authDir string, reload func(*config.Config) bool) (*WatcherWrapper, error)

// WatcherWrapper exposes the subset of watcher methods required by the SDK.
type WatcherWrapper struct {
	start func(ctx context.Context) error
	stop  func() error

	setConfig                    func(cfg *config.Config)
	setConfigReloadCallback      func(callback func(*config.Config, string) bool)
	acknowledgeConfigFile        func(cfg *config.Config, expectedHash string) bool
	setPluginAuthParser          func(parser PluginAuthParser)
	setUsageQueue                func(queue *redisqueue.Queue)
	snapshotAuths                func() []*coreauth.Auth
	setUpdateQueue               func(queue chan<- AuthUpdate)
	dispatchRuntimeUpdate        func(update AuthUpdate) bool
	dispatchPersistedAuthWithRev func(update *AuthUpdate) (bool, uint64)
}

// Start proxies to the underlying watcher Start implementation.
func (w *WatcherWrapper) Start(ctx context.Context) error {
	if w == nil || w.start == nil {
		return nil
	}
	return w.start(ctx)
}

// Stop proxies to the underlying watcher Stop implementation.
func (w *WatcherWrapper) Stop() error {
	if w == nil || w.stop == nil {
		return nil
	}
	return w.stop()
}

// SetConfig updates the watcher configuration cache.
func (w *WatcherWrapper) SetConfig(cfg *config.Config) {
	if w == nil || w.setConfig == nil {
		return
	}
	w.setConfig(cfg)
}

func (w *WatcherWrapper) AcknowledgeConfigFile(cfg *config.Config, expectedHash string) bool {
	if w == nil || w.acknowledgeConfigFile == nil {
		return false
	}
	return w.acknowledgeConfigFile(cfg, expectedHash)
}

func (w *WatcherWrapper) SetConfigReloadCallback(callback func(*config.Config, string) bool) {
	if w == nil || w.setConfigReloadCallback == nil {
		return
	}
	w.setConfigReloadCallback(callback)
}

// DispatchRuntimeAuthUpdate forwards runtime auth updates (e.g., websocket providers)
// into the watcher-managed auth update queue when available.
// Returns true if the update was enqueued successfully.
func (w *WatcherWrapper) DispatchRuntimeAuthUpdate(update AuthUpdate) bool {
	if w == nil || w.dispatchRuntimeUpdate == nil {
		return false
	}
	return w.dispatchRuntimeUpdate(update)
}

// DispatchPersistedAuthUpdateWithRevision forwards an already-persisted update and returns its revision.
func (w *WatcherWrapper) DispatchPersistedAuthUpdateWithRevision(update *AuthUpdate) (bool, uint64) {
	if w == nil || w.dispatchPersistedAuthWithRev == nil {
		return false, 0
	}
	return w.dispatchPersistedAuthWithRev(update)
}

// SetPluginAuthParser updates the parser used for plugin-owned auth files.
func (w *WatcherWrapper) SetPluginAuthParser(parser PluginAuthParser) {
	if w == nil || w.setPluginAuthParser == nil {
		return
	}
	w.setPluginAuthParser(parser)
}

// SetUsageQueue assigns the service-owned usage queue.
func (w *WatcherWrapper) SetUsageQueue(queue *redisqueue.Queue) {
	if w == nil || w.setUsageQueue == nil {
		return
	}
	w.setUsageQueue(queue)
}

// SetClients updates the watcher file-backed clients registry.
// SetClients and SetAPIKeyClients removed; watcher manages its own caches

// SnapshotClients returns the current combined clients snapshot from the underlying watcher.
// SnapshotClients removed; use SnapshotAuths

// SnapshotAuths returns the current auth entries derived from legacy clients.
func (w *WatcherWrapper) SnapshotAuths() []*coreauth.Auth {
	if w == nil || w.snapshotAuths == nil {
		return nil
	}
	return w.snapshotAuths()
}

// SetAuthUpdateQueue registers the channel used to propagate auth updates.
func (w *WatcherWrapper) SetAuthUpdateQueue(queue chan<- AuthUpdate) {
	if w == nil || w.setUpdateQueue == nil {
		return
	}
	w.setUpdateQueue(queue)
}
