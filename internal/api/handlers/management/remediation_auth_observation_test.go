package management

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	coreauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
	"github.com/gin-gonic/gin"
)

func TestListAuthFilesPreservesPassiveQuotaObservations(t *testing.T) {
	manager := coreauth.NewManager(nil, nil, nil)
	observed := time.Now().UTC().Truncate(time.Second)
	auth := &coreauth.Auth{
		Attributes: map[string]string{
			coreauth.AttributePath: filepath.Join(
				t.TempDir(), "observed.json",
			),
		}, ID: "observed.json", FileName: "observed.json", Provider: "codex", Quota: coreauth.QuotaState{
			Exceeded: true, ObservedAt: observed, NextRecoverAt: observed.Add(time.Hour), Family: "family-a",
			Signals: map[string]string{"remaining": "0"},
		}, ModelStates: map[string]*coreauth.ModelState{
			"model-a": {
				Quota: coreauth.QuotaState{
					Exceeded: true, ObservedAt: observed, Family: "family-a",
				},
			},
			"model-b": {
				Quota: coreauth.QuotaState{ObservedAt: observed, Signals: map[string]string{"remaining": "12"}},
			},
		},
	}
	if _, err := manager.Register(context.Background(), auth); err != nil {
		t.Fatal(err)
	}
	h := &Handler{authManager: manager}
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodGet, "/auth-files", nil)
	h.ListAuthFiles(c)
	var payload struct {
		Files []struct {
			Quota  coreauth.QuotaState            `json:"quota"`
			Models map[string]coreauth.QuotaState `json:"model_quotas"`
			Recent []any                          `json:"recent_requests"`
		} `json:"files"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusOK || len(payload.Files) != 1 {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	item := payload.Files[0]
	if !item.Quota.ObservedAt.Equal(observed) || item.Quota.Signals["remaining"] != "0" ||
		!item.Models["model-a"].ObservedAt.Equal(observed) ||
		item.Models["model-b"].Signals["remaining"] != "12" ||
		len(item.Recent) != 24 {
		t.Fatalf("entry=%+v", item)
	}
	current, ok := manager.GetByID(auth.ID)
	if !ok || !current.Quota.Exceeded || current.Quota.Family != "family-a" ||
		!current.Quota.NextRecoverAt.Equal(observed.Add(time.Hour)) ||
		current.ModelStates["model-a"].Quota.Family != "family-a" {
		t.Fatalf("listing changed the independent cooldown state: %+v", current)
	}
}
func TestDiskAuthFilesPreservesProjectAndUnknownQuota(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(
		filepath.Join(dir, "gemini.json"), []byte((`{"type":"gemini","project_id":"project-a","websockets":` +
			`false}`)), 0600,
	); err != nil {
		t.Fatal(err)
	}
	h := &Handler{cfg: &config.Config{AuthDir: dir}}
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodGet, "/auth-files", nil)
	h.ListAuthFiles(c)
	var payload struct {
		Files []map[string]any `json:"files"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	if len(payload.Files) != 1 {
		t.Fatalf("body=%s", rec.Body.String())
	}
	item := payload.Files[0]
	if item["project_id"] != "project-a" || item["websockets"] != false {
		t.Fatalf("item=%v", item)
	}
	for _, key := range []string{"quota", "model_quotas"} {
		if value, ok := item[key]; !ok || value != nil {
			t.Fatalf("unknown %s=%v present=%v", key, value, ok)
		}
	}
}
func TestDeleteAuthFileRejectsExternalIdentityAndPreservesShadow(t *testing.T) {
	dir, externalDir := t.TempDir(), t.TempDir()
	shadow, external := filepath.Join(dir, "shared.json"), filepath.Join(externalDir, "shared.json")
	for _, path := range []string{shadow, external} {
		if err := os.WriteFile(path, []byte(`{"type":"codex"}`), 0600); err != nil {
			t.Fatal(err)
		}
	}
	manager := coreauth.NewManager(nil, nil, nil)
	if _, err := manager.Register(
		context.Background(), &coreauth.Auth{
			ID: "external", FileName: "shared.json", Provider: "codex",
			Attributes: map[string]string{coreauth.AttributePath: external},
		},
	); err != nil {
		t.Fatal(err)
	}
	h := &Handler{cfg: &config.Config{AuthDir: dir}, authManager: manager, tokenStore: &pluginOAuthFileStore{}}
	c, rec := newArtifactCacheTestContext(http.MethodDelete, "/auth-files?name=shared.json", "")
	h.DeleteAuthFile(c)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("external identity status=%d body=%s", rec.Code, rec.Body.String())
	}
	for _, path := range []string{shadow, external} {
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("file changed: %s %v", path, err)
		}
	}
	if _, ok := manager.GetByID("external"); !ok {
		t.Fatal("external identity removed")
	}
}

