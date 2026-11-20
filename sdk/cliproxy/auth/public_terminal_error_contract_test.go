package auth_test

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/api/handlers"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
)

// noinspection GoDirectComparisonOfErrors
func TestPublicTerminalAuthenticationErrorPreservesCauseAndClassification(t *testing.T) {
	cause := errors.New("refresh_token_reused api_key=SYNTHETIC_TERMINAL_SECRET")
	base := &auth.Error{
		Code: "auth_unavailable", Message: "no auth available", HTTPStatus: http.StatusServiceUnavailable,
	}
	wrapped := auth.NewTerminalAuthError(base, cause)
	outer := fmt.Errorf("outer: %w", wrapped)
	var got *auth.Error
	if !auth.IsTerminalAuthError(outer) || !errors.Is(outer, cause) || !errors.Is(outer, base) || !errors.As(
		outer, &got,
	) || got != base {
		t.Fatalf("terminal error lost identity: %v", outer)
	}
	if status := wrapped.(interface{ StatusCode() int }).StatusCode(); status != http.StatusServiceUnavailable {
		t.Fatalf("terminal status=%d", status)
	}
	encoded, err := json.Marshal(wrapped)
	if err != nil || strings.Contains(string(encoded), "SYNTHETIC_TERMINAL_SECRET") || strings.Contains(
		string(encoded), "refresh_token_reused",
	) {
		t.Fatalf("terminal JSON=%s, error=%v", encoded, err)
	}
	body := handlers.BuildErrorResponseBodyWithError(http.StatusServiceUnavailable, cause.Error(), outer)
	var response handlers.ErrorResponse
	if err = json.Unmarshal(
		body, &response,
	); err !=
		nil ||
		response.Error.Code !=
			"upstream_authentication_required" ||
		response.Error.Type !=
			("authentication"+
				"_error") ||
		response.Error.Retryable ==
			nil ||
		*response.Error.Retryable {
		t.Fatalf("terminal response=%s, error=%v", body, err)
	}
	if strings.Contains(string(body), "SYNTHETIC_TERMINAL_SECRET") || !strings.Contains(
		response.Error.Message, "refresh_token_reused",
	) || !strings.Contains(response.Error.Message, "[REDACTED]") {
		t.Fatalf("terminal response reason/privacy=%s", body)
	}
	if auth.IsTerminalAuthError(nil) || auth.IsTerminalAuthError(
		auth.WithCause(
			base, cause,
		),
	) || auth.IsTerminalAuthError(auth.NewModelCooldownError("m", "p", 0)) || auth.NewTerminalAuthError(
		nil, cause,
	) != nil {
		t.Fatal("unmarked errors received terminal classification")
	}
	if withoutCause := auth.NewTerminalAuthError(
		base, nil,
	); !auth.IsTerminalAuthError(withoutCause) || withoutCause.Error() != base.Error() {
		t.Fatalf("terminal error without cause=%v", withoutCause)
	}
	plain := handlers.BuildErrorResponseBody(http.StatusUnauthorized, "invalid local API key")
	plainWithError := handlers.BuildErrorResponseBodyWithError(
		http.StatusUnauthorized, "invalid local API key", errors.New("ordinary failure"),
	)
	if string(plain) != string(plainWithError) {
		t.Fatalf("ordinary error changed: %s / %s", plain, plainWithError)
	}
}
