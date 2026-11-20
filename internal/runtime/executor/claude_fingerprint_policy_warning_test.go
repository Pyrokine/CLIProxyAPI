package executor

import (
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/util"
	cliproxyauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
	log "github.com/sirupsen/logrus"
	logtest "github.com/sirupsen/logrus/hooks/test"
)

func captureClaudeProfileWarnings(t *testing.T) *logtest.Hook {
	t.Helper()
	logger := log.StandardLogger()
	oldHooks, oldLevel, oldOutput := logger.ReplaceHooks(log.LevelHooks{}), logger.GetLevel(), logger.Out
	hook := &logtest.Hook{}
	logger.AddHook(hook)
	logger.SetLevel(log.WarnLevel)
	logger.SetOutput(io.Discard)
	t.Cleanup(
		func() {
			logger.ReplaceHooks(oldHooks)
			logger.SetLevel(oldLevel)
			logger.SetOutput(oldOutput)
		},
	)
	return hook
}

func TestClaudeFingerprintWarningsAreDeduplicatedByInvalidValue(t *testing.T) {
	capture := captureClaudeProfileWarnings(t)
	cfg := &config.Config{}
	for _, raw := range []string{t.Name() + "-unknown-a", t.Name() + "-unknown-b"} {
		for _, source := range []string{"attributes", "canonical metadata", "legacy metadata", "whitespace"} {
			auth := fingerprintAuth(raw, "https://gateway.invalid")
			switch source {
			case "canonical metadata":
				delete(auth.Attributes, cliproxyauth.AttributeClaudeFingerprintProfile)
				auth.Metadata = map[string]any{cliproxyauth.AttributeClaudeFingerprintProfile: raw}
			case "legacy metadata":
				delete(auth.Attributes, cliproxyauth.AttributeClaudeFingerprintProfile)
				auth.Metadata = map[string]any{"fingerprint-profile": raw}
			case "whitespace":
				auth.Attributes[cliproxyauth.AttributeClaudeFingerprintProfile] = " " + raw + " "
			}
			for range 3 {
				policy := resolveClaudeFingerprintPolicy(cfg, auth, auth.Attributes["api_key"])
				if policy.AuthIsOAuthToken || policy.ProfileClaudeCodeCLI || policy.UseOAuthBetas {
					t.Fatalf("invalid profile changed caller-owned policy: %+v", policy)
				}
			}
			ctx := util.WithRoundTripper(
				t.Context(), claudeContractTransport(
					func(request *http.Request) (*http.Response, error) {
						body, err := io.ReadAll(request.Body)
						if err != nil {
							return nil, err
						}
						return claudeContractResponse(request, body), nil
					},
				),
			)
			if _, err := NewClaudeExecutor(cfg).Execute(
				ctx, auth,
				fingerprintRequest(fingerprintPayload), fingerprintOptions(),
			); err != nil {
				t.Fatalf("execute with caller-owned fallback: %v", err)
			}
		}
	}
	for _, profile := range []string{"", "claude-code-cli", " OAUTH-CLI "} {
		auth := fingerprintAuth(profile, "https://gateway.invalid")
		auth.Metadata = map[string]any{"fingerprint_profile": t.Name() + "-ignored-low-priority"}
		resolveClaudeFingerprintPolicy(cfg, auth, auth.Attributes["api_key"])
	}
	warnings := 0
	for _, entry := range capture.AllEntries() {
		if strings.HasPrefix(entry.Message, "invalid Claude fingerprint profile") {
			warnings++
		}
	}
	if warnings != 2 {
		t.Fatalf("profile warnings = %d, want one per distinct invalid value", warnings)
	}
}

func TestClaudeFingerprintWarningsAreAtomicAcrossConcurrentRequests(t *testing.T) {
	capture := captureClaudeProfileWarnings(t)
	auth := fingerprintAuth(t.Name()+"-unknown", "https://gateway.invalid")
	var wg sync.WaitGroup
	start := make(chan struct{})
	for range 64 {
		wg.Go(
			func() {
				<-start
				resolveClaudeFingerprintPolicy(&config.Config{}, auth, auth.Attributes["api_key"])
			},
		)
	}
	close(start)
	wg.Wait()
	warnings := 0
	for _, entry := range capture.AllEntries() {
		if strings.HasPrefix(entry.Message, "invalid Claude fingerprint profile") {
			warnings++
		}
	}
	if warnings != 1 {
		t.Fatalf("concurrent profile warnings = %d, want 1", warnings)
	}
}
