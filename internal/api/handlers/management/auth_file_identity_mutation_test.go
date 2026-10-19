package management

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/quota"
	coreauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
	"github.com/gin-gonic/gin"
)

func registerSameFileDevinAuths(t *testing.T) (*coreauth.Manager, *memoryAuthStore, map[string]string) {
	t.Helper()
	store := &memoryAuthStore{}
	manager := coreauth.NewManager(store, nil, nil)
	sharedPath := filepath.Join(t.TempDir(), "devin-shared.json")
	indexes := make(map[string]string)
	for _, id := range []string{"devin-a", "devin-b"} {
		auth := &coreauth.Auth{
			ID:       id,
			FileName: "devin-shared.json",
			Provider: "devin",
			Status:   coreauth.StatusActive,
			Metadata: map[string]any{"type": "devin", "priority": 1},
			Attributes: map[string]string{
				coreauth.AttributePath:          sharedPath,
				coreauth.AttributeAuthIndexSeed: id,
				"priority":                      "1",
			},
		}
		indexes[id] = auth.EnsureIndex()
		if _, errRegister := manager.Register(context.Background(), auth); errRegister != nil {
			t.Fatal(errRegister)
		}
	}
	return manager, store, indexes
}

func TestAuthFromFileQuotaMaterialIdentity(t *testing.T) {
	path := filepath.Join(t.TempDir(), "codex-material.json")
	var previousRevision, previousDigest string
	for _, serial := range []string{"7", "9007199254740992", "9007199254740993"} {
		raw := []byte(`{"type":"codex","access_token":"synthetic","priority":7,"request-retry":1,"credential_serial":` + serial + `,"disabled":false}`)
		auth, _, err := authFromFile(filepath.Dir(path), path, raw)
		if err != nil {
			t.Fatal(err)
		}
		revision, equivalent, err := coreauth.QuotaCredentialRevisions(auth)
		if err != nil {
			t.Fatal(err)
		}
		digest := auth.Attributes[coreauth.PersistedSHA256Attribute]
		if slices.Contains(equivalent, digest) != (serial == "7") {
			t.Fatal("legacy quota restoration did not respect numeric precision")
		}
		if auth.Attributes["priority"] != "7" {
			t.Fatal("lossless metadata decoding changed the file priority")
		}
		if previousRevision != "" && (revision == previousRevision || slices.Contains(equivalent, previousDigest)) {
			t.Fatal("adjacent large integers share quota material or legacy candidates")
		}
		updated := auth.Clone()
		updated.Disabled = true
		updated.Metadata["disabled"] = true
		statusRevision, _, err := coreauth.QuotaCredentialRevisions(updated)
		if err != nil || statusRevision != revision {
			t.Fatalf("status update changed quota material: %v", err)
		}
		updated.Metadata["access_token"] = "synthetic-replacement"
		replacedRevision, replacedEquivalent, err := coreauth.QuotaCredentialRevisions(updated)
		if err != nil || replacedRevision == revision || slices.Contains(replacedEquivalent, digest) {
			t.Fatalf("material replacement retained the original file proof: %v", err)
		}
		previousRevision, previousDigest = revision, digest
	}
}

func TestPatchAuthFileStatusTargetsSameFileIdentity(t *testing.T) {
	manager, store, indexes := registerSameFileDevinAuths(t)
	handler := NewHandler(&config.Config{}, "", manager, withTokenStore(store))

	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(
		http.MethodPatch,
		"/auth-files/status",
		strings.NewReader(`{"name":"devin-shared.json","auth_index":"`+indexes["devin-a"]+`","disabled":true}`),
	)
	ctx.Request.Header.Set("Content-Type", "application/json")
	handler.PatchAuthFileStatus(ctx)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", recorder.Code, recorder.Body.String())
	}
	for id, wantDisabled := range map[string]bool{"devin-a": true, "devin-b": false} {
		auth, ok := manager.GetByID(id)
		if !ok || auth == nil || auth.Disabled != wantDisabled {
			t.Fatalf("auth %s = %#v, want disabled=%v", id, auth, wantDisabled)
		}
	}
}

