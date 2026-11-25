package management

import (
	"bytes"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
)

func TestProviderKeyAliasesPersistAgainstStableConfiguredReferences(t *testing.T) {
	const codexKey = "demo-codex-aaaa"
	const firstKey = "same-draft-one-tail"
	const secondKey = "same-draft-two-tail"
	const clientKey = "demo-client-authorized"
	path := filepath.Join(t.TempDir(), "config.yaml")
	yaml := "port: 8317\nauth-dir: " + filepath.Join(filepath.Dir(path), "auth") + "\n" +
		"api-keys: [" + clientKey + "]\napi-key-aliases: {" + clientKey + ": Client}\n" +
		"codex-api-key:\n  - api-key: " + codexKey + "\n    base-url: https://codex.invalid\n" +
		"openai-compatibility:\n  - name: Synthetic\n    disabled: true\n    base-url: https://openai.invalid\n" +
		"    api-key-entries:\n      - api-key: " + firstKey + "\n      - api-key: " + secondKey + "\n"
	if err := os.WriteFile(path, []byte(yaml), 0600); err != nil {
		t.Fatal(err)
	}
	loadHandler := func() *Handler {
		t.Helper()
		cfg, err := config.LoadConfig(path)
		if err != nil {
			t.Fatal(err)
		}
		h := &Handler{configFilePath: path}
		h.mu.Lock()
		h.setConfigLocked(cfg)
		h.mu.Unlock()
		return h
	}
	type credential struct {
		Ref       string `json:"alias-ref"`
		AuthIndex string `json:"auth-index"`
	}
	checkResponse := func(recBody string) {
		t.Helper()
		for _, key := range []string{codexKey, firstKey, secondKey, clientKey} {
			if strings.Contains(recBody, key) {
				t.Fatal("management response returned an unredacted key")
			}
		}
	}
	readRefs := func(h *Handler) []string {
		t.Helper()
		c, rec := newArtifactCacheTestContext(http.MethodGet, "/config", "")
		h.GetConfig(c)
		checkResponse(rec.Body.String())
		var body struct {
			Codex  []credential `json:"codex-api-key"`
			OpenAI []struct {
				Disabled bool         `json:"disabled"`
				Entries  []credential `json:"api-key-entries"`
			} `json:"openai-compatibility"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil || rec.Code != http.StatusOK ||
			len(body.Codex) != 1 || len(body.OpenAI) != 1 || len(body.OpenAI[0].Entries) != 2 || !body.OpenAI[0].Disabled {
			t.Fatalf("invalid credential list: status=%d body=%s err=%v", rec.Code, rec.Body.String(), err)
		}
		first, second := body.OpenAI[0].Entries[0], body.OpenAI[0].Entries[1]
		if body.Codex[0].Ref == "" || first.Ref == "" || second.Ref == "" || first.Ref == second.Ref ||
			first.AuthIndex != "" || second.AuthIndex != "" {
			t.Fatal("configured keys do not have distinct references independent of runtime identity")
		}
		refs := []string{body.Codex[0].Ref, first.Ref, second.Ref}
		c, rec = newArtifactCacheTestContext(http.MethodGet, "/codex-api-key", "")
		h.GetCodexKeys(c)
		checkResponse(rec.Body.String())
		if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), refs[0]) {
			t.Fatal("dedicated Codex response lost the credential reference")
		}
		c, rec = newArtifactCacheTestContext(http.MethodGet, "/openai-compatibility", "")
		h.GetOpenAICompat(c)
		checkResponse(rec.Body.String())
		if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), refs[1]) ||
			!strings.Contains(rec.Body.String(), refs[2]) {
			t.Fatal("dedicated OpenAI response lost credential references")
		}
		return refs
	}
	patch := func(h *Handler, ref, alias string, status int) {
		t.Helper()
		input, err := json.Marshal(map[string]string{"key-ref": ref, "alias": alias})
		if err != nil {
			t.Fatal(err)
		}
		c, rec := newArtifactCacheTestContext(http.MethodPatch, "/api-key-aliases", string(input))
		h.PatchAPIKeyAlias(c)
		checkResponse(rec.Body.String())
		if rec.Code != status {
			t.Fatalf("patch alias status=%d want=%d body=%s", rec.Code, status, rec.Body.String())
		}
	}
	readAliases := func(h *Handler) map[string]string {
		t.Helper()
		c, rec := newArtifactCacheTestContext(http.MethodGet, "/api-key-aliases?by=credential-ref", "")
		h.GetAPIKeyAliases(c)
		checkResponse(rec.Body.String())
		var body struct {
			Aliases map[string]string `json:"aliases-by-ref"`
		}
		if err := json.Unmarshal(
			rec.Body.Bytes(), &body,
		); err != nil || rec.Code != http.StatusOK || body.Aliases == nil {
			t.Fatalf("read aliases status=%d body=%s err=%v", rec.Code, rec.Body.String(), err)
		}
		return body.Aliases
	}
	readFile := func() []byte {
		t.Helper()
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		return data
	}

	h := loadHandler()
	refs := readRefs(h)
	patch(h, refs[0], "Codex", http.StatusOK)
	patch(h, refs[1], "First", http.StatusOK)
	patch(h, refs[2], "Second", http.StatusOK)
	before := readFile()
	patch(h, "alias_00000000000000000000000000000000", "Invalid", http.StatusConflict)
	patch(h, refs[1], "invalid alias", http.StatusBadRequest)
	if !bytes.Equal(before, readFile()) {
		t.Fatal("rejected updates modified the YAML")
	}

	h = loadHandler()
	refs = readRefs(h)
	aliases := readAliases(h)
	for i, label := range []string{"Codex", "First", "Second"} {
		if aliases[refs[i]] != label {
			t.Fatalf("readback for row %d = %q want %q", i, aliases[refs[i]], label)
		}
	}
	cfg := h.configSnapshot()
	persisted := cfg.APIKeyAliases
	if len(persisted) != 4 || persisted[codexKey] != "Codex" || persisted[firstKey] != "First" ||
		persisted[secondKey] != "Second" || persisted[clientKey] != "Client" {
		t.Fatal("YAML aliases were not persisted against the original credential keys")
	}
	for _, warning := range config.ValidateConfig(cfg) {
		if strings.HasPrefix(warning, "api-key-aliases:") {
			t.Fatalf("a configured provider alias triggered an unexpected warning: %s", warning)
		}
	}

	patch(h, refs[1], "", http.StatusOK)
	aliases = readAliases(loadHandler())
	if aliases[refs[1]] != "" || aliases[refs[2]] != "Second" {
		t.Fatal("clearing an alias changed the other credential")
	}

	h.replaceAPIKeyAliasesSnapshot(map[string]string{" " + firstKey + " ": "Unrecognized"})
	if readAliases(h)[refs[1]] != "" {
		t.Fatal("a nonmatching YAML alias became associated with an existing credential")
	}
}
