package auth

import (
	"context"
	"errors"
	"fmt"
	"testing"

	internalconfig "github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
)

func TestDeferredAPIKeyAliasLegacyMutationContract(t *testing.T) {
	for _, operation := range []string{
		"register", "update", "persistent-mutation", "merge", "remove", "conditional-remove",
	} {
		t.Run(
			operation, func(t *testing.T) {
				manager := NewManager(nil, nil, nil)
				manager.SetConfig(
					&internalconfig.Config{
						GeminiKey: []internalconfig.GeminiKey{
							{
								APIKey: "synthetic-a",
								Models: []internalconfig.GeminiModel{{Name: "upstream-a", Alias: "client"}},
							},
							{
								APIKey: "synthetic-b",
								Models: []internalconfig.GeminiModel{{Name: "upstream-b", Alias: "client"}},
							},
						},
					},
				)
				auth := &Auth{
					ID: "deferred-alias-fixture", Provider: "gemini",
					Attributes: map[string]string{"api_key": "synthetic-a"},
				}
				wantBefore, wantAfter := "", "upstream-a"
				if operation != "register" {
					var err error
					auth, err = manager.Register(WithSkipPersist(t.Context()), auth)
					if err != nil {
						t.Fatal(err)
					}
					wantBefore = "upstream-a"
				}
				previousTable := manager.apiKeyModelAlias.Load()
				previousPointer := fmt.Sprintf("%p", previousTable)
				ctx := WithDeferredAPIKeyModelAliasRebuild(WithSkipPersist(t.Context()))
				var err error
				switch operation {
				case "register":
					_, err = manager.Register(ctx, auth)
				case "update":
					auth.Attributes["api_key"] = "synthetic-b"
					_, err = manager.Update(ctx, auth)
					wantAfter = "upstream-b"
				case "persistent-mutation":
					next := auth.Clone()
					next.Attributes["api_key"] = "synthetic-b"
					_, err = manager.MutatePersistentAuth(
						WithExpectedAuthGeneration(ctx, auth.Generation), auth.ID,
						func(context.Context) (*Auth, error) {
							return next, nil
						},
					)
					wantAfter = "upstream-b"
				case "merge":
					next := auth.Clone()
					next.Attributes["api_key"] = "synthetic-b"
					_, err = manager.MergePersistentUpdate(ctx, auth, next)
					wantAfter = "upstream-b"
				case "remove":
					err = manager.Remove(ctx, auth.ID)
					wantAfter = ""
				case "conditional-remove":
					var removed bool
					removed, err = manager.RemoveIfGeneration(WithRuntimeOnlyRemove(ctx), auth.ID, auth.Generation)
					if !removed {
						t.Errorf("current generation removal was rejected: %v", err)
					}
					wantAfter = ""
				}
				if err != nil {
					t.Fatal(err)
				}
				if current := fmt.Sprintf("%p", manager.apiKeyModelAlias.Load()); current != previousPointer {
					t.Fatalf(
						"deferred mutation published alias table before explicit refresh: %s -> %s", previousPointer,
						current,
					)
				}
				if got := manager.lookupAPIKeyUpstreamModel(auth.ID, "client"); got != wantBefore {
					t.Fatalf("pre-refresh alias=%q, want %q", got, wantBefore)
				}
				manager.RefreshAPIKeyModelAlias()
				if got := manager.lookupAPIKeyUpstreamModel(auth.ID, "client"); got != wantAfter {
					t.Fatalf("flushed alias=%q, want %q", got, wantAfter)
				}
				if fmt.Sprintf("%p", manager.apiKeyModelAlias.Load()) == previousPointer {
					t.Fatal("explicit refresh did not publish a replacement table")
				}
				if got := previousTable.(apiKeyModelAliasTable)[auth.ID]["client"]; got != wantBefore {
					t.Fatalf("refresh mutated a previously published alias table: %q, want %q", got, wantBefore)
				}
			},
		)
	}
}