func TestPatchAuthFileStatusPreservesQuotaAcrossWatcherDigests(t *testing.T) {
	manager, store, indexes := registerSameFileDevinAuths(t)
	handler := NewHandler(&config.Config{}, "", manager, withTokenStore(store))
	t.Cleanup(func() { _ = handler.Close() })
	observedAt := time.Now().UTC().Add(-time.Minute)
	scheduler := quota.NewSchedulerWithResult(quota.DefaultConfig(), func(_ context.Context, entry *quota.Entry) (quota.FetchResult, error) {
		return quota.FetchResult{
			Data:   []byte(`{"owner":"` + entry.AuthIndex + `","credits":{"balance":"7.5"},"reset_count":2,"expires_at":"2026-11-01T00:00:00Z"}`),
			Source: "synthetic", ObservedAt: observedAt, Completeness: quota.FetchComplete,
		}, nil
	})
	handler.SetQuotaScheduler(scheduler)
	for _, auth := range manager.List() {
		handler.syncQuotaSchedulerForAuth(auth)
	}
	scheduler.RefreshNow([]string{"devin-shared.json"})
	before := scheduler.GetStatus().Credentials
	for _, disabled := range []bool{true, false, true, false} {
		body, err := json.Marshal(map[string]any{"name": "devin-shared.json", "auth_index": indexes["devin-a"], "disabled": disabled})
		if err != nil {
			t.Fatal(err)
		}
		ctx, recorder := quotaTestContext(http.MethodPatch, "/auth-files/status", string(body))
		handler.PatchAuthFileStatus(ctx)
		if recorder.Code != http.StatusOK {
			t.Fatalf("status update failed: %d %s", recorder.Code, recorder.Body.String())
		}
		published, _ := manager.GetByID("devin-a")
		published.Metadata["disabled"] = disabled
		raw, err := json.Marshal(published.Metadata)
		if err != nil {
			t.Fatal(err)
		}
		digest, err := coreauth.DigestPersistedJSON(raw)
		if err != nil {
			t.Fatal(err)
		}
		published.Attributes[coreauth.PersistedSHA256Attribute] = hex.EncodeToString(digest[:])
		published, err = manager.Update(coreauth.WithSkipPersist(context.Background()), published)
		if err != nil {
			t.Fatal(err)
		}
		handler.syncQuotaSchedulerForAuth(published)
		after := scheduler.GetStatus().Credentials
		for id, index := range indexes {
			key := quota.BindingEntryKey(quota.TypeDevin, "devin-shared.json", index)
			previous, current := before[key], after[key]
			if previous == nil || current == nil || len(previous.Data) == 0 || string(current.Data) != string(previous.Data) ||
				current.Generation != previous.Generation || current.Freshness != previous.Freshness ||
				current.ObservedAt == nil || !current.ObservedAt.Equal(observedAt) ||
				current.LastSuccessAt == nil || !current.LastSuccessAt.Equal(*previous.LastSuccessAt) ||
				current.Disabled != (id == "devin-a" && disabled) {
				t.Fatalf("disabled=%v discarded quota for %s: before=%#v after=%#v", disabled, id, previous, current)
			}
			if id == "devin-b" && current.StateRevision != previous.StateRevision {
				t.Fatal("status update changed the same-file sibling")
			}
		}
	}
	replaced, _ := manager.GetByID("devin-a")
	replaced.Metadata["access_token"] = "synthetic-replacement"
	replaced, err := manager.Update(coreauth.WithSkipPersist(context.Background()), replaced)
	if err != nil {
		t.Fatal(err)
	}
	handler.syncQuotaSchedulerForAuth(replaced)
	after := scheduler.GetStatus().Credentials
	keyA := quota.BindingEntryKey(quota.TypeDevin, "devin-shared.json", indexes["devin-a"])
	keyB := quota.BindingEntryKey(quota.TypeDevin, "devin-shared.json", indexes["devin-b"])
	if after[keyA] == nil || len(after[keyA].Data) != 0 || after[keyA].Generation <= before[keyA].Generation ||
		after[keyB] == nil || string(after[keyB].Data) != string(before[keyB].Data) {
		t.Fatalf("credential replacement mixed same-file quota: %#v", after)
	}
}

func TestPatchAuthFileFieldsTargetsSameFileIdentity(t *testing.T) {
	manager, store, indexes := registerSameFileDevinAuths(t)
	handler := NewHandler(&config.Config{}, "", manager, withTokenStore(store))

	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(
		http.MethodPatch,
		"/auth-files/fields",
		strings.NewReader(`{"name":"devin-shared.json","auth-index":"`+indexes["devin-b"]+`","priority":7}`),
	)
	ctx.Request.Header.Set("Content-Type", "application/json")
	handler.PatchAuthFileFields(ctx)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", recorder.Code, recorder.Body.String())
	}
	for id, wantPriority := range map[string]string{"devin-a": "1", "devin-b": "7"} {
		auth, ok := manager.GetByID(id)
		if !ok || auth == nil || auth.Attributes["priority"] != wantPriority {
			t.Fatalf("auth %s = %#v, want priority=%s", id, auth, wantPriority)
		}
	}
}

func TestPatchAuthFileFieldsRejectsMismatchedSameFileIdentity(t *testing.T) {
	manager, store, indexes := registerSameFileDevinAuths(t)
	handler := NewHandler(&config.Config{}, "", manager, withTokenStore(store))

	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(
		http.MethodPatch,
		"/auth-files/fields",
		strings.NewReader(`{"name":"other.json","auth-index":"`+indexes["devin-a"]+`","priority":7}`),
	)
	ctx.Request.Header.Set("Content-Type", "application/json")
	handler.PatchAuthFileFields(ctx)

	if recorder.Code != http.StatusConflict {
		t.Fatalf("status = %d, want %d body=%s", recorder.Code, http.StatusConflict, recorder.Body.String())
	}
}
