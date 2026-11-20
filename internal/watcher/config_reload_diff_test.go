package watcher

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	log "github.com/sirupsen/logrus"
)

func TestWatcherReloadEmitsRedactedModelAndConfigurationChanges(t *testing.T) {
	directory := t.TempDir()
	configPath := filepath.Join(directory, "config.yaml")
	authDir := filepath.Join(directory, "auth")
	if err := os.Mkdir(authDir, 0o700); err != nil {
		t.Fatal(err)
	}
	payload := func(secret, display string, retention, maxContext int) []byte {
		return []byte(fmt.Sprintf(
			"{\n\t\t\t\"auth-dir\":%q,\"debug\":true,\"redis-usage-queue-retention-seconds\":%d,\n\t\t\t\""+
				"gemini-api-key\":[{\"api-key\":%q,\"models\":[{\"name\":\"fixture-model\",\"display-name\":%"+
				"q,\"max-context-length\":%d}]}],\n\t\t\t\"interactions-api-key\":[{\"api-key\":%q,\""+
				"headers\":{\"Authorization\":%q}}],\n\t\t\t\"oauth-settings\":{\"codex\":[{\"name\":\""+
				"fixture-oauth-model\",\"alias\":%q,\"max-context-length\":%d}]}\n\t\t}", authDir,
			retention, secret, display, maxContext, secret, secret, secret, maxContext,
		))
	}
	before := payload("fixture-secret-previous", "Previous display", 60, 4096)
	after := payload("fixture-secret-current", "Current display", 180, 262144)
	previous, err := config.ParseConfigBytes(before)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(configPath, after, 0o600); err != nil {
		t.Fatal(err)
	}

	logger := log.StandardLogger()
	output, level := logger.Out, logger.GetLevel()
	var entries bytes.Buffer
	logger.SetOutput(&entries)
	logger.SetLevel(log.DebugLevel)
	defer func() {
		logger.SetOutput(output)
		logger.SetLevel(level)
	}()
	callbackCount := 0
	w := &Watcher{
		configPath: configPath,
		authDir:    authDir,
		reloadCallback: func(next *config.Config) bool {
			callbackCount++
			return len(next.GeminiKey) == 1 && next.GeminiKey[0].Models[0].MaxContextLength == 262144 &&
				next.RedisUsageQueueRetentionSeconds == 180 && len(next.OAuthSettings["codex"]) == 1 &&
				next.OAuthSettings["codex"][0].MaxContextLength == 262144
		},
	}
	w.SetConfig(previous)
	if !w.reloadConfig() || callbackCount != 1 {
		t.Fatalf("reload failed: callbacks=%d logs=%s", callbackCount, entries.String())
	}
	for _, want := range []string{
		"config changes detected:",
		"gemini[0].models: updated (1 -> 1 entries)",
		"redis-usage-queue-retention-seconds: 60 -> 180",
		"interactions[0].api-key: updated",
		"interactions[0].headers: updated",
		"oauth-settings[codex]: updated (1 -> 1 entries)",
	} {
		if !strings.Contains(entries.String(), want) {
			t.Errorf("reload log missing %q: %s", want, entries.String())
		}
	}
	for _, secret := range []string{"fixture-secret-previous", "fixture-secret-current"} {
		if strings.Contains(entries.String(), secret) {
			t.Errorf("reload log exposed credential %q", secret)
		}
	}
	if w.config == previous || w.config.GeminiKey[0].Models[0].DisplayName != "Current display" ||
		w.config.RedisUsageQueueRetentionSeconds != 180 {
		t.Fatal("accepted reload did not publish the changed configuration")
	}
}
