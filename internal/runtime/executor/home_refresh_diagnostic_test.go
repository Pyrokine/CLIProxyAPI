package executor

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/clienterror"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/registry"
	cliproxyauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
	log "github.com/sirupsen/logrus"
	logtest "github.com/sirupsen/logrus/hooks/test"
)

func TestHomeRefreshDiagnosticSeparatesPublicResponse(t *testing.T) {
	for _, test := range []struct {
		name, payload               string
		cause                       error
		wantStatus                  int
		wantMessage, wantDiagnostic string
	}{
		{"transport", "", errors.New("dial hidden-value: connection refused"), 503, "home refresh temporarily unavailable", "Home refresh transport failed: dial_failed connection_refused"},
		{"decode envelope", "x hidden-value", nil, 502, "home returned invalid auth payload", "Home refresh response decode failed: invalid_response_json"},
		{"decode auth", `{"auth":17}`, nil, 502, "home returned invalid auth payload", "Home refresh response decode failed: invalid_response_json"},
		{"legacy type", `{"error":{"type":"AUTH_UNAVAILABLE","message":"hidden-value"}}`, nil, 503, "credential refresh temporarily unavailable", "Home refresh failed: type=auth_unavailable"},
		{"legacy code", `{"error":{"code":"invalid_grant","message":"hidden-value"}}`, nil, 401, "credential unauthorized", "Home refresh failed: type=invalid_grant"},
		{"unknown type", `{"error":{"type":"hidden-value","message":"hidden-value"}}`, nil, 503, "credential refresh temporarily unavailable", "credential refresh temporarily unavailable"},
		{"dedicated diagnostic", `{"error":{"type":"auth_unavailable","message":"hidden-value","diagnostic":"refresh failed: stage=transport err=EOF"}}`, nil, 503, "credential refresh temporarily unavailable", "refresh failed: stage=transport err=EOF"},
	} {
		t.Run(test.name, func(t *testing.T) {
			client := &testHomeRefreshClient{heartbeat: true, payload: []byte(test.payload), err: test.cause}
			ctx := cliproxyauth.WithHomeRefreshClient(context.Background(), client)
			_, handled, err := refreshAuthViaHome(ctx, &config.Config{Home: config.HomeConfig{Enabled: true}}, &cliproxyauth.Auth{ID: "synthetic", Index: "synthetic", Provider: "codex"})
			if !handled || err == nil || clienterror.HTTPStatusFromError(err) != test.wantStatus || err.Error() != test.wantMessage {
				t.Fatalf("handled=%v error=%v", handled, err)
			}
			var diagnostic interface{ LogDiagnostic() string }
			if !errors.As(err, &diagnostic) || diagnostic.LogDiagnostic() != test.wantDiagnostic || strings.Contains(diagnostic.LogDiagnostic(), "hidden-value") {
				t.Fatalf("diagnostic=%v, want %q", diagnostic, test.wantDiagnostic)
			}
			if _, direct := clienterror.ResponseBodyFromError(err); direct {
				t.Fatal("control error became direct response")
			}
		})
	}
}

