package auth

import "context"

type skipPersistContextKey struct{}
type forcePersistContextKey struct{}
type allowAuthRecreateContextKey struct{}
type durableAuthPublicationContextKey struct{}
type runtimeOnlyRemoveContextKey struct{}

// WithSkipPersist returns a derived context that disables persistence for Manager Update/Register calls.
// It is intended for code paths that are reacting to file watcher events, where the file on disk is
// already the source of truth and persisting again would create a write-back loop.
func WithSkipPersist(ctx context.Context) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithValue(ctx, skipPersistContextKey{}, true)
}

func shouldSkipPersist(ctx context.Context) bool {
	if ctx == nil {
		return false
	}
	v := ctx.Value(skipPersistContextKey{})
	enabled, ok := v.(bool)
	return ok && enabled
}

// WithForcePersist keeps a superseding watcher mutation durable despite WithSkipPersist.
func WithForcePersist(ctx context.Context) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithValue(ctx, forcePersistContextKey{}, true)
}

func shouldForcePersist(ctx context.Context) bool {
	if ctx == nil {
		return false
	}
	v := ctx.Value(forcePersistContextKey{})
	enabled, ok := v.(bool)
	return ok && enabled
}

// WithAllowAuthRecreate marks an auth update as an intentional re-creation after removal.
func WithAllowAuthRecreate(ctx context.Context) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithValue(ctx, allowAuthRecreateContextKey{}, true)
}

func shouldAllowAuthRecreate(ctx context.Context) bool {
	if ctx == nil {
		return false
	}
	v := ctx.Value(allowAuthRecreateContextKey{})
	enabled, ok := v.(bool)
	return ok && enabled
}

// WithDurableAuthPublication keeps runtime publication alive after persistence succeeds.
func WithDurableAuthPublication(ctx context.Context) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithValue(ctx, durableAuthPublicationContextKey{}, true)
}

func shouldCompleteDurableAuthPublication(ctx context.Context) bool {
	if ctx == nil {
		return false
	}
	v := ctx.Value(durableAuthPublicationContextKey{})
	enabled, ok := v.(bool)
	return ok && enabled
}

// WithRuntimeOnlyRemove marks a removal whose durable store deletion has already completed.
// noinspection GoUnusedExportedFunction
func WithRuntimeOnlyRemove(ctx context.Context) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithValue(ctx, runtimeOnlyRemoveContextKey{}, true)
}

func shouldRuntimeOnlyRemove(ctx context.Context) bool {
	if ctx == nil {
		return false
	}
	v := ctx.Value(runtimeOnlyRemoveContextKey{})
	enabled, ok := v.(bool)
	return ok && enabled
}
