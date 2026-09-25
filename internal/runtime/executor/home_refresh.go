package executor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/home"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/logging"
	cliproxyauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
)

func (e statusErr) LogDiagnostic() string {
	return logging.SafeDiagnosticForLog(e.logDiagnostic)
}

type homeRefreshErrorEnvelope struct {
	Error *homeRefreshErrorDetail `json:"error"`
}

type homeRefreshAuthEnvelope struct {
	Auth      cliproxyauth.Auth `json:"auth"`
	AuthIndex string            `json:"auth_index"`
}

type homeRefreshErrorDetail struct {
	Type       string                       `json:"type"`
	Message    string                       `json:"message"`
	Code       string                       `json:"code,omitempty"`
	Diagnostic string                       `json:"diagnostic,omitempty"`
	Upstream   *homeRefreshUpstreamResponse `json:"upstream,omitempty"`
}

type homeRefreshUpstreamResponse struct {
	Status int    `json:"status"`
	Body   []byte `json:"body"`
}

type homeRefreshClient interface {
	HeartbeatOK() bool
	GetRefreshAuth(ctx context.Context, authIndex string, accessTokenSHA256 string) ([]byte, error)
}

var currentHomeRefreshClient = func() homeRefreshClient {
	return home.Current()
}

func homeRefreshClientForContext(ctx context.Context) homeRefreshClient {
	if client, ok := cliproxyauth.HomeRefreshClientFromContext(ctx); ok {
		return client
	}
	return currentHomeRefreshClient()
}

func refreshAuthViaHome(ctx context.Context, cfg *config.Config, auth *cliproxyauth.Auth) (
	*cliproxyauth.Auth,
	bool,
	error,
) {
	if cfg == nil || !cfg.Home.Enabled {
		return nil, false, nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if auth == nil {
		return nil, true, statusErr{code: http.StatusInternalServerError, msg: "home refresh: auth is nil"}
	}

	client := homeRefreshClientForContext(ctx)
	if client == nil || !client.HeartbeatOK() {
		return nil, true, statusErr{code: http.StatusServiceUnavailable, msg: "home control center unavailable"}
	}

	authIndex := strings.TrimSpace(auth.Index)
	if authIndex == "" {
		authIndex = strings.TrimSpace(auth.EnsureIndex())
	}
	if authIndex == "" {
		return nil, true, statusErr{code: http.StatusBadGateway, msg: "home refresh: auth_index is empty"}
	}

	raw, err := client.GetRefreshAuth(ctx, authIndex, cliproxyauth.AccessTokenSHA256(auth))
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return nil, true, err
		}
		return nil, true, statusErr{
			code: http.StatusServiceUnavailable, msg: "home refresh temporarily unavailable",
			logDiagnostic: "Home refresh transport failed: " + logging.SafeErrorDiagnostic(err),
		}
	}

	var env homeRefreshErrorEnvelope
	if errUnmarshal := json.Unmarshal(raw, &env); errUnmarshal != nil {
		return nil, true, statusErr{
			code: http.StatusBadGateway, msg: "home returned invalid auth payload",
			logDiagnostic: "Home refresh response decode failed: " + logging.SafeErrorDiagnostic(errUnmarshal),
		}
	}
	if env.Error != nil {
		diagnostic := strings.TrimSpace(env.Error.Diagnostic)
		if upstream := env.Error.Upstream; upstream != nil {
			if upstream.Status < http.StatusBadRequest || upstream.Status > 599 {
				return nil, true, statusErr{code: http.StatusBadGateway, msg: "home returned invalid upstream status"}
			}
			if diagnostic == "" {
				diagnostic = fmt.Sprintf("Home refresh upstream response: status=%d", upstream.Status)
			}
			return nil, true, statusErr{
				code: upstream.Status, msg: string(upstream.Body), directResponse: true, logDiagnostic: diagnostic,
			}.withUpstreamResponse(nil)
		}
		code := strings.TrimSpace(env.Error.Type)
		if code == "" {
			code = strings.TrimSpace(env.Error.Code)
		}
		statusCode := statusFromHomeRefreshErrorCode(code)
		message := "credential refresh temporarily unavailable"
		switch statusCode {
		case http.StatusUnauthorized:
			message = "credential unauthorized"
		case http.StatusNotFound:
			message = "credential refresh target not found"
		}
		if diagnostic == "" {
			diagnostic = message
			switch safeType := strings.ToLower(code); safeType {
			case "authentication_error", "unauthorized", "invalid_grant", "refresh_token_expired", "refresh_token_revoked", "refresh_token_reused", "model_not_found", "auth_not_found", "auth_unavailable", "refresh_temporarily_unavailable", "refresh_unsupported", "home_unavailable":
				diagnostic = "Home refresh failed: type=" + safeType
			}
		}
		return nil, true, statusErr{code: statusCode, msg: message, logDiagnostic: diagnostic}
	}

	updated, returnedIndex, errParse := parseHomeRefreshAuth(raw)
	if errParse != nil {
		return nil, true, statusErr{
			code: http.StatusBadGateway, msg: "home returned invalid auth payload",
			logDiagnostic: "Home refresh response decode failed: " + logging.SafeErrorDiagnostic(errParse),
		}
	}
	if updated.Disabled || updated.Status == cliproxyauth.StatusDisabled {
		return nil, true, statusErr{code: http.StatusUnauthorized, msg: "credential unauthorized"}
	}
	updated.ID = auth.ID
	if returnedIndex != "" {
		authIndex = returnedIndex
	}
	updated.AssignIndex(authIndex)
	return updated, true, nil
}

func parseHomeRefreshAuth(raw []byte) (*cliproxyauth.Auth, string, error) {
	var rawObject map[string]json.RawMessage
	if errUnmarshal := json.Unmarshal(raw, &rawObject); errUnmarshal != nil {
		return nil, "", errUnmarshal
	}
	if _, ok := rawObject["auth"]; ok {
		var envelope homeRefreshAuthEnvelope
		if errUnmarshal := json.Unmarshal(raw, &envelope); errUnmarshal != nil {
			return nil, "", errUnmarshal
		}
		return &envelope.Auth, strings.TrimSpace(envelope.AuthIndex), nil
	}
	var updated cliproxyauth.Auth
	if errUnmarshal := json.Unmarshal(raw, &updated); errUnmarshal != nil {
		return nil, "", errUnmarshal
	}
	return &updated, "", nil
}

func statusFromHomeRefreshErrorCode(code string) int {
	switch strings.ToLower(strings.TrimSpace(code)) {
	case "authentication_error", "unauthorized", "invalid_grant", "refresh_token_expired", "refresh_token_revoked", "refresh_token_reused":
		return http.StatusUnauthorized
	case "model_not_found":
		return http.StatusNotFound
	case "auth_not_found", "auth_unavailable", "refresh_temporarily_unavailable", "refresh_unsupported", "home_unavailable":
		return http.StatusServiceUnavailable
	default:
		return http.StatusServiceUnavailable
	}
}
