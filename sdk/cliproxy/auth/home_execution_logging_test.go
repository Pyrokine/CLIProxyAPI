package auth

import (
	"context"
	"strings"
	"testing"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/logging"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executionregistry"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
	log "github.com/sirupsen/logrus"
	logtest "github.com/sirupsen/logrus/hooks/test"
)

func TestHomeOriginalSelectedOAuthDiagnostic(t *testing.T) {
	previous := log.GetLevel()
	log.SetLevel(log.DebugLevel)
	hook := logtest.NewLocal(log.StandardLogger())
	defer func() { hook.Reset(); log.SetLevel(previous) }()
	for _, mode := range []string{"execute", "count", "stream"} {
		t.Run(
			mode, func(t *testing.T) {
				hook.Reset()
				manager := NewManager(nil, nil, nil)
				manager.SetConfig(&config.Config{Home: config.HomeConfig{Enabled: true}})
				manager.PublishHomeDispatch(
					&accountedHomeExecutionDispatcher{
						auths: []Auth{
							{
								ID: "home-auth", Provider: "home-execution", Status: StatusActive,
								FileName:   "/synthetic-private-directory/home-auth",
								ProxyURL:   "socks5://synthetic-proxy-user:synthetic-proxy-secret@127.0.0.1:1080",
								Attributes: map[string]string{"auth_kind": "oauth"},
								Metadata:   map[string]any{"access_token": "synthetic-access-secret"},
							},
						},
					}, executionregistry.New(), 1,
				)
				manager.RegisterExecutor(
					&homeExecutionExecutor{
						id:      "home-execution",
						execute: func(context.Context, *Auth, executor.Request, executor.Options) error { return nil },
					},
				)
				ctx := logging.WithRequestID(t.Context(), "req-home-log")
				req := executor.Request{Model: "model-a"}
				var err error
				switch mode {
				case "execute":
					_, err = manager.Execute(ctx, []string{"home-execution"}, req, executor.Options{})
				case "count":
					_, err = manager.ExecuteCount(ctx, []string{"home-execution"}, req, executor.Options{})
				case "stream":
					var result *executor.StreamResult
					result, err = manager.ExecuteStream(
						ctx,
						[]string{"home-execution"},
						req,
						executor.Options{},
					)
					if err == nil {
						readOpenAICompatStreamPayload(t, result)
					}
				}
				if err != nil {
					t.Fatal(err)
				}
				matches := 0
				for _, entry := range hook.AllEntries() {
					if entry.Level == log.DebugLevel &&
						entry.Message ==
							("Use OAuth provider=home-execution auth_file=home-auth for model "+
								"model-a via socks5 "+
								"proxy") {
						matches++
						if entry.Data["request_id"] != "req-home-log" {
							t.Fatalf("selected OAuth request ID=%v", entry.Data["request_id"])
						}
					}
					text, err := entry.String()
					if err != nil {
						t.Fatal(err)
					}
					for _, secret := range []string{
						"synthetic-private-directory", "synthetic-proxy-user", "synthetic-proxy-secret",
						"synthetic-access-secret",
					} {
						if strings.Contains(text, secret) {
							t.Fatalf("selected OAuth diagnostic exposed %s", secret)
						}
					}
				}
				if matches != 1 {
					t.Fatalf("selected OAuth diagnostics=%d, want 1", matches)
				}
			},
		)
	}
}
