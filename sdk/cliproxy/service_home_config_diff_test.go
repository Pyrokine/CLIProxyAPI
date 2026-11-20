package cliproxy

import (
	"bytes"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/home"
	log "github.com/sirupsen/logrus"
)

func TestHomeOverlayEmitsRedactedModelAndConfigurationChanges(t *testing.T) {
	parse := func(secret, display string, retention, maxContext int) *config.Config {
		t.Helper()
		body := fmt.Sprintf(
			"{\n\t\t\t\"debug\":true,\"redis-usage-queue-retention-seconds\":%d,\n"+
				`			"gemini-api-key":[{"api-key":%q,"models":[{"name":"fixture-model","`+
				"display-name\":%q,\"max-context-length\":%d}]}],\n\t\t\t\"interactions"+
				"-api-key\":[{\"api-key\":%q,\"headers\":{\"Authorization\":%q}}],\n\t"+
				`		"oauth-settings":{"codex":[{"name":"fixture-oauth-model","alias":%q,`+
				("\"max-context-length\":%d}]}\n\t\t"+
					"}"), retention, secret, display, maxContext, secret, secret, secret, maxContext,
		)
		cfg, err := config.ParseConfigBytes([]byte(body))
		if err != nil {
			t.Fatal(err)
		}
		return cfg
	}
	previous := parse("fixture-secret-previous", "Previous display", 60, 4096)
	previous.Home, _ = homeLifecycleRedis(t, nil)
	client := home.New(previous.Home)
	t.Cleanup(func() { client.Close() })
	current := parse("fixture-secret-current", "Current display", 180, 262144)
	service := &Service{cfg: previous, configPath: filepath.Join(t.TempDir(), "config.yaml")}

	logger := log.StandardLogger()
	output, level := logger.Out, logger.GetLevel()
	var entries bytes.Buffer
	logger.SetOutput(&entries)
	logger.SetLevel(log.DebugLevel)
	defer func() {
		logger.SetOutput(output)
		logger.SetLevel(level)
	}()
	if err := service.applyHomeOverlayWithClient(t.Context(), current, client); err != nil {
		t.Fatalf("apply Home config: %v", err)
	}
	for _, want := range []string{
		"home config changes detected:",
		"gemini[0].models: updated (1 -> 1 entries)",
		"redis-usage-queue-retention-seconds: 60 -> 180",
		"interactions[0].api-key: updated",
		"interactions[0].headers: updated",
		"oauth-settings[codex]: updated (1 -> 1 entries)",
	} {
		if !strings.Contains(entries.String(), want) {
			t.Errorf("Home callback log missing %q: %s", want, entries.String())
		}
	}
	for _, secret := range []string{"fixture-secret-previous", "fixture-secret-current"} {
		if strings.Contains(entries.String(), secret) {
			t.Errorf("Home callback exposed credential %q", secret)
		}
	}
	if service.cfg ==
		previous ||
		!service.cfg.Home.Enabled ||
		service.cfg.GeminiKey[0].Models[0].DisplayName !=
			"Current display" ||
		service.cfg.RedisUsageQueueRetentionSeconds != 180 ||
		len(service.cfg.OAuthSettings["codex"]) != 1 ||
		service.cfg.OAuthSettings["codex"][0].MaxContextLength != 262144 {
		t.Fatal("Home callback did not publish the changed configuration")
	}
}
