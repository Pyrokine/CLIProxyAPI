package auth

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"

	core "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
)

func TestOriginalIsCloudflareChallengeErrorMessage_ExcludesOriginErrors(t *testing.T) {
	origin520 := `<html><head><title>Web server is returning an unknown error</title></head><body><div class=` +
		`"cf-error-details cf-error-520"><h1>Web server is returning an unknown error</h1><p>There is an ` +
		`unknown connection issue between Cloudflare and the origin web server.</p><ul><li>Ray ID: ` +
		`a385507da9eeb4c4</li><li>Error reference number: 520</li><li>Cloudflare Location: Los Angeles</li></` +
		`ul></div></body></html>`
	if isCloudflareChallengeErrorMessage(origin520) {
		t.Fatalf("expected origin 520 error not to be classified as cloudflare challenge")
	}

	minimalOrigin := `<html><body><h1>Web server is returning an unknown error</h1><p>There is an unknown ` +
		`connection issue between Cloudflare and the origin web server.</p></body></html>`
	if isCloudflareChallengeErrorMessage(minimalOrigin) {
		t.Fatalf("expected minimal origin error not to be classified as cloudflare challenge")
	}

	// Legitimate challenge messages must still be recognized.
	challenges := []string{
		"cf-mitigated: challenge",
		`<html><body><script src="/cdn-cgi/challenge-platform/h/b/orchestrate/chl_page/v1"></script></body></html>`,
		"cloudflare challenge required",
		//goland:noinspection GrazieInspection
		// Isolated "Just a moment..." challenge page without an explicit challenge marker
		`<html><head><title>Just a moment...</title></head><body>Checking your browser... cloudflare</body></html>`,
	}
	for _, ch := range challenges {
		if !isCloudflareChallengeErrorMessage(ch) {
			t.Fatalf("expected %q to be classified as cloudflare challenge", ch)
		}
	}
}

func TestOriginalManager_MarkResult_Cloudflare520OriginError_NotTreatedAsChallenge(t *testing.T) {
	m := NewManager(nil, nil, nil)
	auth := &Auth{
		ID:       "auth-test-520",
		Provider: "codex",
		Status:   StatusActive,
	}
	registered, errRegister := m.Register(context.Background(), auth)
	if errRegister != nil {
		t.Fatal(errRegister)
	}
	auth = registered

	origin520Msg := `<html><body><div class="cf-error-details cf-error-520"><h1>Web server ` +
		`is returning an unknown error</h1><p>There is an unknown connection is` +
		`sue between Cloudflare and the origin web server.</p></div></body></ht` +
		`ml>`

	m.MarkResult(
		context.Background(), Result{
			AuthID:         auth.ID,
			AuthGeneration: auth.Generation,
			Provider:       "codex",
			Model:          "gpt-5.6-sol",
			Success:        false,
			Error: &Error{
				HTTPStatus: 520,
				Message:    origin520Msg,
			},
		},
	)

	updated, ok := m.GetByID(auth.ID)
	if !ok || updated == nil {
		t.Fatalf("expected auth to be found")
	}
	state := updated.ModelStates["gpt-5.6-sol"]
	if state == nil {
		t.Fatalf("expected model state to be present")
	}

	// Quota must not be marked as exceeded or challenged.
	if state.Quota.Exceeded {
		t.Fatalf("expected Quota.Exceeded to be false for 520 origin error")
	}
	if state.Quota.Reason != "" {
		t.Fatalf("expected Quota.Reason to be empty for 520 origin error, got %q", state.Quota.Reason)
	}
	if state.StatusMessage == "cloudflare challenge" {
		t.Fatalf("expected StatusMessage not to be 'cloudflare challenge' for 520 origin error")
	}
	if state.StatusMessage != origin520Msg {
		t.Fatalf("expected StatusMessage to preserve upstream error, got %q", state.StatusMessage)
	}

	// LastError must retain the 520 error.
	if state.LastError == nil || state.LastError.HTTPStatus != 520 {
		t.Fatalf("expected LastError to retain HTTP 520, got %#v", state.LastError)
	}

	// By default, transient errors apply a 1-minute retry window.
	if !state.Unavailable {
		t.Fatalf("expected model to be marked unavailable during transient cooldown")
	}
	diff := time.Until(state.NextRetryAfter)
	if diff < 45*time.Second || diff > 75*time.Second {
		t.Fatalf("expected default transient cooldown of ~60s, got %v", diff)
	}
}

