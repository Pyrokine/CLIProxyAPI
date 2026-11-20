package executor

import (
	"context"
	"sync"
	"testing"

	claudeauth "github.com/Pyrokine/CLIProxyAPI/v6/internal/auth/claude"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	cliproxyauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
)

func newSharedClaudeOAuthAuth(id string) *cliproxyauth.Auth {
	return &cliproxyauth.Auth{
		ID:         id,
		Attributes: map[string]string{"api_key": "sk-ant-oat-race-probe"},
		Metadata:   map[string]any{},
	}
}

func TestClaudeExecutorPrepareRequestAuthIsRaceFreeOnSharedCredential(t *testing.T) {
	executor := NewClaudeExecutor(&config.Config{})
	executor.oauthProfileFetcher = func(context.Context, *cliproxyauth.Auth, string) (*claudeauth.OAuthProfile, error) {
		profile := &claudeauth.OAuthProfile{}
		//goland:noinspection SpellCheckingInspection
		profile.Account.UUID = "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"
		profile.Account.Email = "user@example.com"
		//goland:noinspection SpellCheckingInspection
		profile.Organization.UUID = "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb"
		profile.Organization.Name = "Example Org"
		return profile, nil
	}

	auth := newSharedClaudeOAuthAuth("claude-race-prepare")
	ctx := context.Background()

	var wg sync.WaitGroup
	for range 32 {
		wg.Go(
			func() {
				if executor.ShouldPrepareRequestAuth(auth) {
					if _, err := executor.PrepareRequestAuth(ctx, auth); err != nil {
						t.Errorf("PrepareRequestAuth() error = %v", err)
					}
					return
				}
				_ = executor.ShouldPrepareRequestAuth(auth)
			},
		)
	}
	wg.Wait()

	//goland:noinspection SpellCheckingInspection
	const wantAccountUUID = "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"
	if got := claudeauth.ReadMetadataString(&auth.Metadata, "account_uuid"); got != wantAccountUUID {
		t.Fatalf("account_uuid = %q, want the fetched profile account", got)
	}
	if !claudeauth.HasCanonicalDeviceIDPool(claudeauth.ReadDeviceIDPool(&auth.Metadata)) {
		t.Fatal("device ID pool was not established under concurrency")
	}
}

func TestClaudeExecutorSharedCredentialMetadataReadersUseOneLock(t *testing.T) {
	auth := &cliproxyauth.Auth{
		ID: "claude-race-all-readers", Metadata: map[string]any{
			"access_token":          "sk-ant-oat-race-probe",
			"cloak_mode":            "always",
			"cloak_sensitive_words": "secret",
		},
	}

	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := range 64 {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			if i%3 == 0 {
				claudeauth.StoreMetadataValue(&auth.Metadata, "access_token", "sk-ant-oat-race-probe")
				claudeauth.StoreMetadataValue(&auth.Metadata, "cloak_mode", "always")
				return
			}
			if i%3 == 1 {
				_, _ = claudeCreds(auth)
				return
			}
			_, _, _, _ = getCloakConfigFromAuth(auth)
		}(i)
	}
	close(start)
	wg.Wait()
}

func TestClaudeExecutorSetupTokenMetadataReadersUseOneLock(t *testing.T) {
	for _, key := range []string{"skip_account_profile", "is_setup_token", "setup_token"} {
		t.Run(
			key, func(t *testing.T) {
				auth := newSharedClaudeOAuthAuth(t.Name())
				claudeauth.StoreMetadataValue(&auth.Metadata, key, true)
				var wg sync.WaitGroup
				start := make(chan struct{})
				for i := range 32 {
					wg.Go(
						func() {
							<-start
							if i%2 == 0 {
								claudeauth.StoreMetadataValue(&auth.Metadata, key, true)
								return
							}
							if !isClaudeSetupToken(auth, auth.Attributes["api_key"]) {
								t.Error("setup token flag was not preserved during metadata updates")
							}
						},
					)
				}
				close(start)
				wg.Wait()
			},
		)
	}
}

func TestClaudeExecutorSharedCredentialMetadataMixedAccess(t *testing.T) {
	executor := NewClaudeExecutor(&config.Config{})
	executor.oauthProfileFetcher = func(context.Context, *cliproxyauth.Auth, string) (*claudeauth.OAuthProfile, error) {
		profile := &claudeauth.OAuthProfile{}
		//goland:noinspection SpellCheckingInspection
		profile.Account.UUID = "cccccccc-cccc-4ccc-8ccc-cccccccccccc"
		return profile, nil
	}

	auth := newSharedClaudeOAuthAuth("claude-race-mixed")
	ctx := context.Background()

	var wg sync.WaitGroup
	for i := range 32 {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			switch i % 4 {
			case 0:
				if _, err := executor.PrepareRequestAuth(ctx, auth); err != nil {
					t.Errorf("PrepareRequestAuth() error = %v", err)
				}
			case 1:
				_ = executor.ShouldPrepareRequestAuth(auth)
			case 2:
				_ = claudeauth.ReadMetadataString(&auth.Metadata, "account_uuid")
			default:
				_ = claudeauth.ReadDeviceIDPool(&auth.Metadata)
			}
		}(i)
	}
	wg.Wait()
}