func TestHomeRefreshUpstreamLogDoesNotReuseBody(t *testing.T) {
	for _, body := range []string{"", "arbitrary hidden-value\n"} {
		for _, detail := range []string{"", "stage=refresh Authorization: Bearer hidden-value"} {
			raw, err := json.Marshal(homeRefreshErrorEnvelope{Error: &homeRefreshErrorDetail{Diagnostic: detail, Upstream: &homeRefreshUpstreamResponse{Status: 401, Body: []byte(body)}}})
			if err != nil {
				t.Fatal(err)
			}
			ctx := cliproxyauth.WithHomeRefreshClient(context.Background(), &testHomeRefreshClient{heartbeat: true, payload: raw})
			_, _, err = refreshAuthViaHome(ctx, &config.Config{Home: config.HomeConfig{Enabled: true}}, &cliproxyauth.Auth{ID: "synthetic", Index: "synthetic", Provider: "codex"})
			if err == nil || err.Error() != body {
				t.Fatalf("response error=%v, want exact body", err)
			}
			response, direct := clienterror.ResponseBodyFromError(err)
			if !direct || string(response) != body {
				t.Fatalf("direct=%v response=%q", direct, response)
			}
			diagnostic := err.(interface{ LogDiagnostic() string }).LogDiagnostic()
			if strings.Contains(diagnostic, "hidden-value") || strings.ContainsAny(diagnostic, "\r\n") {
				t.Fatalf("unsafe diagnostic=%q", diagnostic)
			}
			if detail == "" && diagnostic != "Home refresh upstream response: status=401" {
				t.Fatalf("diagnostic=%q", diagnostic)
			}
		}
	}
	for _, cause := range []error{context.Canceled, context.DeadlineExceeded} {
		ctx := cliproxyauth.WithHomeRefreshClient(context.Background(), &testHomeRefreshClient{heartbeat: true, err: cause})
		_, _, err := refreshAuthViaHome(ctx, &config.Config{Home: config.HomeConfig{Enabled: true}}, &cliproxyauth.Auth{ID: "synthetic", Index: "synthetic"})
		if !errors.Is(err, cause) {
			t.Fatalf("context error=%v", err)
		}
	}
}

type homeDiagnosticClient struct{ *testHomeRefreshClient }

func (*homeDiagnosticClient) RPopAuth(context.Context, string, string, http.Header, int) ([]byte, error) {
	return nil, errors.New("unexpected Home credential selection")
}

type homeDiagnosticPreparer struct{ cliproxyauth.ProviderExecutor }

func (*homeDiagnosticPreparer) Identifier() string                               { return "home-diagnostic" }
func (*homeDiagnosticPreparer) ShouldPrepareRequestAuth(*cliproxyauth.Auth) bool { return true }
func (*homeDiagnosticPreparer) PrepareRequestAuth(ctx context.Context, auth *cliproxyauth.Auth) (*cliproxyauth.Auth, error) {
	updated, _, err := refreshAuthViaHome(ctx, &config.Config{Home: config.HomeConfig{Enabled: true}}, auth)
	return updated, err
}

func TestHomeRefreshDiagnosticReachesPreparationWarning(t *testing.T) {
	logger := log.StandardLogger()
	hooks, level, output := logger.ReplaceHooks(make(log.LevelHooks)), logger.GetLevel(), logger.Out
	logger.SetOutput(io.Discard)
	logger.SetLevel(log.WarnLevel)
	hook := logtest.NewLocal(logger)
	t.Cleanup(func() { logger.ReplaceHooks(hooks); logger.SetLevel(level); logger.SetOutput(output) })
	manager := cliproxyauth.NewManager(nil, nil, nil)
	manager.RegisterExecutor(&homeDiagnosticPreparer{})
	credential, err := manager.Register(context.Background(), &cliproxyauth.Auth{ID: "synthetic-log-auth", Provider: "home-diagnostic", Metadata: map[string]any{"access_token": "synthetic"}})
	if err != nil {
		t.Fatal(err)
	}
	registry.GetGlobalRegistry().RegisterClient(credential.ID, credential.Provider, []*registry.ModelInfo{{ID: "synthetic-model"}})
	t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(credential.ID) })
	client := &homeDiagnosticClient{&testHomeRefreshClient{heartbeat: true, err: errors.New("dial hidden-value: connection refused")}}
	manager.SetHomeClientProvider(func() cliproxyauth.HomeClient { return client })
	_, err = manager.Execute(context.Background(), []string{credential.Provider}, cliproxyexecutor.Request{Model: "synthetic-model"}, cliproxyexecutor.Options{})
	if err == nil || strings.Contains(err.Error(), "hidden-value") {
		t.Fatalf("public error=%v", err)
	}
	found := 0
	for _, entry := range hook.AllEntries() {
		if entry.Message != "upstream execution failed" {
			continue
		}
		found++
		message, _ := entry.Data["error"].(string)
		if !strings.Contains(message, "dial_failed connection_refused") || strings.Contains(message, "hidden-value") || entry.Data["status"] != 503 {
			t.Fatalf("warning=%v", entry.Data)
		}
	}
	if found != 1 {
		t.Fatalf("warnings=%d, want 1", found)
	}
}