func TestDeferredAPIKeyAliasFlushKeepsCommittedStateAndLatestConfig(t *testing.T) {
	store := &auditLegacyUpdateStore{}
	manager := NewManager(store, nil, nil)
	cfg := &internalconfig.Config{
		GeminiKey: []internalconfig.GeminiKey{
			{APIKey: "synthetic-a", Models: []internalconfig.GeminiModel{{Name: "upstream-a", Alias: "client"}}},
			{APIKey: "synthetic-b", Models: []internalconfig.GeminiModel{{Name: "upstream-b", Alias: "client"}}},
		},
	}
	manager.SetConfig(cfg)
	ctx := WithDeferredAPIKeyModelAliasRebuild(t.Context())
	auth, err := manager.Register(
		ctx, &Auth{
			ID: "committed", Provider: "gemini",
			Attributes: map[string]string{"api_key": "synthetic-a"}, Metadata: map[string]any{"note": "committed"},
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if got := manager.lookupAPIKeyUpstreamModel(auth.ID, "client"); got != "" {
		t.Fatalf("first deferred commit published %q", got)
	}
	store.fail = errors.New("synthetic durable write failure")
	next := auth.Clone()
	next.Attributes["api_key"] = "synthetic-b"
	if updated, err := manager.Update(ctx, next); updated != nil || !errors.Is(err, store.fail) {
		t.Fatalf("failed update=%+v, error=%v", updated, err)
	}
	if failed, err := manager.Register(
		ctx, &Auth{
			ID: "rejected", Provider: "gemini",
			Attributes: map[string]string{"api_key": "synthetic-b"}, Metadata: map[string]any{"note": "rejected"},
		},
	); failed != nil || !errors.Is(err, store.fail) {
		t.Fatalf("failed register=%+v, error=%v", failed, err)
	}
	cancelCtx, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := manager.Register(cancelCtx, &Auth{ID: "canceled", Provider: "gemini"}); !errors.Is(
		err, context.Canceled,
	) {
		t.Fatalf("canceled register error=%v", err)
	}
	manager.RefreshAPIKeyModelAlias()
	if got := manager.lookupAPIKeyUpstreamModel(auth.ID, "client"); got != "upstream-a" {
		t.Fatalf("flush lost the committed state: %q", got)
	}
	current, _ := manager.GetByID(auth.ID)
	if current == nil || current.Generation != auth.Generation || current.Attributes["api_key"] != "synthetic-a" ||
		store.saved.Attributes["api_key"] != "synthetic-a" {
		t.Fatal("rejected update changed the committed generation or stored snapshot")
	}
	for _, id := range []string{"rejected", "canceled"} {
		if _, exists := manager.GetByID(id); exists || manager.lookupAPIKeyUpstreamModel(id, "client") != "" {
			t.Fatalf("flush published rejected credential %q", id)
		}
	}
	cfg = &internalconfig.Config{
		GeminiKey: []internalconfig.GeminiKey{
			{
				APIKey: "synthetic-a",
				Models: []internalconfig.GeminiModel{{Name: "latest-upstream", Alias: "client"}},
			},
		},
	}
	manager.SetConfig(cfg)
	if got := manager.lookupAPIKeyUpstreamModel(auth.ID, "client"); got != "latest-upstream" {
		t.Fatalf("config reload did not publish immediately: %q", got)
	}
	manager.RefreshAPIKeyModelAlias()
	if got := manager.lookupAPIKeyUpstreamModel(auth.ID, "client"); got != "latest-upstream" {
		t.Fatalf("explicit flush restored an old config: %q", got)
	}
}

func TestDeferredAPIKeyAliasContextAndNilRefresh(t *testing.T) {
	//lint:ignore SA1012 验证 nil context 默认保持即时发布
	if shouldDeferAPIKeyModelAliasRebuild(nil) || shouldDeferAPIKeyModelAliasRebuild(context.Background()) {
		t.Fatal("normal mutations unexpectedly defer alias publication")
	}
	//lint:ignore SA1012 验证 nil 父 context 可启用延迟发布
	if !shouldDeferAPIKeyModelAliasRebuild(WithDeferredAPIKeyModelAliasRebuild(nil)) {
		t.Fatal("nil-parent compatibility context did not enable deferred publication")
	}
	var manager *Manager
	manager.RefreshAPIKeyModelAlias()
}