type pluginRollbackRaceStore struct{ *pluginOAuthFileStore }

func (s *pluginRollbackRaceStore) PersistAuthFiles(_ context.Context, _ string, paths ...string) error {
	for _, path := range paths {
		raw, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		s.mu.Lock()
		s.persisted[path] = bytes.Clone(raw)
		s.mu.Unlock()
	}
	return nil
}
func TestPluginOAuthRollbackPreservesConcurrentHTTPUpload(t *testing.T) {
	dir := t.TempDir()
	first := filepath.Join(dir, "first.json")
	old := []byte(`{"type":"codex","access_token":"synthetic-old","account":"old"}`)
	if err := os.WriteFile(first, old, 0600); err != nil {
		t.Fatal(err)
	}
	store := &pluginRollbackRaceStore{
		&pluginOAuthFileStore{persisted: map[string][]byte{first: bytes.Clone(old)}},
	}
	manager := coreauth.NewManager(store, nil, nil)
	previous, _, err := authFromFile(dir, first, old)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Register(coreauth.WithSkipPersist(context.Background()), previous); err != nil {
		t.Fatal(err)
	}
	h := NewHandler(&config.Config{AuthDir: dir}, "", manager, withTokenStore(store))
	entered, release := make(chan struct{}), make(chan struct{})
	h.SetPostAuthPersistHook(
		func(_ context.Context, a *coreauth.Auth) error {
			if a.FileName == "second.json" {
				close(entered)
				<-release
				return errors.New("injected second runtime failure")
			}
			return nil
		},
	)
	records := []*coreauth.Auth{
		{
			Provider: "codex", FileName: "first.json", Metadata: map[string]any{
				"type": "codex", "access_token": "synthetic-login", "account": "login",
			},
		}, {
			Provider: "codex", FileName: "second.json",
			Metadata: map[string]any{"type": "codex", "access_token": "synthetic-second"},
		},
	}
	done := make(chan error, 1)
	go func() { done <- h.saveClaimedPluginLoginRecords(context.Background(), dir, records) }()
	<-entered
	uploaded := []byte(`{"type":"codex","access_token":"synthetic-upload","account":"uploaded"}`)
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	part, err := writer.CreateFormFile("file", "first.json")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := part.Write(uploaded); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/auth-files", &body)
	c.Request.Header.Set("Content-Type", writer.FormDataContentType())
	h.UploadAuthFile(c)
	close(release)
	rollbackErr := <-done
	if rec.Code != http.StatusOK {
		t.Fatalf("upload status=%d body=%s", rec.Code, rec.Body.String())
	}
	if rollbackErr == nil {
		t.Fatal("expected second credential failure")
	}
	current, ok := manager.GetByID("first.json")
	if !ok || current.Metadata["account"] != "uploaded" {
		t.Fatalf("runtime=%+v", current)
	}
	raw, err := os.ReadFile(first)
	if err != nil || !bytes.Equal(raw, uploaded) {
		t.Fatalf("file=%s error=%v", raw, err)
	}
	store.mu.Lock()
	persisted := bytes.Clone(store.persisted[first])
	store.mu.Unlock()
	if !bytes.Equal(persisted, uploaded) {
		t.Fatalf("durable=%s", persisted)
	}
}
