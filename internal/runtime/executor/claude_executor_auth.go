package executor

import (
	"context"
	"fmt"
	"strings"
	"time"

	claudeauth "github.com/Pyrokine/CLIProxyAPI/v6/internal/auth/claude"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/runtime/executor/helps"
	cliproxyauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
	log "github.com/sirupsen/logrus"
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

func isClaudeSetupToken(auth *cliproxyauth.Auth, apiKey string) bool {
	if !isClaudeOAuthToken(apiKey) || auth == nil {
		return false
	}
	if skip, _ := auth.Metadata["skip_account_profile"].(bool); skip {
		return true
	}
	if setupToken, _ := auth.Metadata["is_setup_token"].(bool); setupToken {
		return true
	}
	if setupToken, _ := auth.Metadata["setup_token"].(bool); setupToken {
		return true
	}
	if kind := strings.ToLower(strings.TrimSpace(auth.Attributes["auth_kind"])); kind == "setup_token" || kind == "setup-token" {
		return true
	}
	scopes := strings.ToLower(claudeauth.ReadMetadataString(&auth.Metadata, "scopes"))
	if scopes == "" {
		scopes = strings.ToLower(claudeauth.ReadMetadataString(&auth.Metadata, "scope"))
	}
	return scopes != "" && !strings.Contains(scopes, "user:profile") && !strings.Contains(scopes, "user:office")
}

func isClaudeOAuthScope403(err error) bool {
	if err == nil {
		return false
	}
	message := strings.ToLower(err.Error())
	for _, marker := range []string{
		"status 403",
		"403 forbidden",
		"scope requirement",
		"insufficient_scope",
		"user:profile",
		"user:office",
	} {
		if strings.Contains(message, marker) {
			return true
		}
	}
	return false
}

func storeClaudeFallbackAccountIdentity(auth *cliproxyauth.Auth, apiKey, kind string) {
	seed := helps.ClaudeCLIAuthIdentitySeed(auth)
	if seed == "" {
		seed = kind + "|" + apiKey
	}
	claudeauth.StoreMetadataString(&auth.Metadata, "account_uuid", helps.StableClaudeCLIAccountUUID(seed))
	claudeauth.StoreMetadataString(
		&auth.Metadata, claudeAccountProfileCheckedAtKey, time.Now().UTC().Format(time.RFC3339),
	)
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
	if isClaudeSetupToken(auth, apiKey) {
		storeClaudeFallbackAccountIdentity(auth, apiKey, "claude-setup-token")
		return auth, nil
	}

	profile, errProfile := e.fetchClaudeOAuthProfile(ctx, auth, apiKey)
	if errProfile != nil {
		if errContext := ctx.Err(); errContext != nil {
			return nil, errContext
		}
		if isClaudeOAuthScope403(errProfile) {
			log.Debugf(
				"Claude OAuth account profile lookup is not available for auth %s: %v; using stable credential identity",
				auth.ID,
				errProfile,
			)
			storeClaudeFallbackAccountIdentity(auth, apiKey, "claude-oauth-fallback")
			return auth, nil
		}
		return nil, fmt.Errorf("populate Claude OAuth account profile: %w", errProfile)
	}
	if profile == nil || strings.TrimSpace(profile.Account.UUID) == "" {
		log.Debugf(
			"Claude OAuth account profile lookup returned no account UUID for auth %s; using stable credential identity",
			auth.ID,
		)
		storeClaudeFallbackAccountIdentity(auth, apiKey, "claude-oauth-fallback")
		return auth, nil
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
	if auth == nil {
		return nil, fmt.Errorf("fetch Claude OAuth profile: auth is nil")
	}
	profileCtx, cancelProfile := context.WithTimeout(ctx, claudeAccountProfileTimeout)
	defer cancelProfile()
	if e.oauthProfileFetcher != nil {
		return e.oauthProfileFetcher(profileCtx, auth, apiKey)
	}
	service := claudeauth.NewAuthWithProxyURL(e.cfg, auth.ProxyURL)
	return service.FetchOAuthProfile(profileCtx, apiKey)
}