func TestOriginalManager_MarkResult_Cloudflare520_CustomTransientCooldown(t *testing.T) {
	prevTransient := transientErrorCooldownSeconds.Load()
	transientErrorCooldownSeconds.Store(5)
	t.Cleanup(func() { transientErrorCooldownSeconds.Store(prevTransient) })

	m := NewManager(nil, nil, nil)
	auth := &Auth{
		ID:       "auth-test-520-custom-cooldown",
		Provider: "codex",
		Status:   StatusActive,
	}
	registered, errRegister := m.Register(context.Background(), auth)
	if errRegister != nil {
		t.Fatal(errRegister)
	}
	auth = registered

	origin520Msg := `<html><body><div class="cf-error-details cf-error-520"><h1>Web server ` +
		`is returning an unknown error</h1></div></body></html>`

	m.MarkResult(
		context.Background(), Result{
			AuthID:         auth.ID,
			AuthGeneration: auth.Generation,
			Provider:       "codex",
			Model:          "gpt-5.6-sol",
			Success:        false,
			Error: &Error{
				HTTPStatus: 520,
				Message:    origin520Msg,
			},
		},
	)

	updated, ok := m.GetByID(auth.ID)
	if !ok || updated == nil {
		t.Fatalf("expected auth to be found")
	}
	state := updated.ModelStates["gpt-5.6-sol"]
	if state == nil {
		t.Fatalf("expected model state to be present")
	}

	diff := time.Until(state.NextRetryAfter)
	if diff < 3*time.Second || diff > 7*time.Second {
		t.Fatalf("expected custom transient cooldown of ~5s, got %v", diff)
	}
}

func TestOriginalManager_MarkResult_Cloudflare520_DisabledTransientCooldown(t *testing.T) {
	prevTransient := transientErrorCooldownSeconds.Load()
	transientErrorCooldownSeconds.Store(-1)
	t.Cleanup(func() { transientErrorCooldownSeconds.Store(prevTransient) })

	m := NewManager(nil, nil, nil)
	auth := &Auth{
		ID:       "auth-test-520-disabled-cooldown",
		Provider: "codex",
		Status:   StatusActive,
	}
	registered, errRegister := m.Register(context.Background(), auth)
	if errRegister != nil {
		t.Fatal(errRegister)
	}
	auth = registered

	origin520Msg := `<html><body><div class="cf-error-details cf-error-520"><h1>Web server ` +
		`is returning an unknown error</h1></div></body></html>`

	m.MarkResult(
		context.Background(), Result{
			AuthID:         auth.ID,
			AuthGeneration: auth.Generation,
			Provider:       "codex",
			Model:          "gpt-5.6-sol",
			Success:        false,
			Error: &Error{
				HTTPStatus: 520,
				Message:    origin520Msg,
			},
		},
	)

	updated, ok := m.GetByID(auth.ID)
	if !ok || updated == nil {
		t.Fatalf("expected auth to be found")
	}
	state := updated.ModelStates["gpt-5.6-sol"]
	if state == nil {
		t.Fatalf("expected model state to be present")
	}

	if !state.NextRetryAfter.IsZero() {
		t.Fatalf(
			"expected NextRetryAfter to be zero when transient cooldown disabled, got %v",
			state.NextRetryAfter,
		)
	}
	if state.Unavailable {
		t.Fatalf("expected model not to be unavailable when cooldown is disabled")
	}
}

