package auth

import (
	"context"
	"net/http"
	"strings"
	"time"

	coreusage "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/usage"
)

const homeResultExecutorType = "home-result"

// ReportHomeUnauthorized publishes a result-only zero-token usage record for a Home 401 attempt.
func (m *Manager) ReportHomeUnauthorized(ctx context.Context, auth *Auth, provider, model string, upstreamBody ...[]byte) {
	failureBody := "upstream unauthorized"
	if len(upstreamBody) > 0 {
		failureBody = string(upstreamBody[0])
	}
	m.reportHomeUnauthorized(ctx, auth, provider, model, AccessTokenSHA256(auth), failureBody)
}

func (m *Manager) reportHomeUnauthorized(
	ctx context.Context,
	auth *Auth,
	provider,
	model,
	accessTokenSHA256,
	failureBody string,
) {
	if m == nil || auth == nil {
		return
	}
	authIndex := strings.TrimSpace(auth.Index)
	if authIndex == "" {
		authIndex = strings.TrimSpace(auth.EnsureIndex())
	}
	accessTokenSHA256 = strings.TrimSpace(accessTokenSHA256)
	if authIndex == "" || accessTokenSHA256 == "" {
		return
	}
	provider = strings.TrimSpace(provider)
	if provider == "" {
		provider = strings.TrimSpace(auth.Provider)
	}
	model = strings.TrimSpace(model)
	alias := strings.TrimSpace(coreusage.RequestedModelAliasFromContext(ctx))
	if alias == "" {
		alias = model
	}
	authType, source := auth.AccountInfo()
	if authType == "" && auth.Attributes != nil {
		authType = strings.TrimSpace(auth.Attributes["auth_kind"])
	}
	record := coreusage.Record{
		Provider:          provider,
		ExecutorType:      homeResultExecutorType,
		Model:             model,
		Alias:             alias,
		AuthID:            auth.ID,
		AuthIndex:         authIndex,
		AccessTokenSHA256: accessTokenSHA256,
		AuthType:          authType,
		Source:            source,
		ReasoningEffort:   coreusage.ReasoningEffortFromContext(ctx),
		ServiceTier:       coreusage.ServiceTierFromContext(ctx),
		Generate:          new(false),
		RequestedAt:       time.Now(),
		Failed:            true,
		Fail: coreusage.Failure{
			StatusCode: http.StatusUnauthorized,
			Body:       failureBody,
		},
	}
	if source != "" && strings.EqualFold(authType, "oauth") {
		record.SourceKind = coreusage.CredentialSourceKindIdentity
	}
	_ = coreusage.PublishRecord(ctx, record)
}
