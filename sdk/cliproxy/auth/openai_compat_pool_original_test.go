package auth

import (
	"context"
	"errors"
	"net/http"
	"testing"

	internalconfig "github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	cliproxyexecutor "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
)

func TestOriginalManagerExecuteCount_OpenAICompatAliasPoolStopsOnInvalidRequest(t *testing.T) {
	alias := "claude-opus-4.66"
	invalidErr := &Error{HTTPStatus: http.StatusUnprocessableEntity, Message: "unprocessable entity"}
	executor := &openAICompatPoolExecutor{
		id:          "pool",
		countErrors: map[string]error{"deepseek-v3.1": invalidErr},
	}
	m := newOpenAICompatPoolTestManager(
		t, alias, []internalconfig.OpenAICompatibilityModel{
			{Name: "deepseek-v3.1", Alias: alias},
			{Name: "glm-5", Alias: alias},
		}, executor,
	)

	_, err := m.ExecuteCount(
		context.Background(), []string{"pool"}, cliproxyexecutor.Request{Model: alias}, cliproxyexecutor.Options{},
	)
	if err == nil || err.Error() != invalidErr.Error() {
		t.Fatalf("execute count error = %v, want %v", err, invalidErr)
	}
	got := executor.CountModels()
	if len(got) != 1 || got[0] != "deepseek-v3.1" {
		t.Fatalf("count calls = %v, want only first invalid model", got)
	}
}

