package tui

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
)

func TestLegacyQuotaFieldsRemainVisibleAndReadOnly(t *testing.T) {
	model := configTabModel{}
	model.fields = model.parseConfig(
		map[string]any{
			"quota-exceeded": map[string]any{"switch-project": true, "switch-preview-model": false},
		},
	)
	want := map[string]string{"quota-exceeded/switch-project": "true", "quota-exceeded/switch-preview-model": "false"}
	for i, field := range model.fields {
		value, ok := want[field.apiPath]
		if !ok {
			continue
		}
		if field.value != value || field.kind != "readonly" || !strings.Contains(field.label, "inactive") {
			t.Fatalf("legacy field = %+v; want inactive, read-only value %q", field, value)
		}
		model.cursor = i
		if next, cmd := model.handleNormalKey(tea.KeyPressMsg{Code: tea.KeyEnter}); cmd != nil || next.editing {
			t.Fatalf("Enter initiated an edit for %s", field.apiPath)
		}
		delete(want, field.apiPath)
	}
	if len(want) != 0 {
		t.Fatalf("missing legacy fields: %v", want)
	}
}
