package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestOAuthSettingsFlatDecoding(t *testing.T) {
	for _, tc := range []struct {
		name   string
		raw    string
		decode func([]byte, any) error
		want   []OAuthModelSetting
	}{
		{name: "single YAML", raw: "oauth-settings:\n  codex:\n    - name: gpt-6-sol\n      max-context-length: 524288\n", decode: yaml.Unmarshal, want: []OAuthModelSetting{{Name: "gpt-6-sol", MaxContextLength: 524288}}},
		{name: "single JSON", raw: `{"oauth-settings":{"codex":[{"name":"gpt-6-sol","max-context-length":524288}]}}`, decode: json.Unmarshal, want: []OAuthModelSetting{{Name: "gpt-6-sol", MaxContextLength: 524288}}},
		{name: "multiple YAML", raw: "oauth-settings:\n  codex:\n    - name: gpt-6-sol\n      max-context-length: 524288\n    - name: deepseek-v4-flash\n      max-context-length: 1048576\n", decode: yaml.Unmarshal, want: []OAuthModelSetting{{Name: "gpt-6-sol", MaxContextLength: 524288}, {Name: "deepseek-v4-flash", MaxContextLength: 1048576}}},
		{name: "multiple JSON", raw: `{"oauth-settings":{"codex":[{"name":"gpt-6-sol","max-context-length":524288},{"name":"deepseek-v4-flash","max-context-length":1048576}]}}`, decode: json.Unmarshal, want: []OAuthModelSetting{{Name: "gpt-6-sol", MaxContextLength: 524288}, {Name: "deepseek-v4-flash", MaxContextLength: 1048576}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var cfg Config
			if err := tc.decode([]byte(tc.raw), &cfg); err != nil {
				t.Fatal(err)
			}
			cfg.SanitizeOAuthSettings()
			if len(cfg.OAuthSettings) != 1 || !slices.Equal(cfg.OAuthSettings["codex"], tc.want) {
				t.Fatalf("OAuth settings = %+v, want %+v", cfg.OAuthSettings, tc.want)
			}
		})
	}
}

func TestOAuthSettingsSanitizePreservesInterleavedRulePriority(t *testing.T) {
	cfg := &Config{OAuthSettings: map[string][]OAuthModelSetting{
		" CODEX ": {
			{Name: " upstream ", MaxContextLength: 524288},
			{Name: "public", MaxContextLength: 600000},
			{Name: "   ", MaxContextLength: 12345},
			{Name: "upstream", MaxContextLength: 1048576},
		},
		"  ":     {{Name: "ignored", MaxContextLength: 100}},
		"claude": nil,
	}}
	before := ResolveOAuthModelSetting(cfg.OAuthSettings[" CODEX "], "public", "upstream", "")
	if before == nil || before.MaxContextLength != 1048576 {
		t.Fatalf("pre-sanitize rule = %+v", before)
	}
	cfg.SanitizeOAuthSettings()
	want := []OAuthModelSetting{{Name: "public", MaxContextLength: 600000}, {Name: "upstream", MaxContextLength: 1048576}}
	if len(cfg.OAuthSettings) != 1 || !slices.Equal(cfg.OAuthSettings["codex"], want) {
		t.Fatalf("sanitized settings = %+v, want %+v", cfg.OAuthSettings, want)
	}
	after := ResolveOAuthModelSetting(cfg.OAuthSettings["codex"], "public", "upstream", "")
	if after == nil || after.MaxContextLength != before.MaxContextLength {
		t.Fatalf("post-sanitize rule = %+v, want %+v", after, before)
	}
	cfg.SanitizeOAuthSettings()
	if !slices.Equal(cfg.OAuthSettings["codex"], want) {
		t.Fatalf("repeated sanitization changed order: %+v", cfg.OAuthSettings)
	}
}

func TestOAuthSettingsParseAndSave(t *testing.T) {
	initial := []byte(`# retain catalog override
# settings for OAuth credentials only
oauth-settings:
  ' CODEX ':
    - name: gpt-5.6-luna
      max-context-length: 131072
    - name: gpt-5.6-luna
      max-context-length: 262144
    - name: gpt-5.6-luna
      alias: luna-preview
      max-context-length: 524288
    - name: '  '
      max-context-length: 123
`)
	cfg, err := ParseConfigBytes(initial)
	if err != nil {
		t.Fatal(err)
	}
	settings := cfg.OAuthSettings["codex"]
	if len(settings) != 2 || settings[0].MaxContextLength != 262144 || settings[1].Alias != "luna-preview" {
		t.Fatalf("OAuth settings did not keep last valid rule: %+v", settings)
	}
	for _, tc := range []struct {
		id, catalog, wantAlias string
		want                   int
	}{
		{id: "gpt-5.6-luna", want: 262144},
		{id: "luna-preview", catalog: "gpt-5.6-luna", wantAlias: "luna-preview", want: 524288},
		{id: "unknown", want: 0},
	} {
		rule := ResolveOAuthModelSetting(settings, tc.id, tc.catalog, "")
		if tc.want == 0 {
			if rule != nil {
				t.Fatalf("unexpected rule for %q: %+v", tc.id, rule)
			}
			continue
		}
		if rule == nil || rule.MaxContextLength != tc.want || rule.Alias != tc.wantAlias {
			t.Fatalf("rule for %q: %+v, want %d", tc.id, rule, tc.want)
		}
	}

	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, initial, 0o600); err != nil {
		t.Fatal(err)
	}
	cfg.OAuthSettings["codex"][0].MaxContextLength = 1048576
	if err := SaveConfigPreserveComments(path, cfg); err != nil {
		t.Fatal(err)
	}
	written, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(written), "# retain catalog override") {
		t.Fatalf("config comment was lost: %s", written)
	}
	loaded, err := LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(loaded.OAuthSettings["codex"]) != 2 || loaded.OAuthSettings["codex"][0].MaxContextLength != 1048576 {
		t.Fatalf("OAuth settings were not preserved on disk: %+v", loaded.OAuthSettings)
	}
	loaded.OAuthSettings = nil
	if err := SaveConfigPreserveComments(path, loaded); err != nil {
		t.Fatal(err)
	}
	written, err = os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(written), "oauth-settings:") {
		t.Fatalf("removed OAuth settings reappeared on disk: %s", written)
	}
}