func TestOriginalManager_MarkResult_Cloudflare520_DisableCoolingAuth(t *testing.T) {
	m := NewManager(nil, nil, nil)
	auth := &Auth{
		ID:       "auth-test-520-disable-cooling-auth",
		Provider: "codex",
		Status:   StatusActive,
		Metadata: map[string]any{
			"disable_cooling": true,
		},
	}
	registered, errRegister := m.Register(context.Background(), auth)
	if errRegister != nil {
		t.Fatal(errRegister)
	}
	auth = registered

	origin520Msg := `<html><body><div class="cf-error-details cf-error-520"><h1>Web server ` +
		`is returning an unknown error</h1></div></body></html>`

	m.MarkResult(
		context.Background(), Result{
			AuthID:         auth.ID,
			AuthGeneration: auth.Generation,
			Provider:       "codex",
			Model:          "gpt-5.6-sol",
			Success:        false,
			Error: &Error{
				HTTPStatus: 520,
				Message:    origin520Msg,
			},
		},
	)

	updated, ok := m.GetByID(auth.ID)
	if !ok || updated == nil {
		t.Fatalf("expected auth to be found")
	}
	state := updated.ModelStates["gpt-5.6-sol"]
	if state == nil {
		t.Fatalf("expected model state to be present")
	}

	if !state.NextRetryAfter.IsZero() {
		t.Fatalf(
			"expected NextRetryAfter to be zero when disable_cooling is true, got %v",
			state.NextRetryAfter,
		)
	}
	if state.Unavailable {
		t.Fatalf("expected model not to be unavailable when disable_cooling is true")
	}
}

func TestOriginalManager_MarkResult_Cloudflare520_WithRetryAfterHint(t *testing.T) {
	m := NewManager(nil, nil, nil)
	auth := &Auth{
		ID:       "auth-test-520-hint",
		Provider: "codex",
		Status:   StatusActive,
	}
	registered, errRegister := m.Register(context.Background(), auth)
	if errRegister != nil {
		t.Fatal(errRegister)
	}
	auth = registered

	origin520Msg := `<html><body><div class="cf-error-details cf-error-520"><h1>Web server ` +
		`is returning an unknown error</h1></div></body></html>`
	hint := 15 * time.Second

	m.MarkResult(
		context.Background(), Result{
			AuthID:         auth.ID,
			AuthGeneration: auth.Generation,
			Provider:       "codex",
			Model:          "gpt-5.6-sol",
			Success:        false,
			RetryAfter:     &hint,
			Error: &Error{
				HTTPStatus: 520,
				Message:    origin520Msg,
			},
		},
	)

	updated, ok := m.GetByID(auth.ID)
	if !ok || updated == nil {
		t.Fatalf("expected auth to be found")
	}
	state := updated.ModelStates["gpt-5.6-sol"]
	if state == nil {
		t.Fatalf("expected model state to be present")
	}

	diff := time.Until(state.NextRetryAfter)
	if diff < 10*time.Second || diff > 20*time.Second {
		t.Fatalf("expected hint-based cooldown of ~15s, got %v", diff)
	}
}

func TestOriginalManager_MarkResult_Cloudflare520_DisabledTransientCooldown_IgnoresHint(t *testing.T) {
	prevTransient := transientErrorCooldownSeconds.Load()
	transientErrorCooldownSeconds.Store(-1)
	t.Cleanup(func() { transientErrorCooldownSeconds.Store(prevTransient) })

	m := NewManager(nil, nil, nil)
	auth := &Auth{
		ID:       "auth-test-520-disabled-with-hint",
		Provider: "codex",
		Status:   StatusActive,
	}
	registered, errRegister := m.Register(context.Background(), auth)
	if errRegister != nil {
		t.Fatal(errRegister)
	}
	auth = registered

	origin520Msg := `<html><body><div class="cf-error-details cf-error-520"><h1>Web server ` +
		`is returning an unknown error</h1></div></body></html>`
	hint := 15 * time.Second

	m.MarkResult(
		context.Background(), Result{
			AuthID:         auth.ID,
			AuthGeneration: auth.Generation,
			Provider:       "codex",
			Model:          "gpt-5.6-sol",
			Success:        false,
			RetryAfter:     &hint,
			Error: &Error{
				HTTPStatus: 520,
				Message:    origin520Msg,
			},
		},
	)

	updated, ok := m.GetByID(auth.ID)
	if !ok || updated == nil {
		t.Fatalf("expected auth to be found")
	}
	state := updated.ModelStates["gpt-5.6-sol"]
	if state == nil {
		t.Fatalf("expected model state to be present")
	}

	if !state.NextRetryAfter.IsZero() {
		t.Fatalf(
			"expected NextRetryAfter to be zero when transient cooldown is "+
				"disabled (-1) despite RetryAfter hint, got "+
				"%v",
			state.NextRetryAfter,
		)
	}
	if state.Unavailable {
		t.Fatalf("expected model not to be unavailable when transient cooldown is disabled")
	}
}

