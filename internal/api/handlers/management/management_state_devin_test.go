package management

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/quota"
)

func TestManagementStatePersistsAuthScopedDevinQuotaEntries(t *testing.T) {
	t.Setenv("WRITABLE_PATH", "")
	t.Setenv("writable_path", "")
	configPath := filepath.Join(t.TempDir(), "config.yaml")
	store, err := openManagementStateStore(configPath)
	if err != nil {
		t.Fatalf("open management state: %v", err)
	}

	entries := []quota.EntryState{
		{
			FileName: "devin-shared.json", Type: quota.TypeDevin, AuthIndex: "index-a",
			CredentialRevision: "revision-a", Generation: 1, StateRevision: 2,
			Status: quota.StatusSuccess, Freshness: quota.FreshnessFresh,
			Data: []byte(`{"daily_quota_present":true,"daily_quota_remaining_percent":20}`),
		},
		{
			FileName: "devin-shared.json", Type: quota.TypeDevin, AuthIndex: "index-b",
			CredentialRevision: "revision-b", Generation: 1, StateRevision: 3,
			Status: quota.StatusSuccess, Freshness: quota.FreshnessFresh,
			Data: []byte(`{"daily_quota_present":true,"daily_quota_remaining_percent":80}`),
		},
	}
	for _, entry := range entries {
		if err = store.saveQuotaEntry(context.Background(), entry); err != nil {
			t.Fatalf("save %s: %v", entry.AuthIndex, err)
		}
	}
	if err = store.Close(); err != nil {
		t.Fatalf("close management state: %v", err)
	}

	store, err = openManagementStateStore(configPath)
	if err != nil {
		t.Fatalf("reopen management state: %v", err)
	}
	defer func() { _ = store.Close() }()
	loaded, err := store.loadQuotaEntries(context.Background())
	if err != nil {
		t.Fatalf("load quota entries: %v", err)
	}
	keyA := quota.BindingEntryKey(quota.TypeDevin, "devin-shared.json", "index-a")
	keyB := quota.BindingEntryKey(quota.TypeDevin, "devin-shared.json", "index-b")
	if len(loaded) != 2 || loaded[keyA].AuthIndex != "index-a" || loaded[keyB].AuthIndex != "index-b" {
		t.Fatalf("persisted Devin entries = %#v", loaded)
	}

	tombstone := loaded[keyA]
	tombstone.Generation++
	tombstone.StateRevision = 1
	tombstone.Tombstone = true
	if err = store.deleteQuotaEntry(context.Background(), tombstone); err != nil {
		t.Fatalf("save identity tombstone: %v", err)
	}
	loaded, err = store.loadQuotaEntries(context.Background())
	if err != nil {
		t.Fatalf("reload quota entries: %v", err)
	}
	if !loaded[keyA].Tombstone || loaded[keyB].Tombstone {
		t.Fatalf("identity tombstones = a:%#v b:%#v", loaded[keyA], loaded[keyB])
	}
}
