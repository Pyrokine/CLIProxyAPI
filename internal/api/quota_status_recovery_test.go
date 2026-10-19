package api

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/quota"
	sdkAuth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/auth"
	coreauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
)

func TestRegisterAuthFilesForQuotaPreservesStatusAndRejectsReplacement(t *testing.T) {
	const name = "codex-fixture.json"
	const payload = `{"credits":{"balance":"7.5"},"reset_count":2}`
	dir := t.TempDir()
	path := filepath.Join(dir, name)
	raw := []byte("{\n  \"type\":\"codex\",\"access_token\":\"synthetic-before\",\"disabled\":false\n}\n")
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(raw)
	key := quota.EntryKey(quota.TypeCodex, name)
	observedAt := time.Now().UTC().Add(-time.Minute)
	scheduler := quota.NewScheduler(quota.DefaultConfig(), nil)
	scheduler.Restore(map[string]quota.EntryState{key: {
		FileName: name, Type: quota.TypeCodex, AuthIndex: computeAuthIndex(name),
		CredentialRevision: hex.EncodeToString(digest[:]), Generation: 5, StateRevision: 4,
		Status: quota.StatusSuccess, Freshness: quota.FreshnessFresh, Data: []byte(payload),
		ObservedAt: &observedAt, LastSuccessAt: &observedAt,
	}})
	registerAuthFilesForQuota(scheduler, dir)
	before := scheduler.GetStatus().Credentials[key]
	if before == nil || string(before.Data) != payload || before.Generation != 5 {
		t.Fatalf("raw file revision was not migrated: %#v", before)
	}
	for _, disabled := range []bool{true, false} {
		data, err := json.Marshal(map[string]any{"type": "codex", "access_token": "synthetic-before", "disabled": disabled})
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, data, 0o600); err != nil {
			t.Fatal(err)
		}
		registerAuthFilesForQuota(scheduler, dir)
		after := scheduler.GetStatus().Credentials[key]
		if after == nil || after.Disabled != disabled || after.Generation != 5 || string(after.Data) != payload ||
			after.ObservedAt == nil || !after.ObservedAt.Equal(observedAt) {
			t.Fatalf("file status discarded quota: %#v", after)
		}
	}
	if err := os.WriteFile(path, []byte(`{"type":"codex","access_token":"synthetic-after"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	registerAuthFilesForQuota(scheduler, dir)
	after := scheduler.GetStatus().Credentials[key]
	if after == nil || len(after.Data) != 0 || after.Generation <= before.Generation || after.ObservedAt != nil {
		t.Fatalf("file replacement reused prior quota: %#v", after)
	}
	for _, invalid := range []string{"null", "[]", "invalid"} {
		if err := os.WriteFile(path, []byte(invalid), 0o600); err != nil {
			t.Fatal(err)
		}
		registerAuthFilesForQuota(scheduler, dir)
		if entry := scheduler.GetStatus().Credentials[key]; entry != nil {
			t.Fatalf("invalid file registered a quota binding: %#v", entry)
		}
	}
}

func TestServerRestoresFileQuotaMaterialIdentity(t *testing.T) {
	for _, tc := range []struct {
		name   string
		serial string
		legacy bool
	}{
		{"normalized legacy source", "7", true},
		{"precise current source", "9007199254740993", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			first := newTestServer(t)
			dir := t.TempDir()
			path := filepath.Join(dir, "codex-material.json")
			raw := []byte(`{"type":"codex","access_token":"synthetic","request-retry":1,"credential_serial":` + tc.serial + `}`)
			if err := os.WriteFile(path, raw, 0o600); err != nil {
				t.Fatal(err)
			}
			store := sdkAuth.NewFileTokenStore()
			store.SetBaseDir(dir)
			ctx := sdkAuth.WithPluginAuthParser(context.Background(), nil)
			load := func() (*coreauth.Manager, *coreauth.Auth) {
				t.Helper()
				auths, err := store.List(ctx)
				if err != nil || len(auths) != 1 {
					t.Fatalf("load source file: count=%d err=%v", len(auths), err)
				}
				manager := coreauth.NewManager(nil, nil, nil)
				auth, err := manager.Register(coreauth.WithSkipPersist(ctx), auths[0])
				if err != nil {
					t.Fatal(err)
				}
				return manager, auth
			}
			_, auth := load()
			observedAt := time.Now().UTC().Add(-time.Minute)
			const payload = `{"rate_limit":{"primary_window":{"used_percent":12}},"credits":{"balance":"7.5"}}`
			scheduler := quota.NewSchedulerWithResult(quota.DefaultConfig(), func(context.Context, *quota.Entry) (quota.FetchResult, error) {
				return quota.FetchResult{Data: []byte(payload), Source: "synthetic", ObservedAt: observedAt, Completeness: quota.FetchComplete}, nil
			})
			first.mgmt.SetQuotaScheduler(scheduler)
			version := auth.Attributes[coreauth.PersistedSHA256Attribute]
			if !tc.legacy {
				var err error
				version, _, err = coreauth.QuotaCredentialRevisions(auth)
				if err != nil {
					t.Fatal(err)
				}
			}
			scheduler.RegisterBinding(auth.FileName, quota.TypeCodex, auth.EnsureIndex(), version)
			scheduler.RefreshNow([]string{auth.FileName})
			key := quota.EntryKey(quota.TypeCodex, auth.FileName)
			before := scheduler.GetStatus().Credentials[key]
			if before == nil || len(before.Data) == 0 {
				t.Fatal("source snapshot was not populated")
			}
			if err := first.mgmt.Close(); err != nil {
				t.Fatal(err)
			}
			cfg := *first.configSnapshot()
			manager, auth := load()
			second := NewServer(&cfg, manager, nil, first.configFilePath)
			t.Cleanup(func() { _ = second.mgmt.Close() })
			after := second.quotaScheduler.GetStatus().Credentials[key]
			if after == nil || string(after.Data) != payload || after.Generation != before.Generation || after.ObservedAt == nil || !after.ObservedAt.Equal(observedAt) {
				t.Fatalf("file-backed source proof was not restored: %#v", after)
			}
			if err := second.mgmt.Close(); err != nil {
				t.Fatal(err)
			}
			auth.Disabled = true
			if _, err := store.Save(ctx, auth); err != nil {
				t.Fatal(err)
			}
			manager, _ = load()
			third := NewServer(&cfg, manager, nil, first.configFilePath)
			t.Cleanup(func() { _ = third.mgmt.Close() })
			restored := third.quotaScheduler.GetStatus().Credentials[key]
			if restored == nil || !restored.Disabled || string(restored.Data) != payload || restored.Generation != before.Generation {
				t.Fatalf("status persistence discarded exact file material: %#v", restored)
			}
		})
	}
}

func TestServerRestoresStatusIndependentQuotaIdentity(t *testing.T) {
	const payload = `{"rate_limit":{"primary_window":{"used_percent":12}},"credits":{"balance":"7.5"},"rate_limit_reset_credits":{"available_count":2,"applicable_available_count":2},"rate_limit_reset_credits_details":{"credits":[{"id":"synthetic","expires_at":"2026-11-01T00:00:00Z"}]}}`
	for _, tc := range []struct {
		name       string
		change     string
		wantRetain bool
	}{
		{"status changed", "status", true},
		{"material replaced", "material", false},
		{"unknown legacy revision", "unknown", false},
		{"missing legacy revision", "missing", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			first := newTestServer(t)
			auth := &coreauth.Auth{
				ID: "legacy.json", FileName: "legacy.json", Provider: "codex",
				Metadata: map[string]any{"type": "codex", "access_token": "synthetic-before", "disabled": false},
			}
			auth.EnsureIndex()
			raw, err := json.Marshal(auth.Metadata)
			if err != nil {
				t.Fatal(err)
			}
			digest, err := coreauth.DigestPersistedJSON(raw)
			if err != nil {
				t.Fatal(err)
			}
			legacyRevision := hex.EncodeToString(digest[:])
			if tc.change == "unknown" {
				legacyRevision = "unproven-legacy-material"
			}
			if tc.change == "missing" {
				legacyRevision = ""
			}
			observedAt := time.Now().UTC().Add(-time.Minute)
			legacy := quota.NewSchedulerWithResult(quota.DefaultConfig(), func(context.Context, *quota.Entry) (quota.FetchResult, error) {
				return quota.FetchResult{Data: []byte(payload), Source: "codex_usage", ObservedAt: observedAt, Completeness: quota.FetchComplete}, nil
			})
			first.mgmt.SetQuotaScheduler(legacy)
			legacy.RegisterBinding(auth.FileName, quota.TypeCodex, auth.Index, legacyRevision)
			legacy.RefreshNow([]string{auth.FileName})
			key := quota.EntryKey(quota.TypeCodex, auth.FileName)
			before := legacy.GetStatus().Credentials[key]
			if before == nil || before.LastSuccessAt == nil || string(before.Data) != payload {
				t.Fatalf("legacy fixture was not persisted: %#v", before)
			}
			if err := first.mgmt.Close(); err != nil {
				t.Fatal(err)
			}

			auth.Disabled = true
			auth.Metadata["disabled"] = true
			if tc.change == "material" {
				auth.Metadata["access_token"] = "synthetic-after"
			}
			manager := coreauth.NewManager(nil, nil, nil)
			if _, err := manager.Register(context.Background(), auth); err != nil {
				t.Fatal(err)
			}
			cfg := *first.configSnapshot()
			second := NewServer(&cfg, manager, nil, first.configFilePath)
			t.Cleanup(func() { _ = second.mgmt.Close() })
			after := second.quotaScheduler.GetStatus().Credentials[key]
			if after == nil || !after.Disabled {
				t.Fatalf("current disabled state was not restored: %#v", after)
			}
			if tc.wantRetain {
				if string(after.Data) != payload || after.Generation != before.Generation || after.Freshness != before.Freshness ||
					after.ObservedAt == nil || !after.ObservedAt.Equal(observedAt) ||
					after.LastSuccessAt == nil || !after.LastSuccessAt.Equal(*before.LastSuccessAt) {
					t.Fatalf("status-only restart discarded quota: before=%#v after=%#v", before, after)
				}
				if err := second.mgmt.Close(); err != nil {
					t.Fatal(err)
				}
				third := NewServer(&cfg, manager, nil, first.configFilePath)
				t.Cleanup(func() { _ = third.mgmt.Close() })
				restored := third.quotaScheduler.GetStatus().Credentials[key]
				if restored == nil || string(restored.Data) != payload || restored.Generation != after.Generation ||
					restored.ObservedAt == nil || !restored.ObservedAt.Equal(observedAt) {
					t.Fatalf("strict revision restart discarded migrated quota: %#v", restored)
				}
			} else if len(after.Data) != 0 || after.Generation <= before.Generation || after.LastSuccessAt != nil ||
				after.ObservedAt != nil || after.Freshness != quota.FreshnessOutdated {
				t.Fatalf("unproven material restored legacy quota: %#v", after)
			}
		})
	}
}
