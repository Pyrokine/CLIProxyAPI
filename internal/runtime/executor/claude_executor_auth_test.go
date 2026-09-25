package executor

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	claudeauth "github.com/Pyrokine/CLIProxyAPI/v6/internal/auth/claude"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/runtime/executor/helps"
	cliproxyauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
)

func newClaudeProfileTestAuth(id string) *cliproxyauth.Auth {
	return &cliproxyauth.Auth{
		ID:         id,
		Attributes: map[string]string{"api_key": "sk-ant-oat01-profile-test"},
		Metadata: map[string]any{
			claudeauth.DeviceIDsMetadataKey: []string{strings.Repeat("a", 64)},
		},
	}
}

func TestPrepareRequestAuthSkipsProfileForSetupTokens(t *testing.T) {
	tests := []struct {
		name  string
		apply func(*cliproxyauth.Auth)
	}{
		{name: "skip account profile", apply: func(auth *cliproxyauth.Auth) { auth.Metadata["skip_account_profile"] = true }},
		{name: "is setup token", apply: func(auth *cliproxyauth.Auth) { auth.Metadata["is_setup_token"] = true }},
		{name: "setup token", apply: func(auth *cliproxyauth.Auth) { auth.Metadata["setup_token"] = true }},
		{name: "auth kind underscore", apply: func(auth *cliproxyauth.Auth) { auth.Attributes["auth_kind"] = "setup_token" }},
		{name: "auth kind hyphen", apply: func(auth *cliproxyauth.Auth) { auth.Attributes["auth_kind"] = "setup-token" }},
		{name: "scope without profile", apply: func(auth *cliproxyauth.Auth) { auth.Metadata["scope"] = "org:create_api_key user:inference" }},
		{name: "scopes without office", apply: func(auth *cliproxyauth.Auth) { auth.Metadata["scopes"] = "user:inference" }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			auth := newClaudeProfileTestAuth("setup-token-" + test.name)
			test.apply(auth)
			var profileCalls atomic.Int32
			executor := NewClaudeExecutor(&config.Config{})
			executor.oauthProfileFetcher = func(context.Context, *cliproxyauth.Auth, string) (*claudeauth.OAuthProfile, error) {
				profileCalls.Add(1)
				return nil, errors.New("profile fetch must not run")
			}

			prepared, errPrepare := executor.PrepareRequestAuth(t.Context(), auth)
			if errPrepare != nil {
				t.Fatal(errPrepare)
			}
			if prepared != auth {
				t.Fatal("PrepareRequestAuth returned a different auth")
			}
			if got := profileCalls.Load(); got != 0 {
				t.Fatalf("profile calls = %d, want 0", got)
			}
			wantUUID := helps.StableClaudeCLIAccountUUID(helps.ClaudeCLIAuthIdentitySeed(auth))
			if got := helps.ClaudeCredentialAccountUUID(auth); got != wantUUID {
				t.Fatalf("account UUID = %q, want %q", got, wantUUID)
			}
			if checkedAt := claudeauth.ReadMetadataString(&auth.Metadata, claudeAccountProfileCheckedAtKey); checkedAt == "" {
				t.Fatal("profile checked timestamp is empty")
			}
		})
	}
}

func TestPrepareRequestAuthStoresOAuthProfile(t *testing.T) {
	auth := newClaudeProfileTestAuth("profile-success")
	profile := &claudeauth.OAuthProfile{}
	profile.Account.UUID = "account-uuid"
	profile.Account.Email = "person@example.com"
	profile.Organization.UUID = "organization-uuid"
	profile.Organization.Name = "organization-name"
	var profileCalls atomic.Int32
	executor := NewClaudeExecutor(&config.Config{})
	executor.oauthProfileFetcher = func(ctx context.Context, gotAuth *cliproxyauth.Auth, apiKey string) (*claudeauth.OAuthProfile, error) {
		profileCalls.Add(1)
		if gotAuth != auth || apiKey != auth.Attributes["api_key"] {
			t.Fatalf("profile fetch auth/key = %#v/%q", gotAuth, apiKey)
		}
		if errContext := ctx.Err(); errContext != nil {
			t.Fatal(errContext)
		}
		return profile, nil
	}

	prepared, errPrepare := executor.PrepareRequestAuth(t.Context(), auth)
	if errPrepare != nil {
		t.Fatal(errPrepare)
	}
	if prepared != auth || profileCalls.Load() != 1 {
		t.Fatalf("prepared = %p, auth = %p, profile calls = %d", prepared, auth, profileCalls.Load())
	}
	for key, want := range map[string]string{
		"account_uuid":      profile.Account.UUID,
		"email":             profile.Account.Email,
		"organization_uuid": profile.Organization.UUID,
		"organization_name": profile.Organization.Name,
	} {
		if got := claudeauth.ReadMetadataString(&auth.Metadata, key); got != want {
			t.Fatalf("metadata[%q] = %q, want %q", key, got, want)
		}
	}
	if checkedAt := claudeauth.ReadMetadataString(&auth.Metadata, claudeAccountProfileCheckedAtKey); checkedAt == "" {
		t.Fatal("profile checked timestamp is empty")
	}
	if executor.ShouldPrepareRequestAuth(auth) {
		t.Fatal("prepared auth requested another profile lookup")
	}
}

