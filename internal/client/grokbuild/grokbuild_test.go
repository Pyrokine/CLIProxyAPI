package grokbuild

import (
	"encoding/json"
	"testing"
)

func TestIsGrokShellUserAgent(t *testing.T) {
	tests := []struct {
		name string
		ua   string
		want bool
	}{
		{name: "shell", ua: "grok-shell/0.2.120 (linux; amd64)", want: true},
		{name: "pager", ua: "grok-pager/0.2.120 grok-shell/0.2.120", want: true},
		{name: "case insensitive", ua: "GROK-SHELL/0.2.120", want: true},
		{name: "ordinary client", ua: "curl/8.7.1", want: false},
	}
	for _, tt := range tests {
		t.Run(
			tt.name, func(t *testing.T) {
				if got := IsGrokShellUserAgent(tt.ua); got != tt.want {
					t.Fatalf("IsGrokShellUserAgent(%q) = %t, want %t", tt.ua, got, tt.want)
				}
			},
		)
	}
}

func TestBuildResponse(t *testing.T) {
	response := BuildResponse(
		[]ModelInfo{
			{ID: "grok-4.5", DisplayName: "Grok 4.5", ContextLength: 256000, ReasoningLevels: []string{" high ", ""}},
			{ID: "plain-model"},
		},
	)
	if response.Object != "list" || len(response.Data) != 2 {
		t.Fatalf("response = %#v", response)
	}
	entry := response.Data[0]
	if entry.ID != "grok-4.5" || entry.Model != entry.ID || entry.Name != "Grok 4.5" {
		t.Fatalf("identity = %#v", entry)
	}
	if entry.ContextWindow != 256000 || entry.APIBackend != "responses" || !entry.SupportedInAPI {
		t.Fatalf("routing metadata = %#v", entry)
	}
	if len(entry.ReasoningEfforts) != 1 || entry.ReasoningEfforts[0].Value != "high" {
		t.Fatalf("reasoning efforts = %#v", entry.ReasoningEfforts)
	}
	if response.Data[1].Name != "plain-model" {
		t.Fatalf("fallback name = %#v", response.Data[1])
	}

	wire, err := json.Marshal(response)
	if err != nil {
		t.Fatalf("json.Marshal() error = %v", err)
	}
	var decoded struct {
		Data []map[string]json.RawMessage `json:"data"`
	}
	if err = json.Unmarshal(wire, &decoded); err != nil {
		t.Fatalf("json.Unmarshal() error = %v", err)
	}
	if _, ok := decoded.Data[1]["context_window"]; ok {
		t.Fatalf("zero context_window was not omitted: %s", wire)
	}
	if _, ok := decoded.Data[1]["reasoning_efforts"]; ok {
		t.Fatalf("empty reasoning_efforts was not omitted: %s", wire)
	}
}
