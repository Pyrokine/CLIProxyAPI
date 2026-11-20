package management

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"testing"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	coreauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
)

func TestMutationOAuthStartQueueAndCancellation(t *testing.T) {
	const marker = "CPA_TEST_MUTATION_OAUTH_CHILD"
	if os.Getenv(marker) != "1" {
		command := exec.CommandContext(
			t.Context(), os.Args[0], "-test.run=^TestMutationOAuthStartQueueAndCancellation$", "-test.count=1",
		)
		command.Env = []string{marker + "=1"}
		if output, err := command.CombinedOutput(); err != nil {
			t.Fatalf("isolated OAuth receipt consumer: %v\n%s", err, output)
		}
		return
	}
	starts, exchanges := 0, 0
	newDevinOAuthService = func(*config.Config) devinOAuthService {
		starts++
		return &fakeDevinOAuthService{
			exchange: func(context.Context, string, string) (string, error) {
				exchanges++
				return "", errors.New("synthetic exchange must remain unused")
			},
			create: func(context.Context, string) (*coreauth.Auth, error) {
				return nil, errors.New("synthetic credential creation must remain unused")
			},
		}
	}
	dir := t.TempDir()
	h := &Handler{cfg: &config.Config{Port: 8317, AuthDir: dir}}
	engine := resourceMutationEngine(t, h)
	engine.GET("/v0/management/devin-auth-url", h.RequestDevinToken)
	engine.DELETE("/v0/management/oauth-session", h.CancelAuthSession)
	engine.GET("/v0/management/get-auth-status", h.GetAuthStatus)
	engine.GET("/v0/management/mutations/:mutation_id", h.GetMutationResult)
	const identity = "11111111-2222-4333-8444-555555555555"
	start := func() *httptest.ResponseRecorder {
		request := httptest.NewRequest(http.MethodGet, "/v0/management/devin-auth-url?nonce=synthetic-nonce", nil)
		request.RemoteAddr = "127.0.0.1:12345"
		request.Header.Set(MutationIDHeader, identity)
		response := httptest.NewRecorder()
		engine.ServeHTTP(response, request)
		return response
	}
	response := start()
	result := decodeMutationHeader(t, response)
	if response.Code != 200 || result.Outcome != "succeeded" || len(result.Effects) != 1 ||
		result.Effects[0].Kind != "task.created" {
		t.Fatalf("OAuth creation claimed more than accepted task: status=%d result=%+v", response.Code, result)
	}
	var payload struct {
		State string `json:"state"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &payload); err != nil || payload.State == "" {
		t.Fatalf("missing actual OAuth session: %v", err)
	}
	defer cancelOAuthSession(payload.State)
	if replay := start(); replay.Code != http.StatusConflict || starts != 1 {
		t.Fatalf("OAuth identity replayed: status=%d starts=%d", replay.Code, starts)
	}
	callResourceMutation(
		t, engine, http.MethodDelete, "/v0/management/oauth-session?state="+payload.State+"&nonce=synthetic-nonce", "",
		200, "succeeded", "task.runtime_applied",
	)
	callResourceMutation(
		t, engine, http.MethodDelete, "/v0/management/oauth-session?state="+payload.State+"&nonce=synthetic-nonce", "",
		200, "uncertain",
	)
	callResourceMutation(t, engine, http.MethodDelete, "/v0/management/oauth-session", "", 400, "rejected")
	readback := httptest.NewRecorder()
	engine.ServeHTTP(readback, httptest.NewRequest(http.MethodGet, "/v0/management/mutations/"+identity, nil))
	var persisted MutationResult
	if err := json.Unmarshal(
		readback.Body.Bytes(), &persisted,
	); err != nil || len(persisted.Effects) != 1 || persisted.Effects[0].Kind != "task.created" {
		t.Fatalf("async receipt changed into credential success: %s err=%v", readback.Body.String(), err)
	}
	if readback.Header().Get(MutationResultHeader) != "" {
		t.Fatal("read-only mutation lookup created a mutation")
	}
	files, err := os.ReadDir(dir)
	if err != nil || len(files) != 0 || exchanges != 0 {
		t.Fatalf(
			"start/cancel attempted exchange or persistence: files=%d exchanges=%d err=%v", len(files), exchanges, err,
		)
	}
}

func TestMutationIFlowCookieValidationRejectsWithoutAuthentication(t *testing.T) {
	h := &Handler{cfg: &config.Config{AuthDir: t.TempDir()}}
	engine := resourceMutationEngine(t, h)
	engine.POST("/v0/management/iflow-auth-url", h.RequestIFlowCookieToken)
	for _, body := range []string{"{", `{}`, `{"cookie":""}`} {
		callResourceMutation(t, engine, http.MethodPost, "/v0/management/iflow-auth-url", body, 400, "rejected")
	}
}
