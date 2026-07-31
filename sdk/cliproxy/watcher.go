package cliproxy

import (
	"context"
	"sync"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/redisqueue"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/watcher"
	coreauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/config"
)

type authUpdateBridge struct {
	input chan watcher.AuthUpdate
	stop  chan struct{}
	done  chan struct{}
	once  sync.Once
}

func newAuthUpdateBridge(output chan<- AuthUpdate) *authUpdateBridge {
	bridge := &authUpdateBridge{
		input: make(chan watcher.AuthUpdate, 256),
		stop:  make(chan struct{}),
		done:  make(chan struct{}),
	}
	go bridge.forward(output)
	return bridge
}

func (b *authUpdateBridge) forward(output chan<- AuthUpdate) {
	defer close(b.done)
	for {
		select {
		case <-b.stop:
			return
		case update := <-b.input:
			select {
			case output <- fromWatcherAuthUpdate(update):
			case <-b.stop:
				return
			}
		}
	}
}

func (b *authUpdateBridge) close() {
	if b == nil {
		return
	}
	b.once.Do(
		func() {
			close(b.stop)
		},
	)
	<-b.done
}

func toWatcherAuthUpdate(update AuthUpdate) watcher.AuthUpdate {
	converted := watcher.AuthUpdate{
		ID:                update.ID,
		Auth:              update.Auth,
		AllowAuthRecreate: update.AllowAuthRecreate,
	}
	switch update.Action {
	case AuthUpdateActionAdd:
		converted.Action = watcher.AuthUpdateActionAdd
	case AuthUpdateActionModify:
		converted.Action = watcher.AuthUpdateActionModify
	case AuthUpdateActionDelete:
		converted.Action = watcher.AuthUpdateActionDelete
	}
	return converted
}

func fromWatcherAuthUpdate(update watcher.AuthUpdate) AuthUpdate {
	converted := AuthUpdate{
		ID:                update.ID,
		Auth:              update.Auth,
		AllowAuthRecreate: update.AllowAuthRecreate,
	}
	switch update.Action {
	case watcher.AuthUpdateActionAdd:
		converted.Action = AuthUpdateActionAdd
	case watcher.AuthUpdateActionModify:
		converted.Action = AuthUpdateActionModify
	case watcher.AuthUpdateActionDelete:
		converted.Action = AuthUpdateActionDelete
	}
	return converted
}

func defaultWatcherFactory(configPath, authDir string, reload func(*config.Config) bool) (*WatcherWrapper, error) {
	w, err := watcher.NewWatcher(configPath, authDir, reload)
	if err != nil {
		return nil, err
	}
	var bridge *authUpdateBridge

	return &WatcherWrapper{
		start: func(ctx context.Context) error {
			return w.Start(ctx)
		},
		stop: func() error {
			errStop := w.Stop()
			if bridge != nil {
				bridge.close()
				bridge = nil
			}
			return errStop
		},
		setConfig: func(cfg *config.Config) {
			w.SetConfig(cfg)
		},
		setConfigReloadCallback: func(callback func(*config.Config, string) bool) {
			w.SetConfigReloadCallback(callback)
		},
		acknowledgeConfigFile: func(cfg *config.Config, expectedHash string) bool {
			return w.AcknowledgeConfigFile(cfg, expectedHash)
		},
		setPluginAuthParser: func(parser PluginAuthParser) {
			w.SetPluginAuthParser(parser)
		},
		setUsageQueue: func(queue *redisqueue.Queue) {
			w.SetUsageQueue(queue)
		},
		snapshotAuths: func() []*coreauth.Auth { return w.SnapshotCoreAuths() },
		setUpdateQueue: func(queue chan<- AuthUpdate) {
			if bridge != nil {
				w.SetAuthUpdateQueue(nil)
				bridge.close()
				bridge = nil
			}
			if queue == nil {
				w.SetAuthUpdateQueue(nil)
				return
			}
			bridge = newAuthUpdateBridge(queue)
			w.SetAuthUpdateQueue(bridge.input)
		},
		dispatchRuntimeUpdate: func(update AuthUpdate) bool {
			return w.DispatchRuntimeAuthUpdate(toWatcherAuthUpdate(update))
		},
	}, nil
}
