package auth

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestAuditRefreshContentRebaseValidatesCurrentStoredVersion(t *testing.T) {
	for _, externalChange := range []bool{false, true} {
		name := "managed-note"
		if externalChange {
			name = "external-after-managed-note"
		}
		t.Run(
			name, func(t *testing.T) {
				path := filepath.Join(t.TempDir(), "synthetic.json")
				store := &retryingRefreshStore{path: path}
				manager := NewManager(store, nil, nil)
				manager.SetHomeClientProvider(func() HomeClient { return nil })
				executor := &auditContentRefreshExecutor{}
				manager.RegisterExecutor(executor)
				registered, err := manager.Register(
					t.Context(), &Auth{
						ID: "stored-merge", Provider: "test", Attributes: map[string]string{"path": path},
						Metadata: map[string]any{
							"access_token": "synthetic-old", "refresh_token": "synthetic-owner", "note": "original",
						},
					},
				)
				if err != nil {
					t.Fatal(err)
				}
				executor.mutate = func(ctx context.Context, _ *Auth) error {
					current, _ := manager.GetByID(registered.ID)
					current.Metadata["note"] = "concurrent-note"
					if _, err := manager.Update(ctx, current); err != nil {
						return err
					}
					if externalChange {
						return os.WriteFile(
							path,
							[]byte(
								`{"access_token":"external-token","refresh_token":"external-owner","disabled":false}`,
							),
							0o600,
						)
					}
					return nil
				}
				_, err = manager.ForceRefreshAuth(t.Context(), registered.ID)
				if externalChange {
					if !errors.Is(err, ErrAuthDurableStateUncertain) {
						t.Fatalf("external conflict returned %v", err)
					}
				} else if err != nil {
					t.Fatal(err)
				}
				current, _ := manager.GetByID(registered.ID)
				if current.Metadata["access_token"] != "synthetic-refreshed" ||
					current.Metadata["note"] != "concurrent-note" {
					t.Fatalf("valid exchange/user edit lost: %#v", current.Metadata)
				}
				raw, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				var saved map[string]any
				if err := json.Unmarshal(raw, &saved); err != nil {
					t.Fatal(err)
				}
				want := "synthetic-refreshed"
				if externalChange {
					want = "external-token"
				}
				if saved["access_token"] != want || (!externalChange && saved["note"] != "concurrent-note") {
					t.Fatalf("unexpected persisted content: %#v", saved)
				}
			},
		)
	}
}

func TestAuditRefreshContentRejectsReplacedRegistrationAndCredential(t *testing.T) {
	for _, kind := range []string{"access-token", "reregister", "remove-recreate"} {
		t.Run(
			kind, func(t *testing.T) {
				manager := NewManager(nil, nil, nil)
				manager.SetHomeClientProvider(func() HomeClient { return nil })
				executor := &auditContentRefreshExecutor{}
				manager.RegisterExecutor(executor)
				base, err := manager.Register(
					t.Context(), &Auth{
						ID: "source-guard", Provider: "test",
						Metadata: map[string]any{"access_token": "synthetic-old", "refresh_token": "synthetic-owner"},
					},
				)
				if err != nil {
					t.Fatal(err)
				}
				executor.mutate = func(ctx context.Context, _ *Auth) error {
					current, _ := manager.GetByID(base.ID)
					switch kind {
					case "access-token":
						current.Metadata["access_token"] = "synthetic-replacement"
						_, err := manager.Update(ctx, current)
						return err
					case "reregister":
						_, err := manager.Register(ctx, current)
						return err
					default:
						if err := manager.Remove(ctx, current.ID); err != nil {
							return err
						}
						_, err := manager.Register(WithAllowAuthRecreate(ctx), current)
						return err
					}
				}
				if _, err := manager.ForceRefreshAuth(t.Context(), base.ID); err == nil {
					t.Fatal("old refresh overwrote replacement authority")
				}
				current, _ := manager.GetByID(base.ID)
				if current.Metadata["access_token"] == "synthetic-refreshed" {
					t.Fatal("replaced authority accepted the old refresh result")
				}
			},
		)
	}
}
