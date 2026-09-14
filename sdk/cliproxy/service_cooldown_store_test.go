package cliproxy

import (
	"context"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	sdkAuth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/auth"
	coreauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
)

type cooldownProviderTokenStore struct {
	cooldownStore coreauth.CooldownStateStore
}

func (s *cooldownProviderTokenStore) List(context.Context) ([]*coreauth.Auth, error) {
	return nil, nil
}

func (s *cooldownProviderTokenStore) Save(context.Context, *coreauth.Auth) (string, error) {
	return "", nil
}

func (s *cooldownProviderTokenStore) Delete(context.Context, string) error {
	return nil
}

func (s *cooldownProviderTokenStore) CooldownStateStore() coreauth.CooldownStateStore {
	return s.cooldownStore
}

type serviceCooldownStateStore struct {
	mu      sync.Mutex
	load    []coreauth.CooldownStateRecord
	records []coreauth.CooldownStateRecord
}

func (s *serviceCooldownStateStore) Load(context.Context) ([]coreauth.CooldownStateRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]coreauth.CooldownStateRecord(nil), s.load...), nil
}

func (s *serviceCooldownStateStore) Save(_ context.Context, records []coreauth.CooldownStateRecord) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.records = append([]coreauth.CooldownStateRecord(nil), records...)
	return nil
}

func TestResolveCooldownStateStoreUsesCapturedBackendProvider(t *testing.T) {
	originalStore := sdkAuth.GetTokenStore()
	t.Cleanup(
		func() {
			sdkAuth.RegisterTokenStore(originalStore)
		},
	)

	providedStore := &serviceCooldownStateStore{}
	sdkAuth.RegisterTokenStore(&cooldownProviderTokenStore{cooldownStore: providedStore})
	cfg := &config.Config{
		AuthDir:            t.TempDir(),
		SaveCooldownStatus: true,
	}
	service, errBuild := NewBuilder().
		WithConfig(cfg).
		WithConfigPath(filepath.Join(t.TempDir(), "config.yaml")).
		Build()
	if errBuild != nil {
		t.Fatalf("Build() error = %v", errBuild)
	}

	sdkAuth.RegisterTokenStore(&cooldownProviderTokenStore{cooldownStore: &serviceCooldownStateStore{}})
	got := service.resolveCooldownStateStore(cfg)
	if got != providedStore {
		t.Fatalf("resolveCooldownStateStore() = %T, want captured backend-provided store", got)
	}
}

func TestResolveCooldownStateStoreUsesExplicitOverride(t *testing.T) {
	originalStore := sdkAuth.GetTokenStore()
	t.Cleanup(func() { sdkAuth.RegisterTokenStore(originalStore) })

	providedStore := &serviceCooldownStateStore{}
	overrideStore := &serviceCooldownStateStore{}
	sdkAuth.RegisterTokenStore(&cooldownProviderTokenStore{cooldownStore: providedStore})
	cfg := &config.Config{AuthDir: t.TempDir(), SaveCooldownStatus: true}
	service, errBuild := NewBuilder().
		WithConfig(cfg).
		WithConfigPath(filepath.Join(t.TempDir(), "config.yaml")).
		WithCooldownStateStore(overrideStore).
		Build()
	if errBuild != nil {
		t.Fatalf("Build() error = %v", errBuild)
	}
	if got := service.resolveCooldownStateStore(cfg); got != overrideStore {
		t.Fatalf("resolveCooldownStateStore() = %T, want explicit override", got)
	}
}

func TestResolveCooldownStateStoreUsesFileFallbackWithCustomManager(t *testing.T) {
	originalStore := sdkAuth.GetTokenStore()
	t.Cleanup(func() { sdkAuth.RegisterTokenStore(originalStore) })

	sdkAuth.RegisterTokenStore(&cooldownProviderTokenStore{cooldownStore: &serviceCooldownStateStore{}})
	cfg := &config.Config{AuthDir: t.TempDir(), SaveCooldownStatus: true}
	service, errBuild := NewBuilder().
		WithConfig(cfg).
		WithConfigPath(filepath.Join(t.TempDir(), "config.yaml")).
		WithCoreAuthManager(coreauth.NewManager(nil, nil, nil)).
		Build()
	if errBuild != nil {
		t.Fatalf("Build() error = %v", errBuild)
	}
	if _, ok := service.resolveCooldownStateStore(cfg).(*coreauth.FileCooldownStateStore); !ok {
		t.Fatalf("resolveCooldownStateStore() = %T, want file fallback", service.resolveCooldownStateStore(cfg))
	}
}

func TestResolveCooldownStateStoreDisabledForConfigAndHome(t *testing.T) {
	store := &serviceCooldownStateStore{}
	service := &Service{cooldownStateStore: store}
	if got := service.resolveCooldownStateStore(&config.Config{SaveCooldownStatus: false}); got != nil {
		t.Fatalf("disabled resolveCooldownStateStore() = %T, want nil", got)
	}
	if got := service.resolveCooldownStateStore(
		&config.Config{
			SaveCooldownStatus: true,
			Home:               config.HomeConfig{Enabled: true},
		},
	); got != nil {
		t.Fatalf("Home resolveCooldownStateStore() = %T, want nil", got)
	}
}

func TestRegisterConfigAPIKeyAuthsBeforeCooldownRestore(t *testing.T) {
	cfg := &config.Config{CodexKey: []config.CodexKey{{APIKey: "test-key"}}}
	manager := coreauth.NewManager(nil, nil, nil)
	service := &Service{cfg: cfg, coreManager: manager}
	if errRegister := service.registerConfigAPIKeyAuths(context.Background(), cfg); errRegister != nil {
		t.Fatalf("registerConfigAPIKeyAuths() error = %v", errRegister)
	}
	auths := manager.List()
	if len(auths) != 1 {
		t.Fatalf("registered config auths = %d, want 1", len(auths))
	}

	now := time.Now().UTC()
	store := &serviceCooldownStateStore{
		load: []coreauth.CooldownStateRecord{
			{
				AuthID:         auths[0].ID,
				Model:          "gpt-test",
				NextRetryAfter: now.Add(time.Hour),
				Quota: coreauth.QuotaState{
					Exceeded:      true,
					NextRecoverAt: now.Add(time.Hour),
				},
				UpdatedAt: now,
			},
		},
	}
	manager.SetCooldownStateStore(store)
	if errRestore := manager.RestoreCooldownStates(context.Background()); errRestore != nil {
		t.Fatalf("RestoreCooldownStates() error = %v", errRestore)
	}
	restored, ok := manager.GetByID(auths[0].ID)
	if !ok || restored.ModelStates["gpt-test"] == nil || !restored.ModelStates["gpt-test"].Quota.Exceeded {
		t.Fatalf("restored config auth cooldown = %#v", restored)
	}
}
