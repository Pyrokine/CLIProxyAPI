package management

import (
	"encoding/json"
	"testing"
	"time"

	coreauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
)

func passiveQuotaEntryJSON(
	t *testing.T,
	provider string,
	quota coreauth.QuotaState,
	states map[string]*coreauth.ModelState,
) map[string]any {
	t.Helper()
	auth := &coreauth.Auth{
		ID: "passive-observation", Provider: provider, Status: coreauth.StatusActive,
		Attributes: map[string]string{"runtime_only": "true"}, Quota: quota, ModelStates: states,
	}
	entry := (&Handler{}).buildAuthFileEntry(auth)
	if entry == nil {
		t.Fatal("passive observation entry is nil")
	}
	raw, err := json.Marshal(entry)
	if err != nil {
		t.Fatal(err)
	}
	var result map[string]any
	if err := json.Unmarshal(raw, &result); err != nil {
		t.Fatal(err)
	}
	return result
}

func TestModelQuotaObservationPayloadOmitsUnsupportedProviders(t *testing.T) {
	states := map[string]*coreauth.ModelState{
		"grok-4": {
			Quota: coreauth.QuotaState{
				ObservedAt: time.Unix(10, 0), Signals: map[string]string{"X-Ratelimit-Remaining-Requests": "1"},
			},
		},
	}
	for _, provider := range []string{
		"grok", "gemini", "gemini-interactions", "openai", "openai-compatibility", "plugin-provider",
	} {
		t.Run(
			provider, func(t *testing.T) {
				entry := passiveQuotaEntryJSON(t, provider, states["grok-4"].Quota, states)
				got, ok := entry["model_quotas"].(map[string]any)
				if !ok || len(got) != 0 {
					t.Fatalf("unsupported provider returned model observations: %#v", entry["model_quotas"])
				}
				quota := entry["quota"].(map[string]any)
				if _, present := quota["observed_at"]; present || len(quota["signals"].(map[string]any)) != 0 {
					t.Fatalf("unsupported provider returned credential observations: %#v", quota)
				}
			},
		)
	}
}

func TestModelQuotaObservationPayloadSkipsNilAndEmptyStates(t *testing.T) {
	states := map[string]*coreauth.ModelState{
		"nil": nil, "empty": {},
		"observed": {
			Quota: coreauth.QuotaState{
				ObservedAt: time.Unix(10, 0), Signals: map[string]string{"X-Codex-Plan-Type": "pro"},
			},
		},
	}
	entry := passiveQuotaEntryJSON(t, "codex", coreauth.QuotaState{}, states)
	got, ok := entry["model_quotas"].(map[string]any)
	if !ok || len(got) != 1 {
		t.Fatalf("model observations = %#v, want only observed state", entry["model_quotas"])
	}
	if _, present := got["observed"]; !present {
		t.Fatalf("observed model quota missing: %#v", got)
	}
}

func TestQuotaObservationPayloadExcludesCooldownState(t *testing.T) {
	observed := time.Unix(10, 0)
	entry := passiveQuotaEntryJSON(
		t, "codex", coreauth.QuotaState{
			Exceeded: true, Reason: "credential_quota", NextRecoverAt: time.Unix(20, 0), BackoffLevel: 3,
			ObservedAt: observed, Signals: map[string]string{"X-Codex-Plan-Type": "pro"},
		}, nil,
	)
	payload, ok := entry["quota"].(map[string]any)
	if !ok {
		t.Fatalf("missing passive quota: %#v", entry["quota"])
	}
	for _, key := range []string{"exceeded", "reason", "next_recover_at", "backoff_level"} {
		if _, present := payload[key]; present {
			t.Fatalf("cooldown %s leaked: %#v", key, payload)
		}
	}
	if got, err := time.Parse(time.RFC3339Nano, payload["observed_at"].(string)); err != nil || !got.Equal(observed) {
		t.Fatalf("observed time changed: %v, %v", got, err)
	}
	if payload["signals"].(map[string]any)["X-Codex-Plan-Type"] != "pro" {
		t.Fatalf("passive signals changed: %#v", payload)
	}
}
