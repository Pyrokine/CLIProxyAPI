package auth

import (
	"errors"
	"net/http"
	"slices"
	"testing"
	"time"

	core "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
)

func TestCandidateCauseContractsMatchSelectorAndScheduler(t *testing.T) {
	now := time.Now()
	modelError := &Error{Code: "model-fixture", Message: "model cause", HTTPStatus: 429}
	otherError := &Error{Code: "other-fixture", Message: "other cause", HTTPStatus: 404}
	credentialError := &Error{Code: "credential-fixture", Message: "credential cause", HTTPStatus: 401}
	candidate := func(id string, at time.Time, state *ModelState) *Auth {
		a := &Auth{
			ID: id, Provider: "gemini", UpdatedAt: at, Unavailable: true,
			NextRetryAfter: now.Add(time.Minute), LastError: credentialError,
			StateEffectScope: core.StateEffectScopeCredential,
		}
		if state != nil {
			a.ModelStates = map[string]*ModelState{"candidate-model": state}
		}
		return a
	}
	for _, tc := range []struct {
		name, model string
		auths       []*Auth
		want        error
	}{
		{
			name: "model before newer credential", model: "candidate-model",
			auths: []*Auth{
				candidate(
					"model", now.Add(-time.Hour), &ModelState{LastError: modelError, UpdatedAt: now.Add(-time.Hour)},
				), candidate("credential", now, nil),
			},
			want: modelError,
		},
		{
			name: "latest model observation", model: "candidate-model",
			auths: []*Auth{
				candidate("z", now, &ModelState{LastError: otherError, UpdatedAt: now.Add(-time.Minute)}),
				candidate("a", now, &ModelState{LastError: modelError, UpdatedAt: now}),
			},
			want: modelError,
		},
		{
			name: "auth ID resolves equal observations", model: "candidate-model",
			auths: []*Auth{
				candidate("z", now, &ModelState{LastError: modelError, UpdatedAt: now}),
				candidate("a", now, &ModelState{LastError: otherError, UpdatedAt: now}),
			},
			want: modelError,
		},
		{
			name: "missing model uses credential", model: "unrelated-model",
			auths: []*Auth{candidate("a", now, &ModelState{LastError: modelError, UpdatedAt: now})},
			want:  credentialError,
		},
		{
			name: "thinking suffix matches same model", model: "candidate-model(high)",
			auths: []*Auth{candidate("a", now, &ModelState{LastError: modelError, UpdatedAt: now})},
			want:  modelError,
		},
		{
			name: "missing observation uses credential time", model: "candidate-model",
			auths: []*Auth{
				candidate("z", now.Add(-time.Hour), &ModelState{LastError: otherError}),
				candidate("a", now, &ModelState{LastError: modelError}),
			},
			want: modelError,
		},
	} {
		t.Run(
			tc.name, func(t *testing.T) {
				for _, provider := range []string{"gemini", "antigravity"} {
					for _, a := range tc.auths {
						a.Provider = provider
					}
					for _, reverse := range []bool{false, true} {
						if reverse {
							slices.Reverse(tc.auths)
						}
						for _, a := range tc.auths {
							if blocked, _, _ := isAuthBlockedForModel(a, tc.model, now); !blocked {
								t.Fatalf("fixture auth %s is available", a.ID)
							}
						}
						_, err := getAvailableAuths(tc.auths, provider, tc.model, core.Options{}, now)
						if !errors.Is(err, tc.want) {
							t.Fatalf(
								"selector(provider=%s, reverse=%v) cause = %v, want %v",
								provider,
								reverse,
								err,
								tc.want,
							)
						}
						scheduler := &modelScheduler{entries: make(map[string]*scheduledAuth)}
						for _, a := range tc.auths {
							scheduler.entries[a.ID] = &scheduledAuth{auth: a}
						}
						if got := scheduler.latestCandidateErrorWithTimeLocked(tc.model, nil); !errors.Is(
							got.err, tc.want,
						) {
							t.Fatalf(
								"scheduler(provider=%s, reverse=%v) cause = %v, want %v", provider, reverse, got.err,
								tc.want,
							)
						}
					}
				}
			},
		)
	}
}

func TestCandidateCauseStatusMessagesAndCredentialTieBreak(t *testing.T) {
	now := time.Now()
	for _, tc := range []struct{ name, model, want string }{
		{"model message", "candidate-model", "model status"},
		{"credential tie", "unrelated-model", "credential z"},
	} {
		t.Run(
			tc.name, func(t *testing.T) {
				auths := []*Auth{
					{
						ID: "z", StatusMessage: "credential z", UpdatedAt: now, ModelStates: map[string]*ModelState{
							"candidate-model": {
								StatusMessage: "model status", UpdatedAt: now,
							},
						},
					},
					{ID: "a", StatusMessage: "credential a", UpdatedAt: now},
				}
				err := latestSelectionCandidateError(auths, tc.model)
				if err == nil || err.Error() != tc.want {
					t.Fatalf("cause = %v, want %q", err, tc.want)
				}
			},
		)
	}
	cause := &Error{HTTPStatus: http.StatusUnauthorized}
	base := &Error{Code: "auth_unavailable", HTTPStatus: http.StatusServiceUnavailable}
	if got := WithCause(
		base, cause,
	).(interface{ StatusCode() int }).StatusCode(); got != http.StatusServiceUnavailable {
		t.Fatalf("explicit base status = %d", got)
	}
}
