package cliproxy

import (
	"context"
	"fmt"
	"testing"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	coreauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
	coreexecutor "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
	sdktranslator "github.com/Pyrokine/CLIProxyAPI/v6/sdk/translator"
)

func TestDeferredAliasServiceBatchReachesRequestConsumer(t *testing.T) {
	for _, entry := range []string{"batch", "closed-queue"} {
		t.Run(
			entry, func(t *testing.T) {
				cfg := &config.Config{
					GeminiKey: []config.GeminiKey{
						{
							APIKey: "synthetic-a",
							Models: []config.GeminiModel{{Name: "gemini-2.5-flash", Alias: "batch-client"}},
						},
						{
							APIKey: "synthetic-b",
							Models: []config.GeminiModel{{Name: "gemini-2.5-pro", Alias: "batch-client"}},
						},
					},
				}
				manager := coreauth.NewManager(nil, nil, nil)
				manager.SetHomeClientProvider(func() coreauth.HomeClient { return nil })
				manager.SetConfig(cfg)
				service := &Service{cfg: cfg, coreManager: manager}
				ctx := coreauth.WithSkipPersist(t.Context())
				makeAuth := func(suffix string) *coreauth.Auth {
					auth := &coreauth.Auth{
						ID: t.Name() + suffix, Provider: "gemini", Status: coreauth.StatusActive,
						Attributes: map[string]string{"api_key": "synthetic-a"},
					}
					t.Cleanup(func() { GlobalModelRegistry().UnregisterClient(auth.ID) })
					return auth
				}
				original, err := manager.Register(ctx, makeAuth("-updated"))
				if err != nil {
					t.Fatal(err)
				}
				removed, err := manager.Register(ctx, makeAuth("-removed"))
				if err != nil {
					t.Fatal(err)
				}
				updated := original.Clone()
				updated.Attributes["api_key"] = "synthetic-b"
				added := makeAuth("-added")
				invalid := makeAuth("-invalid")
				invalid.Attributes["weight"] = "not-an-integer"
				updates := []AuthUpdate{
					{Action: AuthUpdateActionModify, Auth: updated},
					{Action: AuthUpdateActionAdd, Auth: added},
					{Action: AuthUpdateActionDelete, ID: removed.ID},
					{Action: AuthUpdateActionAdd, Auth: invalid},
					{Action: AuthUpdateActionModify, Auth: original.Clone()},
				}
				for i := range updates {
					updates[i].SetRevision(2)
				}
				updates[len(updates)-1].SetRevision(1)
				if entry == "closed-queue" {
					service.authUpdates = make(chan AuthUpdate, len(updates))
					for _, update := range updates {
						service.authUpdates <- update
					}
					close(service.authUpdates)
					service.authQueueWorkers.Add(1)
					service.consumeAuthUpdates(ctx)
					service.authQueueWorkers.Wait()
				} else {
					service.handleAuthUpdates(ctx, updates)
				}
				for _, id := range []string{removed.ID, invalid.ID} {
					if _, exists := manager.GetByID(id); exists {
						t.Fatalf("removed or invalid auth remains available: %q", id)
					}
				}
				current, _ := manager.GetByID(original.ID)
				if current == nil || current.Generation == original.Generation ||
					current.Attributes["api_key"] != "synthetic-b" ||
					service.authUpdateRevisions[original.ID] != 2 {
					t.Fatal("batch lost the current generation or accepted a stale revision")
				}
				capture := &geminiSearchServiceExecutor{
					customOwnedExecutor: &customOwnedExecutor{id: "gemini"}, cfg: cfg,
				}
				manager.RegisterExecutor(capture)
				for _, expected := range []struct{ id, model string }{
					{original.ID, "gemini-2.5-pro"}, {added.ID, "gemini-2.5-flash"},
				} {
					payload := []byte(`{"model":"batch-client","input":"check batch"}`)
					_, err := manager.Execute(
						t.Context(), []string{"gemini"}, coreexecutor.Request{Model: "batch-client", Payload: payload},
						coreexecutor.Options{
							SourceFormat: sdktranslator.FormatOpenAIResponse, OriginalRequest: payload,
							Metadata: map[string]any{coreexecutor.PinnedAuthMetadataKey: expected.id},
						},
					)
					if err != nil || capture.request.Model != expected.model {
						t.Fatalf(
							"batch alias consumer auth=%q model=%q error=%v", expected.id, capture.request.Model, err,
						)
					}
				}
				if err := manager.StopDeferredAuthRecreates(context.Background()); err != nil {
					t.Fatal(err)
				}
			},
		)
	}
}

func TestDeferredAliasConfigBatchReachesRequestConsumer(t *testing.T) {
	cfg := &config.Config{}
	for i := range 3 {
		cfg.GeminiKey = append(
			cfg.GeminiKey, config.GeminiKey{
				APIKey: fmt.Sprintf("synthetic-config-%d", i),
				Models: []config.GeminiModel{{Name: "gemini-2.5-flash", Alias: "config-batch-client"}},
			},
		)
	}
	manager := coreauth.NewManager(nil, nil, nil)
	manager.SetHomeClientProvider(func() coreauth.HomeClient { return nil })
	manager.SetConfig(cfg)
	service := &Service{cfg: cfg, coreManager: manager}
	if err := service.registerConfigAPIKeyAuths(t.Context(), cfg); err != nil {
		t.Fatal(err)
	}
	auths := manager.List()
	if len(auths) != 3 {
		t.Fatalf("config batch registered %d auths, want 3", len(auths))
	}
	for _, auth := range auths {
		t.Cleanup(func() { GlobalModelRegistry().UnregisterClient(auth.ID) })
		service.registerModelsForAuthWithConfig(auth, cfg)
	}
	capture := &geminiSearchServiceExecutor{customOwnedExecutor: &customOwnedExecutor{id: "gemini"}, cfg: cfg}
	manager.RegisterExecutor(capture)
	for _, auth := range auths {
		payload := []byte(`{"model":"config-batch-client","input":"check config batch"}`)
		_, err := manager.Execute(
			t.Context(), []string{"gemini"}, coreexecutor.Request{Model: "config-batch-client", Payload: payload},
			coreexecutor.Options{
				SourceFormat: sdktranslator.FormatOpenAIResponse, OriginalRequest: payload,
				Metadata: map[string]any{coreexecutor.PinnedAuthMetadataKey: auth.ID},
			},
		)
		if err != nil || capture.request.Model != "gemini-2.5-flash" {
			t.Fatalf("config alias consumer auth=%q model=%q error=%v", auth.ID, capture.request.Model, err)
		}
	}
}
