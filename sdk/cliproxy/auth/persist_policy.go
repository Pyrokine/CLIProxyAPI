package auth

import (
	"context"
	"errors"
)

type skipPersistContextKey struct{}
type deferAPIKeyModelAliasRebuildContextKey struct{}
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

// WithDeferredAPIKeyModelAliasRebuild 延迟本批次的别名表发布，调用方在批次结束后调用 RefreshAPIKeyModelAlias
func WithDeferredAPIKeyModelAliasRebuild(ctx context.Context) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithValue(ctx, deferAPIKeyModelAliasRebuildContextKey{}, true)
}

func shouldDeferAPIKeyModelAliasRebuild(ctx context.Context) bool {
	if ctx == nil {
		return false
	}
	deferred, _ := ctx.Value(deferAPIKeyModelAliasRebuildContextKey{}).(bool)
	return deferred
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

// ErrAuthGenerationConflict means a conditional mutation no longer owns the credential.
var ErrAuthGenerationConflict = errors.New("auth generation conflict")

type expectedAuthGenerationContextKey struct{}

// WithExpectedAuthGeneration guards MutatePersistentAuth before its durable callback.
// Generation zero requires the credential to be absent; unbound contexts keep unconditional semantics.
func WithExpectedAuthGeneration(ctx context.Context, generation uint64) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithValue(ctx, expectedAuthGenerationContextKey{}, generation)
}