func TestOriginalManager_MarkResult_AuthLevelCloudflare520_DisabledTransientCooldown_IgnoresHint(t *testing.T) {
	prevTransient := transientErrorCooldownSeconds.Load()
	transientErrorCooldownSeconds.Store(-1)
	t.Cleanup(func() { transientErrorCooldownSeconds.Store(prevTransient) })

	m := NewManager(nil, nil, nil)
	auth := &Auth{
		ID:       "auth-test-520-auth-disabled-with-hint",
		Provider: "codex",
		Status:   StatusActive,
	}
	registered, errRegister := m.Register(context.Background(), auth)
	if errRegister != nil {
		t.Fatal(errRegister)
	}
	auth = registered

	origin520Msg := `<html><body><div class="cf-error-details cf-error-520"><h1>Web server ` +
		`is returning an unknown error</h1></div></body></html>`
	hint := 15 * time.Second

	m.MarkResult(
		context.Background(), Result{
			AuthID:         auth.ID,
			AuthGeneration: auth.Generation,
			Provider:       "codex",
			Model:          "",
			Success:        false,
			RetryAfter:     &hint,
			Error: &Error{
				HTTPStatus: 520,
				Message:    origin520Msg,
			},
		},
	)

	updated, ok := m.GetByID(auth.ID)
	if !ok || updated == nil {
		t.Fatalf("expected auth to be found")
	}

	if !updated.NextRetryAfter.IsZero() {
		t.Fatalf(
			"expected auth NextRetryAfter to be zero when transient cooldown is "+
				"disabled (-1) despite RetryAfter hint, got "+
				"%v",
			updated.NextRetryAfter,
		)
	}
	if updated.Unavailable {
		t.Fatalf("expected auth not to be unavailable when transient cooldown is disabled")
	}
}

func TestCloudflareOriginal503PreservesProviderScopedUnknownOutcome(t *testing.T) {
	previous := transientErrorCooldownSeconds.Load()
	t.Cleanup(func() { transientErrorCooldownSeconds.Store(previous) })
	for _, tc := range []struct {
		name, authID string
		cooldown     int
	}{
		{"disabled", "auth-test-503-disabled", -1},
		{"hint", "auth-test-503-hint", 0},
	} {
		t.Run(
			tc.name, func(t *testing.T) {
				SetTransientErrorCooldownSeconds(tc.cooldown)
				hook := &resultCaptureHook{}
				manager := NewManager(nil, nil, hook)
				auth, err := manager.Register(
					t.Context(), &Auth{ID: tc.authID, Provider: "claude", Status: StatusActive},
				)
				if err != nil {
					t.Fatal(err)
				}
				hint := 20 * time.Second
				manager.MarkResult(
					t.Context(), Result{
						AuthID: auth.ID, AuthGeneration: auth.Generation, Provider: "claude",
						Model: "claude-3-5-sonnet",
						Error: &Error{HTTPStatus: 503, Message: "service unavailable"}, RetryAfter: &hint,
					},
				)
				current, _ := manager.GetByID(auth.ID)
				if current.Unavailable || !current.NextRetryAfter.IsZero() || current.Failed != 1 {
					t.Fatal(
						"unknown provider outcome changed credential availability or lost its failure statistic",
					)
				}
				if state := current.ModelStates["claude-3-5-sonnet"]; state != nil &&
					(state.Unavailable || !state.NextRetryAfter.IsZero()) {
					t.Fatal("provider outcome created an independent model cooldown")
				}
				results := hook.Results()
				if len(results) != 1 {
					t.Fatalf("recorded results = %d, want 1", len(results))
				}
				result := results[0]
				if result.AuthID != auth.ID || result.AuthGeneration != auth.Generation ||
					result.Disposition.Outcome != core.ExecutionOutcomeUnknown ||
					result.Disposition.RetryDirective != core.RetryDirectiveNoRetry ||
					result.Disposition.EffectScope != core.StateEffectScopeProvider ||
					result.Disposition.EffectKey !=
						"claude" ||
					result.RetryAfter ==
						nil ||
					*result.RetryAfter !=
						hint ||
					result.Error ==
						nil ||
					result.Error.HTTPStatus !=
						503 ||
					result.Error.Message !=
						"service unavailable" {
					t.Fatalf("provider outcome lost original error, wait hint or authority: %#v", result)
				}
			},
		)
	}
}

