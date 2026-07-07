package auth

import "testing"

func TestSchedulerAuthCandidatesFilterSensitiveFields(t *testing.T) {
	candidates := schedulerAuthCandidates(
		[]*Auth{
			{
				ID:       "auth-1",
				Provider: "Claude",
				Status:   StatusActive,
				Attributes: map[string]string{
					"provider_key":  "claude",
					"priority":      "7",
					"api-key":       "secret-key",
					"refresh_token": "refresh-token",
					"proxy_url":     "http://proxy.local",
					"label":         "primary",
				},
				Metadata: map[string]any{
					"token": "secret-token",
				},
			},
		},
	)

	if len(candidates) != 1 {
		t.Fatalf("candidates len = %d, want 1", len(candidates))
	}
	candidate := candidates[0]
	if candidate.Provider != "claude" {
		t.Fatalf("provider = %q, want claude", candidate.Provider)
	}
	if candidate.Metadata != nil {
		t.Fatalf("metadata = %#v, want nil", candidate.Metadata)
	}
	if candidate.Priority != 7 {
		t.Fatalf("priority = %d, want 7", candidate.Priority)
	}
	if got := candidate.Attributes["provider_key"]; got != "claude" {
		t.Fatalf("provider_key = %q, want claude", got)
	}
	if got := candidate.Attributes["label"]; got != "primary" {
		t.Fatalf("label = %q, want primary", got)
	}
	for _, key := range []string{"api-key", "refresh_token", "proxy_url"} {
		if _, ok := candidate.Attributes[key]; ok {
			t.Fatalf("attribute %q should be filtered", key)
		}
	}
}
