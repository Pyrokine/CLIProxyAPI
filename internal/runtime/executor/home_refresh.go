package executor

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/home"
	cliproxyauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
)

type homeRefreshErrorEnvelope struct {
	Error *homeRefreshErrorDetail `json:"error"`
}

type homeRefreshErrorDetail struct {
	Type    string `json:"type"`
	Message string `json:"message"`
	Code    string `json:"code,omitempty"`
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

	client := home.Current()
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

	raw, err := client.GetRefreshAuth(ctx, authIndex)
	if err != nil {
		return nil, true, statusErr{code: http.StatusBadGateway, msg: err.Error()}
	}

	var env homeRefreshErrorEnvelope
	if errUnmarshal := json.Unmarshal(raw, &env); errUnmarshal == nil && env.Error != nil {
		code := strings.TrimSpace(env.Error.Type)
		if code == "" {
			code = strings.TrimSpace(env.Error.Code)
		}
		msg := strings.TrimSpace(env.Error.Message)
		if msg == "" {
			msg = "home returned error"
		}
		return nil, true, statusErr{code: statusFromHomeRefreshErrorCode(code), msg: msg}
	}

	var updated cliproxyauth.Auth
	if errUnmarshal := json.Unmarshal(raw, &updated); errUnmarshal != nil {
		return nil, true, statusErr{code: http.StatusBadGateway, msg: "home returned invalid auth payload"}
	}
	updated.Index = authIndex
	updated.EnsureIndex()
	return &updated, true, nil
}

func statusFromHomeRefreshErrorCode(code string) int {
	switch strings.ToLower(strings.TrimSpace(code)) {
	case "authentication_error", "unauthorized":
		return http.StatusUnauthorized
	case "model_not_found":
		return http.StatusNotFound
	default:
		return http.StatusBadGateway
	}
}