func TestPrepareRequestAuthFallsBackForProfileScopeLimitations(t *testing.T) {
	tests := []struct {
		name    string
		profile *claudeauth.OAuthProfile
		err     error
	}{
		{name: "status 403", err: errors.New("fetch Claude OAuth profile failed with status 403")},
		{name: "insufficient scope", err: errors.New("oauth insufficient_scope: user:profile")},
		{name: "empty account UUID", profile: &claudeauth.OAuthProfile{}},
		{name: "nil profile"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			auth := newClaudeProfileTestAuth("profile-fallback-" + test.name)
			var profileCalls atomic.Int32
			executor := NewClaudeExecutor(&config.Config{})
			executor.oauthProfileFetcher = func(context.Context, *cliproxyauth.Auth, string) (*claudeauth.OAuthProfile, error) {
				profileCalls.Add(1)
				return test.profile, test.err
			}

			prepared, errPrepare := executor.PrepareRequestAuth(t.Context(), auth)
			if errPrepare != nil {
				t.Fatal(errPrepare)
			}
			if prepared != auth || profileCalls.Load() != 1 {
				t.Fatalf("prepared = %p, auth = %p, profile calls = %d", prepared, auth, profileCalls.Load())
			}
			wantUUID := helps.StableClaudeCLIAccountUUID(helps.ClaudeCLIAuthIdentitySeed(auth))
			if got := helps.ClaudeCredentialAccountUUID(auth); got != wantUUID {
				t.Fatalf("account UUID = %q, want %q", got, wantUUID)
			}

			preparedAgain, errAgain := executor.PrepareRequestAuth(t.Context(), auth)
			if errAgain != nil || preparedAgain != auth {
				t.Fatalf("second preparation = %p, error = %v", preparedAgain, errAgain)
			}
			if got := profileCalls.Load(); got != 1 {
				t.Fatalf("profile calls after repeated preparation = %d, want 1", got)
			}
		})
	}
}

func TestPrepareRequestAuthPropagatesProfileFailures(t *testing.T) {
	for _, test := range []struct {
		name string
		err  error
	}{
		{name: "unauthorized", err: errors.New("fetch Claude OAuth profile failed with status 401")},
		{name: "server error", err: errors.New("fetch Claude OAuth profile failed with status 500")},
		{name: "unrelated permission error", err: errors.New("permission_error from unrelated policy")},
		{name: "generic forbidden", err: errors.New("forbidden by unrelated policy")},
	} {
		t.Run(test.name, func(t *testing.T) {
			auth := newClaudeProfileTestAuth("profile-error-" + test.name)
			executor := NewClaudeExecutor(&config.Config{})
			executor.oauthProfileFetcher = func(context.Context, *cliproxyauth.Auth, string) (*claudeauth.OAuthProfile, error) {
				return nil, test.err
			}

			prepared, errPrepare := executor.PrepareRequestAuth(t.Context(), auth)
			if prepared != nil {
				t.Fatalf("prepared = %#v, want nil", prepared)
			}
			if !errors.Is(errPrepare, test.err) {
				t.Fatalf("PrepareRequestAuth error = %v, want %v", errPrepare, test.err)
			}
			if got := helps.ClaudeCredentialAccountUUID(auth); got != "" {
				t.Fatalf("account UUID = %q, want empty", got)
			}
		})
	}
}

