package management

import (
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	"github.com/gin-gonic/gin"
)

func TestMutationConfigurationTerminalCompletionAndValidation(t *testing.T) {
	for _, entry := range []struct {
		name, body, outcome string
		status              int
		callback, fail      bool
	}{
		{"runtime_acknowledged", `{"value":true}`, "succeeded", 200, true, false},
		{"runtime_unavailable", `{"value":true}`, "uncertain", 200, false, false},
		{"runtime_failed", `{"value":true}`, "partial", 503, true, true},
		{"missing_value", `{}`, "rejected", 400, true, false},
		{"invalid_value", `{"value":"bad"}`, "rejected", 400, true, false},
	} {
		t.Run(
			entry.name, func(t *testing.T) {
				path := filepath.Join(t.TempDir(), "config.yaml")
				if err := os.WriteFile(path, []byte("port: 8317\ndebug: false\n"), 0o600); err != nil {
					t.Fatal(err)
				}
				h := &Handler{cfg: &config.Config{Port: 8317}, configFilePath: path}
				calls := 0
				if entry.callback {
					h.configCommitted = func(_ context.Context, commit ConfigCommit) error {
						calls++
						if !commit.Config.Debug {
							t.Fatal("runtime callback did not receive requested configuration")
						}
						if entry.fail {
							return errors.New("synthetic runtime rejection")
						}
						return nil
					}
				}
				engine := resourceMutationEngine(t, h)
				engine.PUT("/v0/management/debug", h.PutDebug)
				var kinds []string
				if entry.status != 400 {
					kinds = []string{"configuration.persisted", "configuration.published"}
					if entry.callback && !entry.fail {
						kinds = append(kinds, "configuration.runtime_applied")
					}
				}
				callResourceMutation(
					t,
					engine,
					http.MethodPut,
					"/v0/management/debug",
					entry.body,
					entry.status,
					entry.outcome,
					kinds...,
				)
				if entry.status == 400 && (calls != 0 || h.configSnapshot().Debug) {
					t.Fatal("validation rejection changed configuration")
				}
			},
		)
	}
}

func TestMutationConfigurationRejectsYAMLAndLocalFieldValidation(t *testing.T) {
	for _, entry := range []struct {
		name, path, body string
		status           int
		handler          func(*Handler) gin.HandlerFunc
	}{
		{
			"yaml", "/v0/management/config.yaml", "port: [bad]\n", 422,
			func(h *Handler) gin.HandlerFunc { return h.PutConfigYAML },
		},
		{
			"yaml_validation", "/v0/management/config/validate", "port: [bad]\n", 200,
			func(h *Handler) gin.HandlerFunc { return h.ValidateConfigYAML },
		},
		{
			"routing", "/v0/management/routing/strategy", `{"value":"unknown"}`, 400,
			func(h *Handler) gin.HandlerFunc { return h.PutRoutingStrategy },
		},
		{
			"proxy", "/v0/management/proxy-url", `{"value":"ftp://synthetic.invalid"}`, 403,
			func(h *Handler) gin.HandlerFunc { return h.PutProxyURL },
		},
	} {
		t.Run(
			entry.name, func(t *testing.T) {
				path := filepath.Join(t.TempDir(), "config.yaml")
				before := []byte("port: 8317\n")
				if err := os.WriteFile(path, before, 0o600); err != nil {
					t.Fatal(err)
				}
				h := &Handler{cfg: &config.Config{Port: 8317}, configFilePath: path}
				engine := resourceMutationEngine(t, h)
				engine.PUT(entry.path, entry.handler(h))
				callResourceMutation(t, engine, http.MethodPut, entry.path, entry.body, entry.status, "rejected")
				after, err := os.ReadFile(path)
				if err != nil || string(after) != string(before) {
					t.Fatalf("validation changed configuration: %q err=%v", after, err)
				}
			},
		)
	}
}
