package home

import (
	"context"
	"sync/atomic"
	"time"
)

var currentClient atomic.Pointer[Client]

// KVClient exposes the Home key-value operations used by request-scoped runtime state.
type KVClient interface {
	Enabled() bool
	HeartbeatOK() bool
	KVGet(ctx context.Context, key string) ([]byte, bool, error)
	KVSet(ctx context.Context, key string, value []byte, opts KVSetOptions) (bool, error)
	KVDel(ctx context.Context, keys ...string) (int64, error)
	KVExpire(ctx context.Context, key string, ttl time.Duration) (bool, error)
}

// ClientScope identifies the Home client and logical service handling a request.
type ClientScope struct {
	Client  KVClient
	ID      string
	Enabled bool
}

type clientScopeContextKey struct{}

// WithClientScope binds a request to its service-scoped Home runtime.
func WithClientScope(ctx context.Context, scope ClientScope) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithValue(ctx, clientScopeContextKey{}, scope)
}

// ClientScopeFromContext returns the service-scoped Home runtime bound to a request.
func ClientScopeFromContext(ctx context.Context) (ClientScope, bool) {
	if ctx == nil {
		return ClientScope{}, false
	}
	scope, ok := ctx.Value(clientScopeContextKey{}).(ClientScope)
	return scope, ok
}

// SetCurrent sets the active home client used by runtime integrations.
func SetCurrent(client *Client) {
	currentClient.Store(client)
}

// Current returns the active home client instance, if any.
func Current() *Client {
	return currentClient.Load()
}

// ClearCurrent removes the active home client.
func ClearCurrent() {
	currentClient.Store(nil)
}

// ClearCurrentIf removes the active client only when it is client.
func ClearCurrentIf(client *Client) {
	if client != nil {
		currentClient.CompareAndSwap(client, nil)
	}
}