func TestOriginalManagerExecute_OpenAICompatAliasPoolRotatesWithinAuth(t *testing.T) {
	alias := "claude-opus-4.66"
	executor := &openAICompatPoolExecutor{id: "pool"}
	m := newOpenAICompatPoolTestManager(
		t, alias, []internalconfig.OpenAICompatibilityModel{
			{Name: "deepseek-v3.1", Alias: alias},
			{Name: "glm-5", Alias: alias},
		}, executor,
	)

	for i := range 3 {
		resp, err := m.Execute(
			context.Background(), []string{"pool"}, cliproxyexecutor.Request{Model: alias}, cliproxyexecutor.Options{},
		)
		if err != nil {
			t.Fatalf("execute %d: %v", i, err)
		}
		if len(resp.Payload) == 0 {
			t.Fatalf("execute %d returned empty payload", i)
		}
	}

	got := executor.ExecuteModels()
	want := []string{"deepseek-v3.1", "glm-5", "deepseek-v3.1"}
	if len(got) != len(want) {
		t.Fatalf("execute calls = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("execute call %d model = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestOriginalManagerExecute_OpenAICompatAliasPoolForceMappingRotatesAndRewritesResponse(t *testing.T) {
	alias := "claude-opus-4.66"
	executor := &openAICompatPoolExecutor{
		id: "pool",
		executePayloads: map[string][]byte{
			"deepseek-v3.1": []byte(`{"model":"deepseek-v3.1"}`),
			"glm-5":         []byte(`{"model":"glm-5"}`),
		},
	}
	m := newOpenAICompatPoolTestManager(
		t, alias, []internalconfig.OpenAICompatibilityModel{
			{Name: "deepseek-v3.1", Alias: alias, ForceMapping: true},
			{Name: "glm-5", Alias: alias, ForceMapping: true},
		}, executor,
	)

	var payloads []string
	for i := range 2 {
		resp, err := m.Execute(
			context.Background(), []string{"pool"}, cliproxyexecutor.Request{Model: alias}, cliproxyexecutor.Options{},
		)
		if err != nil {
			t.Fatalf("execute %d: %v", i, err)
		}
		payloads = append(payloads, string(resp.Payload))
	}

	got := executor.ExecuteModels()
	wantModels := []string{"deepseek-v3.1", "glm-5"}
	for i := range wantModels {
		if got[i] != wantModels[i] {
			t.Fatalf("execute call %d model = %q, want %q", i, got[i], wantModels[i])
		}
	}
	wantPayloads := []string{`{"model":"claude-opus-4.66"}`, `{"model":"claude-opus-4.66"}`}
	for i := range wantPayloads {
		if payloads[i] != wantPayloads[i] {
			t.Fatalf("payload %d = %s, want %s", i, payloads[i], wantPayloads[i])
		}
	}
}

func TestOriginalManagerExecute_OpenAICompatAliasPoolStopsOnBadRequest(t *testing.T) {
	alias := "claude-opus-4.66"
	invalidErr := &Error{HTTPStatus: http.StatusBadRequest, Message: "invalid_request_error: malformed payload"}
	executor := &openAICompatPoolExecutor{
		id:            "pool",
		executeErrors: map[string]error{"deepseek-v3.1": invalidErr},
	}
	m := newOpenAICompatPoolTestManager(
		t, alias, []internalconfig.OpenAICompatibilityModel{
			{Name: "deepseek-v3.1", Alias: alias},
			{Name: "glm-5", Alias: alias},
		}, executor,
	)

	_, err := m.Execute(
		context.Background(), []string{"pool"}, cliproxyexecutor.Request{Model: alias}, cliproxyexecutor.Options{},
	)
	if err == nil || err.Error() != invalidErr.Error() {
		t.Fatalf("execute error = %v, want %v", err, invalidErr)
	}
	got := executor.ExecuteModels()
	if len(got) != 1 || got[0] != "deepseek-v3.1" {
		t.Fatalf("execute calls = %v, want only first invalid model", got)
	}
}

func TestOriginalManagerExecute_OpenAICompatAliasPoolFallsBackOnModelSupportBadRequest(t *testing.T) {
	alias := "claude-opus-4.66"
	modelSupportErr := &Error{
		HTTPStatus: http.StatusBadRequest,
		Message:    "invalid_request_error: The requested model is not supported.",
	}
	executor := &openAICompatPoolExecutor{
		id:            "pool",
		executeErrors: map[string]error{"deepseek-v3.1": modelSupportErr},
	}
	m := newOpenAICompatPoolTestManager(
		t, alias, []internalconfig.OpenAICompatibilityModel{
			{Name: "deepseek-v3.1", Alias: alias},
			{Name: "glm-5", Alias: alias},
		}, executor,
	)

	resp, err := m.Execute(
		context.Background(), []string{"pool"}, cliproxyexecutor.Request{Model: alias}, cliproxyexecutor.Options{},
	)
	if err != nil {
		t.Fatalf("execute error = %v, want fallback success", err)
	}
	if string(resp.Payload) != "glm-5" {
		t.Fatalf("payload = %q, want %q", string(resp.Payload), "glm-5")
	}
	got := executor.ExecuteModels()
	want := []string{"deepseek-v3.1", "glm-5"}
	if len(got) != len(want) {
		t.Fatalf("execute calls = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("execute call %d model = %q, want %q", i, got[i], want[i])
		}
	}

	updated, ok := m.GetByID("pool-auth-" + t.Name())
	if !ok || updated == nil {
		t.Fatalf("expected auth to remain registered")
	}
	state := updated.ModelStates["deepseek-v3.1"]
	if state == nil {
		t.Fatalf("expected suspended upstream model state")
	}
	if !state.Unavailable || state.EffectKey != "deepseek-v3.1" || state.ResetAuthority != StateResetAuthoritySuccess {
		t.Fatalf("expected target suspension until a same-generation success, got %+v", state)
	}
}

func TestOriginalManagerExecute_OpenAICompatAliasPoolFallsBackOnModelSupportUnprocessableEntity(t *testing.T) {
	alias := "claude-opus-4.66"
	modelSupportErr := &Error{
		HTTPStatus: http.StatusUnprocessableEntity,
		Message:    "The requested model is not supported.",
	}
	executor := &openAICompatPoolExecutor{
		id:            "pool",
		executeErrors: map[string]error{"deepseek-v3.1": modelSupportErr},
	}
	m := newOpenAICompatPoolTestManager(
		t, alias, []internalconfig.OpenAICompatibilityModel{
			{Name: "deepseek-v3.1", Alias: alias},
			{Name: "glm-5", Alias: alias},
		}, executor,
	)

	resp, err := m.Execute(
		context.Background(), []string{"pool"}, cliproxyexecutor.Request{Model: alias}, cliproxyexecutor.Options{},
	)
	if err != nil {
		t.Fatalf("execute error = %v, want fallback success", err)
	}
	if string(resp.Payload) != "glm-5" {
		t.Fatalf("payload = %q, want %q", string(resp.Payload), "glm-5")
	}
	got := executor.ExecuteModels()
	want := []string{"deepseek-v3.1", "glm-5"}
	if len(got) != len(want) {
		t.Fatalf("execute calls = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("execute call %d model = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestOriginalManagerExecute_OpenAICompatAliasPoolFallsBackWithinSameAuth(t *testing.T) {
	alias := "claude-opus-4.66"
	executor := &openAICompatPoolExecutor{
		id: "pool",
		executeErrors: map[string]error{
			"deepseek-v3.1": &Error{
				HTTPStatus: http.StatusTooManyRequests, Message: "quota",
			},
		},
	}
	m := newOpenAICompatPoolTestManager(
		t, alias, []internalconfig.OpenAICompatibilityModel{
			{Name: "deepseek-v3.1", Alias: alias},
			{Name: "glm-5", Alias: alias},
		}, executor,
	)

	resp, err := m.Execute(
		context.Background(), []string{"pool"}, cliproxyexecutor.Request{Model: alias}, cliproxyexecutor.Options{},
	)
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if string(resp.Payload) != "glm-5" {
		t.Fatalf("payload = %q, want %q", string(resp.Payload), "glm-5")
	}
	got := executor.ExecuteModels()
	want := []string{"deepseek-v3.1", "glm-5"}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("execute call %d model = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestOriginalManagerExecute_OpenAICompatAliasPoolUsesSelectedModelForceMapping(t *testing.T) {
	alias := "public-model"
	executor := &openAICompatPoolExecutor{
		id: "pool",
		executeErrors: map[string]error{
			"first-upstream": &Error{
				HTTPStatus: http.StatusTooManyRequests, Message: "quota",
			},
		},
		executePayloads: map[string][]byte{"second-upstream": []byte(`{"model":"second-upstream"}`)},
	}
	manager := newOpenAICompatPoolTestManager(
		t, alias, []internalconfig.OpenAICompatibilityModel{
			{Name: "first-upstream", Alias: alias, ForceMapping: true},
			{Name: "second-upstream", Alias: alias},
		}, executor,
	)

	response, errExecute := manager.Execute(
		context.Background(), []string{"pool"}, cliproxyexecutor.Request{Model: alias}, cliproxyexecutor.Options{},
	)
	if errExecute != nil {
		t.Fatalf("Execute() error = %v", errExecute)
	}
	if got := string(response.Payload); got != `{"model":"second-upstream"}` {
		t.Fatalf("payload = %s, want selected model without force mapping", got)
	}
}

func TestOriginalManagerExecuteStream_OpenAICompatAliasPoolFallsBackBeforeFirstByte(t *testing.T) {
	alias := "claude-opus-4.66"
	executor := &openAICompatPoolExecutor{
		id: "pool",
		streamFirstErrors: map[string]error{
			"deepseek-v3.1": &Error{
				HTTPStatus: http.StatusTooManyRequests, Message: "quota",
			},
		},
	}
	m := newOpenAICompatPoolTestManager(
		t, alias, []internalconfig.OpenAICompatibilityModel{
			{Name: "deepseek-v3.1", Alias: alias},
			{Name: "glm-5", Alias: alias},
		}, executor,
	)

	streamResult, err := m.ExecuteStream(
		context.Background(), []string{"pool"}, cliproxyexecutor.Request{Model: alias}, cliproxyexecutor.Options{},
	)
	if err != nil {
		t.Fatalf("execute stream: %v", err)
	}
	var payload []byte
	for chunk := range streamResult.Chunks {
		if chunk.Err != nil {
			t.Fatalf("unexpected stream error: %v", chunk.Err)
		}
		payload = append(payload, chunk.Payload...)
	}
	if string(payload) != "glm-5" {
		t.Fatalf("payload = %q, want %q", string(payload), "glm-5")
	}
	got := executor.StreamModels()
	want := []string{"deepseek-v3.1", "glm-5"}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("stream call %d model = %q, want %q", i, got[i], want[i])
		}
	}
	if gotHeader := streamResult.Headers.Get("X-Model"); gotHeader != "glm-5" {
		t.Fatalf("header X-Model = %q, want %q", gotHeader, "glm-5")
	}
}

func TestOriginalManagerExecuteStream_OpenAICompatAliasPoolStopsOnInvalidRequest(t *testing.T) {
	alias := "claude-opus-4.66"
	invalidErr := &Error{HTTPStatus: http.StatusUnprocessableEntity, Message: "unprocessable entity"}
	executor := &openAICompatPoolExecutor{
		id:                "pool",
		streamFirstErrors: map[string]error{"deepseek-v3.1": invalidErr},
	}
	m := newOpenAICompatPoolTestManager(
		t, alias, []internalconfig.OpenAICompatibilityModel{
			{Name: "deepseek-v3.1", Alias: alias},
			{Name: "glm-5", Alias: alias},
		}, executor,
	)

	streamResult, err := m.ExecuteStream(
		context.Background(), []string{"pool"}, cliproxyexecutor.Request{Model: alias}, cliproxyexecutor.Options{},
	)
	if err == nil {
		for chunk := range streamResult.Chunks {
			if len(chunk.Payload) != 0 {
				t.Fatalf("invalid request returned payload=%q", chunk.Payload)
			}
			if chunk.Err != nil {
				err = chunk.Err
			}
		}
	}
	if !errors.Is(err, invalidErr) {
		t.Fatalf("execute stream error = %v, want %v", err, invalidErr)
	}
	got := executor.StreamModels()
	if len(got) != 1 || got[0] != "deepseek-v3.1" {
		t.Fatalf("stream calls = %v, want only first invalid model", got)
	}
}

func TestOriginalManagerExecute_OpenAICompatAliasPoolSkipsSuspendedUpstreamOnLaterRequests(t *testing.T) {
	alias := "claude-opus-4.66"
	modelSupportErr := &Error{
		HTTPStatus: http.StatusBadRequest,
		Message:    "invalid_request_error: The requested model is not supported.",
	}
	executor := &openAICompatPoolExecutor{
		id:            "pool",
		executeErrors: map[string]error{"deepseek-v3.1": modelSupportErr},
	}
	m := newOpenAICompatPoolTestManager(
		t, alias, []internalconfig.OpenAICompatibilityModel{
			{Name: "deepseek-v3.1", Alias: alias},
			{Name: "glm-5", Alias: alias},
		}, executor,
	)

	for i := range 3 {
		resp, err := m.Execute(
			context.Background(), []string{"pool"}, cliproxyexecutor.Request{Model: alias}, cliproxyexecutor.Options{},
		)
		if err != nil {
			t.Fatalf("execute %d: %v", i, err)
		}
		if string(resp.Payload) != "glm-5" {
			t.Fatalf("execute %d payload = %q, want %q", i, string(resp.Payload), "glm-5")
		}
	}

	got := executor.ExecuteModels()
	want := []string{"deepseek-v3.1", "glm-5", "glm-5", "glm-5"}
	if len(got) != len(want) {
		t.Fatalf("execute calls = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("execute call %d model = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestOriginalManagerExecuteStream_OpenAICompatAliasPoolSkipsSuspendedUpstreamOnLaterRequests(t *testing.T) {
	alias := "claude-opus-4.66"
	modelSupportErr := &Error{
		HTTPStatus: http.StatusUnprocessableEntity,
		Message:    "The requested model is not supported.",
	}
	executor := &openAICompatPoolExecutor{
		id:                "pool",
		streamFirstErrors: map[string]error{"deepseek-v3.1": modelSupportErr},
	}
	m := newOpenAICompatPoolTestManager(
		t, alias, []internalconfig.OpenAICompatibilityModel{
			{Name: "deepseek-v3.1", Alias: alias},
			{Name: "glm-5", Alias: alias},
		}, executor,
	)

	for i := range 3 {
		streamResult, err := m.ExecuteStream(
			context.Background(), []string{"pool"}, cliproxyexecutor.Request{Model: alias}, cliproxyexecutor.Options{},
		)
		if err != nil {
			t.Fatalf("execute stream %d: %v", i, err)
		}
		if payload := readOpenAICompatStreamPayload(t, streamResult); payload != "glm-5" {
			t.Fatalf("execute stream %d payload = %q, want %q", i, payload, "glm-5")
		}
		if gotHeader := streamResult.Headers.Get("X-Model"); gotHeader != "glm-5" {
			t.Fatalf("execute stream %d header X-Model = %q, want %q", i, gotHeader, "glm-5")
		}
	}

	got := executor.StreamModels()
	want := []string{"deepseek-v3.1", "glm-5", "glm-5", "glm-5"}
	if len(got) != len(want) {
		t.Fatalf("stream calls = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("stream call %d model = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestOriginalManagerExecuteCount_OpenAICompatAliasPoolRotatesWithinAuth(t *testing.T) {
	alias := "claude-opus-4.66"
	executor := &openAICompatPoolExecutor{id: "pool"}
	m := newOpenAICompatPoolTestManager(
		t, alias, []internalconfig.OpenAICompatibilityModel{
			{Name: "deepseek-v3.1", Alias: alias},
			{Name: "glm-5", Alias: alias},
		}, executor,
	)

	for i := range 2 {
		resp, err := m.ExecuteCount(
			context.Background(), []string{"pool"}, cliproxyexecutor.Request{Model: alias}, cliproxyexecutor.Options{},
		)
		if err != nil {
			t.Fatalf("execute count %d: %v", i, err)
		}
		if len(resp.Payload) == 0 {
			t.Fatalf("execute count %d returned empty payload", i)
		}
	}

	got := executor.CountModels()
	want := []string{"deepseek-v3.1", "glm-5"}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("count call %d model = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestOriginalManagerExecuteStream_OpenAICompatAliasPoolStopsOnInvalidBootstrap(t *testing.T) {
	alias := "claude-opus-4.66"
	invalidErr := &Error{HTTPStatus: http.StatusBadRequest, Message: "invalid_request_error: malformed payload"}
	executor := &openAICompatPoolExecutor{
		id:                "pool",
		streamFirstErrors: map[string]error{"deepseek-v3.1": invalidErr},
	}
	m := newOpenAICompatPoolTestManager(
		t, alias, []internalconfig.OpenAICompatibilityModel{
			{Name: "deepseek-v3.1", Alias: alias},
			{Name: "glm-5", Alias: alias},
		}, executor,
	)

	streamResult, err := m.ExecuteStream(
		context.Background(), []string{"pool"}, cliproxyexecutor.Request{Model: alias}, cliproxyexecutor.Options{},
	)
	if err == nil {
		for chunk := range streamResult.Chunks {
			if len(chunk.Payload) != 0 {
				t.Fatalf("invalid bootstrap returned payload=%q", chunk.Payload)
			}
			if chunk.Err != nil {
				err = chunk.Err
			}
		}
	}
	if !errors.Is(err, invalidErr) {
		t.Fatalf("error = %v, want %v", err, invalidErr)
	}
	if got := executor.StreamModels(); len(got) != 1 || got[0] != "deepseek-v3.1" {
		t.Fatalf("stream calls = %v, want only first upstream model", got)
	}
}