func TestOriginalManager_MarkResult_AuthLevelCloudflare520_SetsTransientError(t *testing.T) {
	m := NewManager(nil, nil, nil)
	//goland:noinspection SpellCheckingInspection
	auth := &Auth{
		ID:       "auth-test-520-authlevel",
		Provider: "codex",
		Status:   StatusActive,
	}
	registered, errRegister := m.Register(context.Background(), auth)
	if errRegister != nil {
		t.Fatal(errRegister)
	}
	auth = registered

	origin520Msg := `<html><body><div class="cf-error-details cf-error-520"><h1>Web server ` +
		`is returning an unknown error</h1><p>There is an unknown connection is` +
		`sue between Cloudflare and the origin web server.</p></div></body></ht` +
		`ml>`

	m.MarkResult(
		context.Background(), Result{
			AuthID:         auth.ID,
			AuthGeneration: auth.Generation,
			Provider:       "codex",
			Model:          "",
			Success:        false,
			Error: &Error{
				HTTPStatus: 520,
				Message:    origin520Msg,
			},
		},
	)

	updated, ok := m.GetByID(auth.ID)
	if !ok || updated == nil {
		t.Fatalf("expected auth to be found")
	}
	if updated.Quota.Exceeded {
		t.Fatalf("expected Quota.Exceeded to be false for 520 origin error")
	}
	if updated.Quota.Reason != "" {
		t.Fatalf("expected Quota.Reason to be empty for 520 origin error, got %q", updated.Quota.Reason)
	}
	if updated.StatusMessage != "transient upstream error" {
		t.Fatalf("expected StatusMessage to be 'transient upstream error', got %q", updated.StatusMessage)
	}
	if updated.LastError == nil || updated.LastError.HTTPStatus != 520 {
		t.Fatalf("expected LastError to retain HTTP 520, got %#v", updated.LastError)
	}
	if !updated.Unavailable {
		t.Fatalf("expected auth to be marked unavailable during transient cooldown")
	}
	diff := time.Until(updated.NextRetryAfter)
	if diff < 45*time.Second || diff > 75*time.Second {
		t.Fatalf("expected default transient cooldown of ~60s, got %v", diff)
	}
}

func TestOriginalIsCloudflareChallengeResultError_Excludes5xx(t *testing.T) {
	// Status >= 500 origin errors must not be classified as challenges even if containing challenge strings.
	for _, status := range []int{500, 502, 503, 504, 520, 521, 522, 523, 524, 525, 526} {
		err5xx := &Error{
			HTTPStatus: status,
			Message:    "cf-mitigated: challenge but with 5xx status code",
		}
		if isCloudflareChallengeResultError(err5xx) {
			t.Fatalf("expected %d status code to be excluded from cloudflare challenge", status)
		}
	}

	err403 := &Error{
		HTTPStatus: 403,
		Message:    "cf-mitigated: challenge",
	}
	if !isCloudflareChallengeResultError(err403) {
		t.Fatalf("expected 403 challenge to be recognized")
	}
}