func TestPrepareRequestAuthPropagatesCancellationAndDeadline(t *testing.T) {
	for _, test := range []struct {
		name       string
		newContext func() (context.Context, context.CancelFunc)
		want       error
	}{
		{name: "canceled", newContext: func() (context.Context, context.CancelFunc) {
			ctx, cancel := context.WithCancel(t.Context())
			cancel()
			return ctx, func() {}
		}, want: context.Canceled},
		{name: "deadline", newContext: func() (context.Context, context.CancelFunc) {
			return context.WithTimeout(t.Context(), 20*time.Millisecond)
		}, want: context.DeadlineExceeded},
	} {
		t.Run(test.name, func(t *testing.T) {
			auth := newClaudeProfileTestAuth("profile-context-" + test.name)
			executor := NewClaudeExecutor(&config.Config{})
			executor.oauthProfileFetcher = func(ctx context.Context, _ *cliproxyauth.Auth, _ string) (*claudeauth.OAuthProfile, error) {
				<-ctx.Done()
				return nil, ctx.Err()
			}
			ctx, cancel := test.newContext()
			defer cancel()

			prepared, errPrepare := executor.PrepareRequestAuth(ctx, auth)
			if prepared != nil {
				t.Fatalf("prepared = %#v, want nil", prepared)
			}
			if !errors.Is(errPrepare, test.want) {
				t.Fatalf("PrepareRequestAuth error = %v, want %v", errPrepare, test.want)
			}
			if got := helps.ClaudeCredentialAccountUUID(auth); got != "" {
				t.Fatalf("account UUID = %q, want empty", got)
			}
		})
	}
}

func TestFetchClaudeOAuthProfileAppliesInternalTimeout(t *testing.T) {
	auth := newClaudeProfileTestAuth("profile-internal-timeout")
	executor := NewClaudeExecutor(&config.Config{})
	var observedDeadline time.Time
	executor.oauthProfileFetcher = func(ctx context.Context, gotAuth *cliproxyauth.Auth, apiKey string) (*claudeauth.OAuthProfile, error) {
		if gotAuth != auth || apiKey != auth.Attributes["api_key"] {
			t.Fatalf("profile fetch auth/key = %#v/%q", gotAuth, apiKey)
		}
		var ok bool
		observedDeadline, ok = ctx.Deadline()
		if !ok {
			t.Fatal("profile fetch context has no deadline")
		}
		return &claudeauth.OAuthProfile{}, nil
	}
	startedAt := time.Now()

	if _, errFetch := executor.fetchClaudeOAuthProfile(context.Background(), auth, auth.Attributes["api_key"]); errFetch != nil {
		t.Fatal(errFetch)
	}
	if got := observedDeadline.Sub(startedAt); got < claudeAccountProfileTimeout-500*time.Millisecond ||
		got > claudeAccountProfileTimeout+500*time.Millisecond {
		t.Fatalf("profile timeout = %v, want %v", got, claudeAccountProfileTimeout)
	}
}

func TestClaudeRefreshTokenAcceptsMetadataSpellings(t *testing.T) {
	for _, test := range []struct {
		name     string
		metadata map[string]any
		want     string
	}{
		{name: "snake case", metadata: map[string]any{"refresh_token": "snake"}, want: "snake"},
		{name: "camel case", metadata: map[string]any{"refreshToken": "camel"}, want: "camel"},
		{name: "snake case precedence", metadata: map[string]any{"refresh_token": "snake", "refreshToken": "camel"}, want: "snake"},
		{name: "missing", metadata: map[string]any{}},
		{name: "nil auth"},
	} {
		t.Run(test.name, func(t *testing.T) {
			var auth *cliproxyauth.Auth
			if test.metadata != nil {
				auth = &cliproxyauth.Auth{Metadata: test.metadata}
			}
			if got := claudeRefreshToken(auth); got != test.want {
				t.Fatalf("claudeRefreshToken() = %q, want %q", got, test.want)
			}
		})
	}
}

func TestClaudeOAuthScope403ClassifierIsNarrow(t *testing.T) {
	for _, test := range []struct {
		err  error
		want bool
	}{
		{err: errors.New("fetch Claude OAuth profile failed with status 403"), want: true},
		{err: errors.New("403 Forbidden"), want: true},
		{err: errors.New("scope requirement user:office"), want: true},
		{err: errors.New("insufficient_scope"), want: true},
		{err: errors.New("permission_error")},
		{err: errors.New("forbidden")},
		{err: errors.New("status 401")},
		{err: fmt.Errorf("unrelated: %w", context.Canceled)},
		{},
	} {
		if got := isClaudeOAuthScope403(test.err); got != test.want {
			t.Fatalf("isClaudeOAuthScope403(%v) = %v, want %v", test.err, got, test.want)
		}
	}
}
