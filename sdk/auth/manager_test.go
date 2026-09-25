package auth

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	claudeauth "github.com/Pyrokine/CLIProxyAPI/v6/internal/auth/claude"
	devinauth "github.com/Pyrokine/CLIProxyAPI/v6/internal/auth/devin"
	coreauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/config"
)

type cancellingManagerAuthenticator struct {
	cancel context.CancelFunc
}

func (a *cancellingManagerAuthenticator) Provider() string {
	return "cancel-test"
}

func (a *cancellingManagerAuthenticator) Login(
	context.Context,
	*config.Config,
	*LoginOptions,
) (*coreauth.Auth, error) {
	if a.cancel != nil {
		a.cancel()
	}
	return &coreauth.Auth{ID: "cancel-test.json", Provider: a.Provider()}, nil
}

func (*cancellingManagerAuthenticator) RefreshLead() *time.Duration {
	return nil
}

type recordingManagerStore struct {
	saves int
}

func (*recordingManagerStore) List(context.Context) ([]*coreauth.Auth, error) {
	return nil, nil
}

func (s *recordingManagerStore) Save(context.Context, *coreauth.Auth) (string, error) {
	s.saves++
	return "saved.json", nil
}

func (*recordingManagerStore) Delete(context.Context, string) error {
	return nil
}

type cancellingBaseDirManagerStore struct {
	recordingManagerStore
	cancel context.CancelFunc
}

func (s *cancellingBaseDirManagerStore) SetBaseDir(_ string) {
	s.cancel()
}

func TestManagerLoginDoesNotSaveAfterCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	store := &recordingManagerStore{}
	manager := NewManager(store, &cancellingManagerAuthenticator{cancel: cancel})

	record, path, err := manager.Login(ctx, "cancel-test", &config.Config{}, nil)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Login error = %v, want context.Canceled", err)
	}
	if record == nil || record.ID != "cancel-test.json" {
		t.Fatalf("Login record = %#v", record)
	}
	if path != "" {
		t.Fatalf("Login path = %q, want empty", path)
	}
	if store.saves != 0 {
		t.Fatalf("store saves = %d, want 0", store.saves)
	}
}

func TestManagerLoginDoesNotMutateSharedStoreBaseDir(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	store := &cancellingBaseDirManagerStore{cancel: cancel}
	manager := NewManager(store, &cancellingManagerAuthenticator{})

	record, path, err := manager.Login(
		ctx,
		"cancel-test",
		&config.Config{AuthDir: t.TempDir()},
		nil,
	)
	if err != nil {
		t.Fatalf("Login error = %v", err)
	}
	if record == nil || record.ID != "cancel-test.json" {
		t.Fatalf("Login record = %#v", record)
	}
	if path != "saved.json" {
		t.Fatalf("Login path = %q, want saved.json", path)
	}
	if store.saves != 1 {
		t.Fatalf("store saves = %d, want 1", store.saves)
	}
	if ctx.Err() != nil {
		t.Fatalf("shared base-dir setter was called: %v", ctx.Err())
	}
}

type fixedManagerAuthenticator struct {
	provider string
	id       string
}

func (a fixedManagerAuthenticator) Provider() string { return a.provider }

func (a fixedManagerAuthenticator) Login(
	context.Context,
	*config.Config,
	*LoginOptions,
) (*coreauth.Auth, error) {
	return &coreauth.Auth{ID: a.id, Provider: a.provider}, nil
}

func (fixedManagerAuthenticator) RefreshLead() *time.Duration { return nil }

type scopedManagerStore struct {
	mu           sync.Mutex
	dirs         map[string]string
	unscopedSave int
}

