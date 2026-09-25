package auth_test

import (
	"errors"
	"net/http"
	"testing"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/clienterror"
	cliproxyauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
)

type causeStatusError struct {
	status int
}

func (e causeStatusError) Error() string   { return http.StatusText(e.status) }
func (e causeStatusError) StatusCode() int { return e.status }

func TestWithCausePreservesCauseHTTPStatusWhenBaseHasNone(t *testing.T) {
	cause := causeStatusError{status: http.StatusTooManyRequests}
	err := cliproxyauth.WithCause(
		&cliproxyauth.Error{Code: "auth_unavailable", Message: "no auth available"},
		cause,
	)

	if got := clienterror.HTTPStatusFromErrorOr(err, http.StatusInternalServerError); got != cause.status {
		t.Fatalf("HTTP status = %d, want %d", got, cause.status)
	}
	if !errors.Is(err, cause) {
		t.Fatal("wrapped error no longer unwraps to the upstream cause")
	}
}

func TestWithCausePrefersExplicitBaseHTTPStatus(t *testing.T) {
	err := cliproxyauth.WithCause(
		&cliproxyauth.Error{
			Code:       "auth_unavailable",
			Message:    "no auth available",
			HTTPStatus: http.StatusServiceUnavailable,
		},
		causeStatusError{status: http.StatusTooManyRequests},
	)

	if got := clienterror.HTTPStatusFromError(err); got != http.StatusServiceUnavailable {
		t.Fatalf("HTTP status = %d, want %d", got, http.StatusServiceUnavailable)
	}
}
