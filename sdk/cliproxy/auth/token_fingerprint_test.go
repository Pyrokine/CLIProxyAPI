package auth

import (
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"slices"
	"strings"
	"testing"
)

func testCodexBindingToken(t *testing.T, accountID, planType string, expiresAt int64) string {
	t.Helper()
	payload, err := json.Marshal(
		map[string]any{
			"exp": expiresAt,
			"https://api.openai.com/auth": map[string]any{
				"chatgpt_account_id": accountID,
				"chatgpt_plan_type":  planType,
			},
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	return "header." + base64.RawURLEncoding.EncodeToString(payload) + ".signature"
}

func TestCredentialBindingSHA256PreservesSameRefreshOwner(t *testing.T) {
	current := &Auth{
		Provider: "codex",
		Metadata: map[string]any{
			"refresh_token": "owner-refresh",
			"access_token":  "access-a",
			"id_token":      testCodexBindingToken(t, "account-a", "plus", 1),
			"last_refresh":  "2026-09-01T00:00:00Z",
			"note":          "before",
		},
		ProxyURL: "http://proxy-a.example",
	}
	updated := current.Clone()
	updated.Metadata["access_token"] = "access-b"
	updated.Metadata["id_token"] = testCodexBindingToken(t, "account-a", "plus", 2)
	updated.Metadata["last_refresh"] = "2026-09-02T00:00:00Z"
	updated.Metadata["note"] = "after"
	updated.ProxyURL = "http://proxy-b.example"

	currentBinding := credentialBindingSHA256(current)
	updatedBinding := credentialBindingSHA256(updated)
	if currentBinding == "" || currentBinding != updatedBinding {
		t.Fatalf("same owner bindings = %q, %q", currentBinding, updatedBinding)
	}
}

func TestCodexRefreshTokenRotationKeepsStableBinding(t *testing.T) {
	current := &Auth{
		Provider: "codex",
		Metadata: map[string]any{
			"refresh_token": "owner-refresh-a",
			"access_token":  "access-a",
			"id_token":      testCodexBindingToken(t, "account-a", "plus", 1),
		},
	}
	updated := current.Clone()
	updated.Metadata["refresh_token"] = "owner-refresh-b"
	updated.Metadata["access_token"] = "access-b"
	updated.Metadata["id_token"] = testCodexBindingToken(t, "account-a", "plus", 2)

	if credentialBindingSHA256(current) != credentialBindingSHA256(updated) {
		t.Fatal("stable Codex owner binding changed after token rotation")
	}
	if !persistentAuthBindingChanged(current, updated) {
		t.Fatal("explicit credential replacement did not replace the binding")
	}
	if refreshedAuthBindingChanged(current, updated) {
		t.Fatal("automatic token refresh replaced the stable owner binding")
	}
}

func TestCredentialBindingSHA256ChangesWithOwnerOrEntitlement(t *testing.T) {
	base := &Auth{
		Provider: "codex",
		Metadata: map[string]any{
			"refresh_token": "owner-a-refresh",
			"access_token":  "access-a",
			"id_token":      testCodexBindingToken(t, "account-a", "plus", 1),
		},
	}
	tests := []struct {
		name   string
		update func(*Auth)
	}{
		{
			name: "account",
			update: func(auth *Auth) {
				auth.Metadata["id_token"] = testCodexBindingToken(t, "account-b", "plus", 2)
			},
		},
		{
			name: "plan",
			update: func(auth *Auth) {
				auth.Metadata["id_token"] = testCodexBindingToken(t, "account-a", "pro", 2)
			},
		},
	}
	for _, tt := range tests {
		t.Run(
			tt.name, func(t *testing.T) {
				updated := base.Clone()
				tt.update(updated)
				if credentialBindingSHA256(base) == credentialBindingSHA256(updated) {
					t.Fatal("binding did not change")
				}
			},
		)
	}
}

func TestCredentialBindingSHA256UsesAccessTokenWithoutRefreshOwner(t *testing.T) {
	current := &Auth{Provider: "claude", Metadata: map[string]any{"access_token": "access-a"}}
	updated := current.Clone()
	updated.Metadata["access_token"] = "access-b"
	if credentialBindingSHA256(current) == credentialBindingSHA256(updated) {
		t.Fatal("access-only credential binding did not change")
	}

	codex := &Auth{
		Provider: "codex", Metadata: map[string]any{
			"access_token": "access-a",
			"id_token":     testCodexBindingToken(t, "account-a", "plus", 1),
		},
	}
	updatedCodex := codex.Clone()
	updatedCodex.Metadata["access_token"] = "access-b"
	if !persistentAuthBindingChanged(codex, updatedCodex) {
		t.Fatal("access-only Codex credential replacement did not change the binding")
	}

	apiKey := &Auth{Provider: "openai-compatibility", Attributes: map[string]string{AttributeAPIKey: "key-a"}}
	updatedAPIKey := apiKey.Clone()
	updatedAPIKey.Attributes[AttributeAPIKey] = "key-b"
	if credentialBindingSHA256(apiKey) == credentialBindingSHA256(updatedAPIKey) {
		t.Fatal("API key binding did not change")
	}
}

func TestQuotaCredentialRevisionsPreserveStatusAndLegacyIdentity(t *testing.T) {
	base := &Auth{Provider: "codex", Metadata: map[string]any{
		"type": "codex", "access_token": "synthetic-access", "disabled": false,
	}, Attributes: map[string]string{PersistedSHA256Attribute: "old-file-digest"}}
	revision, equivalent, err := QuotaCredentialRevisions(base)
	if err != nil || revision == "" {
		t.Fatalf("quota revision=%q, err=%v", revision, err)
	}
	for _, disabled := range []bool{false, true, false} {
		updated := base.Clone()
		updated.Disabled = disabled
		updated.Metadata["disabled"] = disabled
		updated.Attributes[PersistedSHA256Attribute] = "new-file-digest"
		current, _, err := QuotaCredentialRevisions(updated)
		if err != nil || current != revision {
			t.Fatalf("status=%v changed quota identity: %q, %v", disabled, current, err)
		}
		encoded, err := json.Marshal(updated.Metadata)
		if err != nil {
			t.Fatal(err)
		}
		digest, err := DigestPersistedJSON(encoded)
		if err != nil || !slices.Contains(equivalent, hex.EncodeToString(digest[:])) {
			t.Fatalf("equivalent persisted revision not retained: %v", err)
		}
	}
	if base.Metadata["disabled"] != false || base.Attributes[PersistedSHA256Attribute] != "old-file-digest" {
		t.Fatal("quota fingerprint mutated the published auth")
	}
}

func TestQuotaCredentialRevisionsKeepExactMaterialIsolation(t *testing.T) {
	base := &Auth{Provider: "codex", Metadata: map[string]any{
		"type": "codex", "account_id": "same-account", "refresh_token": "same-owner",
		"access_token": "synthetic-before", "note": "before",
	}}
	revision, _, err := QuotaCredentialRevisions(base)
	if err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{"access_token", "refresh_token", "account_id", "note"} {
		t.Run(field, func(t *testing.T) {
			updated := base.Clone()
			updated.Metadata[field] = "synthetic-after"
			after, equivalent, err := QuotaCredentialRevisions(updated)
			if err != nil || after == revision || slices.Contains(equivalent, revision) {
				t.Fatalf("changed material retained old identity: revision=%q, err=%v", after, err)
			}
		})
	}
	base.Attributes = map[string]string{AttributeAPIKey: "synthetic-attribute-key"}
	after, equivalent, err := QuotaCredentialRevisions(base)
	if err != nil || after == revision || len(equivalent) != 1 {
		t.Fatalf("unproven attribute key accepted legacy metadata: %q, %v, %v", after, equivalent, err)
	}
}

type quotaRevisionRawStorage struct{ raw []byte }

func (s quotaRevisionRawStorage) RawJSON() []byte { return s.raw }
func (quotaRevisionRawStorage) SaveTokenToFile(string) error {
	panic("unexpected persistence during quota fingerprint test")
}

func TestBindPersistedAuthDigestRequiresUnchangedMaterial(t *testing.T) {
	raw := []byte(`{"type":"codex","access_token":"synthetic","request-retry":1}`)
	metadata, err := DecodeCredentialMetadata(raw)
	if err != nil {
		t.Fatal(err)
	}
	NormalizeCredentialMetadata(metadata)
	attributes := map[string]string{"source": "synthetic"}
	auth := &Auth{Provider: "codex", Metadata: metadata, Attributes: attributes}
	digest, err := DigestPersistedJSON(raw)
	if err != nil {
		t.Fatal(err)
	}
	if err := BindPersistedAuthDigest(auth, digest); err != nil {
		t.Fatal(err)
	}
	if _, mutated := attributes[PersistedSHA256Attribute]; mutated {
		t.Fatal("binding mutated a shared attribute map")
	}
	original := hex.EncodeToString(digest[:])
	for _, field := range []string{"unchanged", "disabled", "access_token", "note", "provider", "attribute_digest"} {
		t.Run(field, func(t *testing.T) {
			updated := auth.Clone()
			switch field {
			case "disabled":
				updated.Disabled = true
				updated.Metadata["disabled"] = true
			case "access_token", "note":
				updated.Metadata[field] = "changed"
			case "provider":
				updated.Provider = "claude"
			case "attribute_digest":
				updated.Attributes[PersistedSHA256Attribute] = strings.Repeat("f", 64)
			}
			_, equivalent, err := QuotaCredentialRevisions(updated)
			want := field == "unchanged" || field == "disabled" || field == "attribute_digest"
			if err != nil || slices.Contains(equivalent, original) != want || slices.Contains(equivalent, strings.Repeat("f", 64)) {
				t.Fatalf("persisted material proof was not isolated: %v", err)
			}
		})
	}
}

func TestQuotaCredentialRevisionsRejectRoundedLegacyNumbers(t *testing.T) {
	oldMetadata := map[string]any{"type": "codex", "access_token": "synthetic", "credential_serial": float64(9007199254740993)}
	oldEnvelope, err := json.Marshal(map[string]any{"provider": "codex", "api_key": "", "metadata": oldMetadata})
	if err != nil {
		t.Fatal(err)
	}
	oldRevision, err := DigestPersistedJSON(oldEnvelope)
	if err != nil {
		t.Fatal(err)
	}
	raw := []byte(`{"type":"codex","access_token":"synthetic","credential_serial":9007199254740992}`)
	metadata, err := DecodeCredentialMetadata(raw)
	if err != nil {
		t.Fatal(err)
	}
	digest, err := DigestPersistedJSON(raw)
	if err != nil {
		t.Fatal(err)
	}
	auth := &Auth{Provider: "codex", Metadata: metadata}
	if err := BindPersistedAuthDigest(auth, digest); err != nil {
		t.Fatal(err)
	}
	revision, equivalent, err := QuotaCredentialRevisions(auth)
	if err != nil || revision == hex.EncodeToString(oldRevision[:]) || slices.Contains(equivalent, hex.EncodeToString(digest[:])) {
		t.Fatalf("rounded legacy material remained a quota identity: %v", err)
	}
}

func TestQuotaCredentialRevisionsPreserveJSONNumbers(t *testing.T) {
	var previous string
	for _, serial := range []string{"9007199254740992", "9007199254740993"} {
		raw := []byte(`{"type":"codex","access_token":"synthetic","credential_serial":` + serial + `}`)
		auth := &Auth{Provider: "codex", Storage: quotaRevisionRawStorage{raw: raw}}
		revision, equivalent, err := QuotaCredentialRevisions(auth)
		if err != nil {
			t.Fatal(err)
		}
		if revision == previous {
			t.Fatal("adjacent large integers share a quota identity")
		}
		digest, err := DigestPersistedJSON(raw)
		if err != nil || len(equivalent) != 1 || equivalent[0] != revision || slices.Contains(equivalent, hex.EncodeToString(digest[:])) {
			t.Fatalf("precise numeric material accepted an ambiguous legacy candidate: %v", err)
		}
		previous = revision
	}
	for _, value := range []any{float64(9007199254740992), []any{float64(-9007199254740992)}} {
		_, _, err := QuotaCredentialRevisions(&Auth{Provider: "codex", Metadata: map[string]any{"serial": value}})
		if err == nil {
			t.Fatal("rounded metadata without exact source was accepted")
		}
	}
}

func TestQuotaCredentialRevisionsRejectInvalidMaterial(t *testing.T) {
	for name, auth := range map[string]*Auth{
		"nil auth":             nil,
		"invalid storage":      {Storage: quotaRevisionRawStorage{raw: []byte(`invalid`)}},
		"null storage":         {Storage: quotaRevisionRawStorage{raw: []byte(`null`)}},
		"array storage":        {Storage: quotaRevisionRawStorage{raw: []byte(`[]`)}},
		"unsupported metadata": {Metadata: map[string]any{"invalid": make(chan int)}},
	} {
		t.Run(name, func(t *testing.T) {
			revision, equivalent, err := QuotaCredentialRevisions(auth)
			if err == nil || revision != "" || len(equivalent) != 0 {
				t.Fatalf("invalid material accepted: revision=%q, equivalents=%v, err=%v", revision, equivalent, err)
			}
		})
	}
}

func TestQuotaCredentialRevisionsMergeStorageWithoutMutation(t *testing.T) {
	const original = `{"type":"codex","access_token":"stored","refresh_token":"owner","disabled":true}`
	raw := []byte(original)
	auth := &Auth{Provider: "codex", Storage: quotaRevisionRawStorage{raw: raw},
		Metadata: map[string]any{"access_token": "current", "disabled": false}}
	revision, _, err := QuotaCredentialRevisions(auth)
	if err != nil {
		t.Fatal(err)
	}
	complete := &Auth{Provider: "codex", Metadata: map[string]any{
		"type": "codex", "access_token": "current", "refresh_token": "owner", "disabled": false,
	}}
	want, _, err := QuotaCredentialRevisions(complete)
	if err != nil || revision != want || string(raw) != original ||
		auth.Metadata["disabled"] != false || auth.Metadata["access_token"] != "current" {
		t.Fatalf("storage merge changed identity or source: got=%q want=%q err=%v", revision, want, err)
	}
}

func TestPersistentAuthBindingChangedUsesExactBaseURL(t *testing.T) {
	current := &Auth{
		Provider: "openai-compatibility",
		Attributes: map[string]string{
			"base_url":      "https://example.com/v1/Account",
			AttributeAPIKey: "key",
		},
	}
	updated := current.Clone()
	updated.Attributes["base_url"] = "https://example.com/v1/account"
	if !persistentAuthBindingChanged(current, updated) {
		t.Fatal("case-sensitive upstream path change did not replace the binding")
	}
}