// noinspection GoErrorStringFormat
func TestOriginalIsCloudflareChallengeError(t *testing.T) {
	// 520 error wrapped in standard error interface must not be treated as challenge.
	err520 := &Error{
		HTTPStatus: 520,
		Message:    "cf-error-details cf-error-520: Web server is returning an unknown error",
	}
	if isCloudflareChallengeError(err520) {
		t.Fatalf("expected 520 error not to be cloudflare challenge")
	}

	// 403 challenge error must be recognized.
	err403 := &Error{
		HTTPStatus: http.StatusForbidden,
		Message:    "cf-mitigated: challenge",
	}
	if !isCloudflareChallengeError(err403) {
		t.Fatalf("expected 403 challenge error to be recognized")
	}

	// Plain error without status but with challenge message must be recognized.
	plainChallenge := errors.New("challenge-platform orchestrate script blocked")
	if !isCloudflareChallengeError(plainChallenge) {
		t.Fatalf("expected plain challenge error to be recognized")
	}

	// Plain origin error without challenge markers must not be recognized.
	plainOrigin := errors.New("Web server is returning an unknown error between Cloudflare and origin")
	if isCloudflareChallengeError(plainOrigin) {
		t.Fatalf("expected plain origin error not to be recognized as challenge")
	}
}

func TestCloudflareOriginalOriginFailureStopsEveryManagerEntry(t *testing.T) {
	for _, mode := range []string{"execute", "count", "stream"} {
		t.Run(
			mode, func(t *testing.T) {
				cause := &Error{
					HTTPStatus: 520,
					Message: `<html><body><div class="cf-error-details cf-error-520"><h1>Web server ` +
						`is returning an unknown error</h1><p>There is an unknown connection is` +
						`sue between Cloudflare and the origin web server.</p></div></body></ht` +
						`ml>`,
				}
				capture := &requestScopedTestExecutor{provider: "codex"}
				capture.execute = func(*Auth) (core.Response, error) { return core.Response{}, cause }
				capture.count = capture.execute
				capture.stream = func(*Auth) (*core.StreamResult, error) { return nil, cause }
				hook := &resultCaptureHook{}
				manager := NewManager(nil, &FillFirstSelector{}, hook)
				manager.SetHomeClientProvider(func() HomeClient { return nil })
				manager.SetRetryConfig(3, 30*time.Second, 0)
				manager.RegisterExecutor(capture)
				for _, id := range []string{"origin-a-" + mode, "origin-b-" + mode} {
					registerRequestScopedTestAuth(
						t, manager, &Auth{ID: id, Provider: "codex", Status: StatusActive}, "gpt-5.6-sol",
					)
				}
				request := core.Request{Model: "gpt-5.6-sol"}
				var err error
				switch mode {
				case "execute":
					_, err = manager.Execute(t.Context(), []string{"codex"}, request, core.Options{})
				case "count":
					_, err = manager.ExecuteCount(t.Context(), []string{"codex"}, request, core.Options{})
				case "stream":
					_, err = manager.ExecuteStream(
						t.Context(),
						[]string{"codex"},
						request,
						core.Options{Stream: true},
					)
				}
				calls := capture.calls.Load() + capture.countCalls.Load() + capture.streamCalls.Load()
				if !errors.Is(err, cause) || calls != 1 {
					t.Fatalf("origin error=%v calls=%d, want the original failure and one attempt", err, calls)
				}
				results := hook.Results()
				if len(results) != 1 || results[0].Disposition.Outcome != core.ExecutionOutcomeFailed ||
					results[0].Disposition.RetryDirective != core.RetryDirectiveNoRetry ||
					results[0].Disposition.EffectScope !=
						core.StateEffectScopeExactModel ||
					results[0].Model !=
						request.Model {
					t.Fatalf("origin result lost its terminal outcome or model scope: %#v", results)
				}
			},
		)
	}
}
