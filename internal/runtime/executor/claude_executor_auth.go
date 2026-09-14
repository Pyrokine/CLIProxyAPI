package executor

import (
	"context"
	"fmt"
	"strings"
	"time"

	claudeauth "github.com/Pyrokine/CLIProxyAPI/v6/internal/auth/claude"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/runtime/executor/helps"
	cliproxyauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
)

const (
	claudeAccountProfileCheckedAtKey = "claude_account_profile_checked_at"
	claudeAccountProfileTimeout      = 10 * time.Second
)

type claudeOAuthProfileFetcher func(context.Context, *cliproxyauth.Auth, string) (*claudeauth.OAuthProfile, error)

func (e *ClaudeExecutor) ShouldPrepareRequestAuth(auth *cliproxyauth.Auth) bool {
	apiKey, _ := claudeCreds(auth)
	if !isClaudeOAuthToken(apiKey) || auth == nil {
		return false
	}
	if !claudeauth.HasCanonicalDeviceIDPool(claudeauth.ReadDeviceIDPool(&auth.Metadata)) {
		return true
	}
	return helps.ClaudeCredentialAccountUUID(auth) == ""
}

func (e *ClaudeExecutor) PrepareRequestAuth(ctx context.Context, auth *cliproxyauth.Auth) (*cliproxyauth.Auth, error) {
	if auth == nil || !e.ShouldPrepareRequestAuth(auth) {
		return auth, nil
	}
	apiKey, _ := claudeCreds(auth)
	claudeauth.EnsureMetadataMap(&auth.Metadata)
	if _, errDeviceIDs := helps.EnsureClaudeCredentialDevicePoolRequired(ctx, auth); errDeviceIDs != nil {
		return nil, errDeviceIDs
	}
	if helps.ClaudeCredentialAccountUUID(auth) != "" {
		return auth, nil
	}

	profile, errProfile := e.fetchClaudeOAuthProfile(ctx, auth, apiKey)
	if errProfile != nil {
		if errContext := ctx.Err(); errContext != nil {
			return nil, errContext
		}
		return nil, fmt.Errorf("populate Claude OAuth account profile: %w", errProfile)
	}
	if profile == nil || strings.TrimSpace(profile.Account.UUID) == "" {
		return nil, fmt.Errorf("populate Claude OAuth account profile: account UUID is empty")
	}
	claudeauth.StoreMetadataString(&auth.Metadata, "account_uuid", profile.Account.UUID)
	claudeauth.StoreMetadataString(&auth.Metadata, "email", profile.Account.Email)
	claudeauth.StoreMetadataString(&auth.Metadata, "organization_uuid", profile.Organization.UUID)
	claudeauth.StoreMetadataString(&auth.Metadata, "organization_name", profile.Organization.Name)
	claudeauth.StoreMetadataString(
		&auth.Metadata, claudeAccountProfileCheckedAtKey, time.Now().UTC().Format(time.RFC3339),
	)
	return auth, nil
}

func (e *ClaudeExecutor) fetchClaudeOAuthProfile(
	ctx context.Context,
	auth *cliproxyauth.Auth,
	apiKey string,
) (*claudeauth.OAuthProfile, error) {
	if e == nil {
		return nil, fmt.Errorf("fetch Claude OAuth profile: executor is nil")
	}
	if e.oauthProfileFetcher != nil {
		return e.oauthProfileFetcher(ctx, auth, apiKey)
	}
	if auth == nil {
		return nil, fmt.Errorf("fetch Claude OAuth profile: auth is nil")
	}
	profileCtx, cancelProfile := context.WithTimeout(ctx, claudeAccountProfileTimeout)
	defer cancelProfile()
	service := claudeauth.NewAuthWithProxyURL(e.cfg, auth.ProxyURL)
	return service.FetchOAuthProfile(profileCtx, apiKey)
}