func (*scopedManagerStore) List(context.Context) ([]*coreauth.Auth, error) { return nil, nil }
func (*scopedManagerStore) Delete(context.Context, string) error           { return nil }
func (s *scopedManagerStore) Save(context.Context, *coreauth.Auth) (string, error) {
	s.mu.Lock()
	s.unscopedSave++
	s.mu.Unlock()
	return "", errors.New("unscoped save called")
}
func (s *scopedManagerStore) SaveWithBaseDir(
	_ context.Context,
	dir string,
	auth *coreauth.Auth,
) (string, error) {
	s.mu.Lock()
	if s.dirs == nil {
		s.dirs = make(map[string]string)
	}
	s.dirs[auth.ID] = dir
	s.mu.Unlock()
	return filepath.Join(dir, auth.ID), nil
}

func TestManagerConcurrentLoginUsesOperationScopedAuthDirs(t *testing.T) {
	store := &scopedManagerStore{}
	firstDir := t.TempDir()
	secondDir := t.TempDir()
	first := NewManager(store, fixedManagerAuthenticator{provider: "first", id: "first.json"})
	second := NewManager(store, fixedManagerAuthenticator{provider: "second", id: "second.json"})

	var wait sync.WaitGroup
	wait.Add(2)
	errorsByLogin := make(chan error, 2)
	go func() {
		defer wait.Done()
		_, path, err := first.Login(
			context.Background(), "first", &config.Config{AuthDir: firstDir}, nil,
		)
		if err == nil && path != filepath.Join(firstDir, "first.json") {
			err = errors.New("first login used the wrong auth directory")
		}
		errorsByLogin <- err
	}()
	go func() {
		defer wait.Done()
		_, path, err := second.Login(
			context.Background(), "second", &config.Config{AuthDir: secondDir}, nil,
		)
		if err == nil && path != filepath.Join(secondDir, "second.json") {
			err = errors.New("second login used the wrong auth directory")
		}
		errorsByLogin <- err
	}()
	wait.Wait()
	close(errorsByLogin)
	for err := range errorsByLogin {
		if err != nil {
			t.Fatal(err)
		}
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	if store.unscopedSave != 0 || store.dirs["first.json"] != firstDir || store.dirs["second.json"] != secondDir {
		t.Fatalf("scoped saves = %#v, unscoped = %d", store.dirs, store.unscopedSave)
	}
}

type recordManagerAuthenticator struct {
	provider string
	record   *coreauth.Auth
}

func (a recordManagerAuthenticator) Provider() string { return a.provider }
func (a recordManagerAuthenticator) Login(
	context.Context,
	*config.Config,
	*LoginOptions,
) (*coreauth.Auth, error) {
	return a.record, nil
}
func (recordManagerAuthenticator) RefreshLead() *time.Duration { return nil }

type failingTokenStorage struct {
	err error
}

func (s failingTokenStorage) SaveTokenToFile(string) error { return s.err }

type emptyPathManagerStore struct {
	creationIntent bool
	deletes        int
}

func (*emptyPathManagerStore) List(context.Context) ([]*coreauth.Auth, error) { return nil, nil }
func (s *emptyPathManagerStore) Save(ctx context.Context, _ *coreauth.Auth) (string, error) {
	s.creationIntent = coreauth.HasAuthCreationIntent(ctx)
	return "", nil
}
func (s *emptyPathManagerStore) Delete(context.Context, string) error {
	s.deletes++
	return nil
}

func TestManagerLoginPreservesExistingCredentialMetadata(t *testing.T) {
	authDir := t.TempDir()
	fileName := "demo.json"
	writeManagerCredential(t, filepath.Join(authDir, fileName), map[string]any{
		"type": "demo", "access_token": "old-token", "disabled": true,
		"weight": 4, "proxy_url": "http://proxy.example", "prefix": "team",
		"headers": map[string]any{"X-Team": "blue"}, "disable_cooling": false,
		"request_retry": 0, "fingerprint_profile": "claude-code-cli", "note": "keep",
	})
	record := &coreauth.Auth{
		ID: fileName, FileName: fileName, Provider: "demo",
		Metadata: map[string]any{"type": "demo", "access_token": "new-token"},
	}
	store := NewFileTokenStore()
	manager := NewManager(store, recordManagerAuthenticator{provider: "demo", record: record})

	_, savedPath, errLogin := manager.Login(
		context.Background(), "demo", &config.Config{AuthDir: authDir}, nil,
	)
	if errLogin != nil {
		t.Fatal(errLogin)
	}
	if savedPath != filepath.Join(authDir, fileName) {
		t.Fatalf("saved path = %q", savedPath)
	}
	saved := readManagerCredential(t, savedPath)
	if saved["access_token"] != "new-token" || saved["note"] != "keep" || saved["disabled"] != true {
		t.Fatalf("saved metadata = %#v", saved)
	}
	if record.ProxyURL != "http://proxy.example" || record.Prefix != "team" ||
		record.Attributes[coreauth.AttributeWeight] != "4" || record.Attributes["header:X-Team"] != "blue" {
		t.Fatalf("runtime metadata projection = %#v", record)
	}
}

func TestManagerLoginMigratesClaudeOnlyAfterCanonicalSave(t *testing.T) {
	authDir := t.TempDir()
	const email = "user@example.com"
	legacyFileName := claudeauth.CredentialFileName(email, "", "")
	legacyPath := filepath.Join(authDir, legacyFileName)
	writeManagerCredential(t, legacyPath, map[string]any{
		"type": "claude", "email": email, "organization_uuid": "organization-a",
		"account_uuid": "account-a", "access_token": "old-token", "disabled": true,
		"prefix": "team", "weight": 5,
	})
	canonicalFileName := claudeauth.CredentialFileName(email, "organization-a", "account-a")
	record := &coreauth.Auth{
		ID: canonicalFileName, FileName: canonicalFileName, Provider: "claude",
		Metadata: map[string]any{
			"type": "claude", "email": email, "organization_uuid": "organization-a",
			"account_uuid": "account-a", "access_token": "new-token",
		},
	}
	store := NewFileTokenStore()
	manager := NewManager(store, recordManagerAuthenticator{provider: "claude", record: record})

	_, savedPath, errLogin := manager.Login(
		context.Background(), "claude", &config.Config{AuthDir: authDir}, nil,
	)
	if errLogin != nil {
		t.Fatal(errLogin)
	}
	if _, errStat := os.Stat(legacyPath); !errors.Is(errStat, os.ErrNotExist) {
		t.Fatalf("legacy credential still exists: %v", errStat)
	}
	saved := readManagerCredential(t, savedPath)
	if saved["access_token"] != "new-token" || saved["prefix"] != "team" || saved["disabled"] != true {
		t.Fatalf("migrated metadata = %#v", saved)
	}
}

func TestManagerLoginKeepsClaudeLegacyWhenCanonicalSaveFails(t *testing.T) {
	authDir := t.TempDir()
	const email = "user@example.com"
	legacyPath := filepath.Join(authDir, claudeauth.CredentialFileName(email, "", ""))
	writeManagerCredential(t, legacyPath, map[string]any{
		"type": "claude", "email": email, "organization_uuid": "organization-a", "account_uuid": "account-a",
	})
	canonicalFileName := claudeauth.CredentialFileName(email, "organization-a", "account-a")
	saveErr := errors.New("save failed")
	record := &coreauth.Auth{
		ID: canonicalFileName, FileName: canonicalFileName, Provider: "claude",
		Storage: failingTokenStorage{err: saveErr},
		Metadata: map[string]any{
			"type": "claude", "email": email, "organization_uuid": "organization-a", "account_uuid": "account-a",
		},
	}
	manager := NewManager(NewFileTokenStore(), recordManagerAuthenticator{provider: "claude", record: record})
	if _, _, errLogin := manager.Login(
		context.Background(), "claude", &config.Config{AuthDir: authDir}, nil,
	); !errors.Is(errLogin, saveErr) {
		t.Fatalf("Login error = %v, want save failure", errLogin)
	}
	if _, errStat := os.Stat(legacyPath); errStat != nil {
		t.Fatalf("legacy credential removed after failed save: %v", errStat)
	}
}

func TestManagerLoginKeepsClaudeLegacyWhenCanonicalSaveReturnsEmptyPath(t *testing.T) {
	authDir := t.TempDir()
	const email = "user@example.com"
	legacyPath := filepath.Join(authDir, claudeauth.CredentialFileName(email, "", ""))
	writeManagerCredential(t, legacyPath, map[string]any{
		"type": "claude", "email": email, "organization_uuid": "organization-a", "account_uuid": "account-a",
	})
	canonicalFileName := claudeauth.CredentialFileName(email, "organization-a", "account-a")
	record := &coreauth.Auth{
		ID: canonicalFileName, FileName: canonicalFileName, Provider: "claude",
		Metadata: map[string]any{
			"type": "claude", "email": email, "organization_uuid": "organization-a", "account_uuid": "account-a",
		},
	}
	store := &emptyPathManagerStore{}
	manager := NewManager(store, recordManagerAuthenticator{provider: "claude", record: record})
	if _, savedPath, errLogin := manager.Login(
		context.Background(), "claude", &config.Config{AuthDir: authDir}, nil,
	); errLogin == nil || savedPath != "" {
		t.Fatalf("Login path/error = %q/%v, want empty path and error", savedPath, errLogin)
	}
	if !store.creationIntent {
		t.Fatal("canonical save did not carry explicit creation intent")
	}
	if store.deletes != 0 {
		t.Fatalf("legacy deletes = %d, want 0", store.deletes)
	}
	if _, errStat := os.Stat(legacyPath); errStat != nil {
		t.Fatalf("legacy credential removed after empty save path: %v", errStat)
	}
}

func TestManagerLoginDoesNotMergeClaudeFromDifferentOrganization(t *testing.T) {
	authDir := t.TempDir()
	const email = "user@example.com"
	legacyPath := filepath.Join(authDir, claudeauth.CredentialFileName(email, "", ""))
	writeManagerCredential(t, legacyPath, map[string]any{
		"type": "claude", "email": email, "organization_uuid": "organization-a",
		"account_uuid": "shared-account", "prefix": "wrong-team",
	})
	canonicalFileName := claudeauth.CredentialFileName(email, "organization-b", "shared-account")
	record := &coreauth.Auth{
		ID: canonicalFileName, FileName: canonicalFileName, Provider: "claude",
		Metadata: map[string]any{
			"type": "claude", "email": email, "organization_uuid": "organization-b",
			"account_uuid": "shared-account", "access_token": "new-token",
		},
	}
	manager := NewManager(NewFileTokenStore(), recordManagerAuthenticator{provider: "claude", record: record})
	_, savedPath, errLogin := manager.Login(
		context.Background(), "claude", &config.Config{AuthDir: authDir}, nil,
	)
	if errLogin != nil {
		t.Fatal(errLogin)
	}
	if _, errStat := os.Stat(legacyPath); errStat != nil {
		t.Fatalf("different-organization legacy credential was removed: %v", errStat)
	}
	if _, inherited := readManagerCredential(t, savedPath)["prefix"]; inherited {
		t.Fatal("credential inherited metadata from a different organization")
	}
}

func TestManagerLoginKeepsClaudeOrganizationsAsSeparateCredentials(t *testing.T) {
	authDir := t.TempDir()
	const email = "user@example.com"
	store := NewFileTokenStore()
	paths := make([]string, 0, 2)
	for _, organization := range []string{"organization-a", "organization-b"} {
		fileName := claudeauth.CredentialFileName(email, organization, "shared-account")
		record := &coreauth.Auth{
			ID: fileName, FileName: fileName, Provider: "claude",
			Metadata: map[string]any{
				"type": "claude", "email": email, "organization_uuid": organization,
				"account_uuid": "shared-account", "access_token": "token-" + organization,
			},
		}
		manager := NewManager(store, recordManagerAuthenticator{provider: "claude", record: record})
		_, savedPath, errLogin := manager.Login(
			context.Background(), "claude", &config.Config{AuthDir: authDir}, nil,
		)
		if errLogin != nil {
			t.Fatalf("Login(%s) error = %v", organization, errLogin)
		}
		paths = append(paths, savedPath)
	}
	if paths[0] == paths[1] {
		t.Fatalf("organization credentials share path %q", paths[0])
	}
	for index, organization := range []string{"organization-a", "organization-b"} {
		saved := readManagerCredential(t, paths[index])
		if saved["organization_uuid"] != organization || saved["access_token"] != "token-"+organization {
			t.Fatalf("saved %s credential = %#v", organization, saved)
		}
	}
	store.SetBaseDir(authDir)
	auths, errList := store.List(context.Background())
	if errList != nil {
		t.Fatalf("List() error = %v", errList)
	}
	if len(auths) != 2 {
		t.Fatalf("List() auth count = %d, want 2", len(auths))
	}
}

func TestManagerLoginReusesDevinIdentityWhenEnrichmentFails(t *testing.T) {
	authDir := t.TempDir()
	token := devinauth.FormatSessionToken("private-token")
	canonicalFileName := devinauth.CredentialFileName("user-1", "org-1", token)
	canonicalPath := filepath.Join(authDir, canonicalFileName)
	writeManagerCredential(t, canonicalPath, map[string]any{
		"type": "devin", "api_key": token, "session_token": token,
		"user_name": "same-name", "user_id": "user-1", "org_id": "org-1",
		"disabled": true, "prefix": "team", "note": "keep",
	})
	fallbackFileName := devinauth.CredentialFileName("", "", token)
	record := &coreauth.Auth{
		ID: fallbackFileName, FileName: fallbackFileName, Provider: "devin",
		Metadata: map[string]any{
			"type": "devin", "api_key": token, "session_token": token,
			"base_url": devinauth.DefaultServerURL, "auth_kind": "oauth",
		},
	}
	manager := NewManager(NewFileTokenStore(), recordManagerAuthenticator{provider: "devin", record: record})
	_, savedPath, errLogin := manager.Login(
		context.Background(), "devin", &config.Config{AuthDir: authDir}, nil,
	)
	if errLogin != nil {
		t.Fatal(errLogin)
	}
	if savedPath != canonicalPath || record.FileName != canonicalFileName {
		t.Fatalf("Devin path/identity = %q/%q, want %q", savedPath, record.FileName, canonicalFileName)
	}
	saved := readManagerCredential(t, savedPath)
	if saved["user_id"] != "user-1" || saved["org_id"] != "org-1" ||
		saved["note"] != "keep" || saved["disabled"] != true {
		t.Fatalf("preserved Devin metadata = %#v", saved)
	}
	if record.Attributes["user_id"] != "user-1" || record.Attributes["org_id"] != "org-1" {
		t.Fatalf("runtime Devin identity = %#v", record.Attributes)
	}
	if _, errStat := os.Stat(filepath.Join(authDir, fallbackFileName)); !errors.Is(errStat, os.ErrNotExist) {
		t.Fatalf("fallback credential was created: %v", errStat)
	}
}

func writeManagerCredential(t *testing.T, path string, metadata map[string]any) {
	t.Helper()
	raw, errJSON := json.Marshal(metadata)
	if errJSON != nil {
		t.Fatal(errJSON)
	}
	if errWrite := os.WriteFile(path, raw, 0o600); errWrite != nil {
		t.Fatal(errWrite)
	}
}

func readManagerCredential(t *testing.T, path string) map[string]any {
	t.Helper()
	raw, errRead := os.ReadFile(path)
	if errRead != nil {
		t.Fatal(errRead)
	}
	metadata := make(map[string]any)
	if errJSON := json.Unmarshal(raw, &metadata); errJSON != nil {
		t.Fatal(errJSON)
	}
	return metadata
}
