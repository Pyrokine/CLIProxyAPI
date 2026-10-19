package executor

import (
	"context"
	"errors"
)

// ErrNoActiveUpstreamWebsocket indicates that a turn has no live upstream socket to interrupt.
var ErrNoActiveUpstreamWebsocket = errors.New("no active upstream websocket for response.interrupt")

type websocketAuthCheckKey struct{}

// WithWebsocketAuthCheck supplies a live account-state check for the bound socket.
func WithWebsocketAuthCheck(ctx context.Context, check func(string) bool) context.Context {
	return context.WithValue(ctx, websocketAuthCheckKey{}, check)
}

// WebsocketAuthEnabled checks whether the bound credential remains enabled.
func WebsocketAuthEnabled(ctx context.Context, authID string) bool {
	if ctx == nil {
		return true
	}
	check, _ := ctx.Value(websocketAuthCheckKey{}).(func(string) bool)
	return check == nil || check(authID)
}
