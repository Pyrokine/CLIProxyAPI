package diff_test

import (
	"reflect"
	"strings"
	"testing"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/watcher/diff"
)

func TestDiffOAuthSettingsChanges(t *testing.T) {
	oldMap := map[string][]config.OAuthModelSetting{
		"codex":  {{Name: "gpt-6-sol", MaxContextLength: 272000}},
		"vertex": {{Name: "gemini-2.5-pro", MaxContextLength: 1048576}},
	}
	newMap := map[string][]config.OAuthModelSetting{
		"codex":  {{Name: "gpt-6-sol", MaxContextLength: 524288}},
		"claude": {{Name: "claude-sonnet-4-5-20250929", MaxContextLength: 200000}},
	}
	changes, affected := diff.OAuthSettingsChanges(oldMap, newMap)
	want := []string{
		"oauth-settings[claude]: added (1 entries)",
		"oauth-settings[codex]: updated (1 -> 1 entries)",
		"oauth-settings[vertex]: removed",
	}
	if !reflect.DeepEqual(changes, want) || !reflect.DeepEqual(affected, []string{"claude", "codex", "vertex"}) {
		t.Fatalf("changes=%v affected=%v", changes, affected)
	}
}

func TestDiffOAuthSettingsChangesReordering(t *testing.T) {
	first := config.OAuthModelSetting{Name: "gpt-6-sol", MaxContextLength: 524288}
	second := config.OAuthModelSetting{Name: "deepseek-v4-flash", MaxContextLength: 1048576}
	changes, affected := diff.OAuthSettingsChanges(
		map[string][]config.OAuthModelSetting{"codex": {first, second}},
		map[string][]config.OAuthModelSetting{"codex": {second, first}},
	)
	if !reflect.DeepEqual(changes, []string{"oauth-settings[codex]: updated (2 -> 2 entries)"}) ||
		!reflect.DeepEqual(affected, []string{"codex"}) {
		t.Fatalf("rule reorder was ignored: changes=%v affected=%v", changes, affected)
	}
}

func TestSummarizeOAuthSettingsCompatibilityAndIsolation(t *testing.T) {
	settings := []config.OAuthModelSetting{
		{Name: " Model ", Alias: " Alias ", MaxContextLength: 8192},
		{Name: "model", Alias: "alias", MaxContextLength: 8192},
		{Name: " ", MaxContextLength: 10},
	}
	before := append([]config.OAuthModelSetting(nil), settings...)
	entries := map[string][]config.OAuthModelSetting{" CODEX ": settings, " ": {{Name: "ignored"}}, "vertex": nil}
	var summaries = diff.SummarizeOAuthSettings(entries)
	if len(summaries) != 2 || !reflect.DeepEqual(entries[" CODEX "], before) {
		t.Fatalf("summary changed inputs or channels: summaries=%v entries=%v", summaries, entries)
	}
	changes, affected := diff.OAuthSettingsChanges(nil, entries)
	if !reflect.DeepEqual(
		changes, []string{"oauth-settings[codex]: added (1 entries)", "oauth-settings[vertex]: added (0 entries)"},
	) ||
		!reflect.DeepEqual(affected, []string{"codex", "vertex"}) {
		t.Fatalf("normalized legacy input changed: changes=%v affected=%v", changes, affected)
	}
	if diff.SummarizeOAuthSettings(nil) != nil ||
		diff.SummarizeOAuthSettings(map[string][]config.OAuthModelSetting{" ": nil}) != nil {
		t.Fatal("empty summaries must remain nil")
	}
	if changes, affected := diff.OAuthSettingsChanges(entries, entries); len(changes) != 0 || len(affected) != 0 {
		t.Fatalf("unchanged rules reported differences: %v %v", changes, affected)
	}
}

func TestOAuthSettingsSameCountChangesReachConfigDiagnostics(t *testing.T) {
	for _, field := range []string{"model", "alias", "context"} {
		t.Run(
			field, func(t *testing.T) {
				previous := config.OAuthModelSetting{
					Name: "private-model", Alias: "private-alias", MaxContextLength: 8192,
				}
				current := previous
				switch field {
				case "model":
					current.Name = "changed-private-model"
				case "alias":
					current.Alias = "changed-private-alias"
				case "context":
					current.MaxContextLength = 16384
				}
				oldCfg := &config.Config{OAuthSettings: map[string][]config.OAuthModelSetting{"codex": {previous}}}
				newCfg := &config.Config{OAuthSettings: map[string][]config.OAuthModelSetting{"codex": {current}}}
				changes := diff.BuildConfigChangeDetails(oldCfg, newCfg)
				if !strings.Contains(strings.Join(changes, "\n"), "oauth-settings[codex]: updated (1 -> 1 entries)") {
					t.Fatalf("same-count channel change was not diagnosed: %v", changes)
				}
				for _, secret := range []string{
					"private-model", "private-alias", "changed-private-model", "changed-private-alias",
				} {
					if strings.Contains(strings.Join(changes, "\n"), secret) {
						t.Fatalf("diagnostic disclosed a model or alias: %v", changes)
					}
				}
			},
		)
	}
}
