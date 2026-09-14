package models

import "testing"

func TestNewCatalogLoadsAvailableModelsWithoutMutatingInput(t *testing.T) {
	availableModels := []map[string]any{
		{
			"id":           "gpt-4o",
			"display_name": "GPT-4o Alias",
			"max_tokens":   64000,
		},
	}

	catalog := NewCatalog(availableModels, false)
	data := responseModels(t, catalog.Response())
	if len(data) != 1 {
		t.Fatalf("models length = %d, want 1", len(data))
	}
	if got := stringValue(data[0], "id"); got != ClientModelID("gpt-4o") {
		t.Fatalf("client id = %q, want %q", got, ClientModelID("gpt-4o"))
	}
	if got := stringValue(data[0], "display_name"); got != "GPT-4o Alias" {
		t.Fatalf("display_name = %q, want GPT-4o Alias", got)
	}
	if got := data[0]["max_tokens"]; got != 64000 {
		t.Fatalf("max_tokens = %#v, want 64000", got)
	}
	if got := availableModels[0]["id"]; got != "gpt-4o" {
		t.Fatalf("input id mutated to %#v", got)
	}
}

func TestCatalogPreservesProviderModelIDExactly(t *testing.T) {
	providerID := " provider/model "
	catalog := NewCatalog(
		[]map[string]any{
			{
				"id":           providerID,
				"display_name": "Provider Model",
			},
		}, false,
	)
	data := responseModels(t, catalog.Response())
	clientID := ClientModelID(providerID)
	if got := stringValue(data[0], "id"); got != clientID {
		t.Fatalf("client id = %q, want %q", got, clientID)
	}
	if got := catalog.ResolveModelID(clientID); got != providerID {
		t.Fatalf("provider id = %q, want %q", got, providerID)
	}
}

func TestCatalogResponseOrderIsDeterministic(t *testing.T) {
	availableModels := []map[string]any{
		{"id": "claude-z", "display_name": "Zebra"},
		{"id": "gpt-4o", "display_name": "Alpha"},
		{"id": "claude-c", "display_name": "Alpha"},
		{"id": "claude-b", "display_name": "Beta"},
	}

	response := NewCatalog(availableModels, false).Response()
	data := responseModels(t, response)
	wantIDs := []string{
		"claude-c",
		ClientModelID("gpt-4o"),
		"claude-b",
		"claude-z",
	}
	if len(data) != len(wantIDs) {
		t.Fatalf("models length = %d, want %d", len(data), len(wantIDs))
	}
	for i, want := range wantIDs {
		if got := stringValue(data[i], "id"); got != want {
			t.Fatalf("models[%d].id = %q, want %q", i, got, want)
		}
	}
	if got := response["first_id"]; got != wantIDs[0] {
		t.Fatalf("first_id = %#v, want %q", got, wantIDs[0])
	}
	if got := response["last_id"]; got != wantIDs[len(wantIDs)-1] {
		t.Fatalf("last_id = %#v, want %q", got, wantIDs[len(wantIDs)-1])
	}
}

func TestCatalogDisablesModelListCloaking(t *testing.T) {
	const providerID = "gpt-4o"
	catalog := NewCatalog(
		[]map[string]any{
			{
				"id":           providerID,
				"display_name": "GPT-4o",
			},
		}, true,
	)

	response := catalog.Response()
	data := responseModels(t, response)
	if len(data) != 1 {
		t.Fatalf("models length = %d, want 1", len(data))
	}
	if got := stringValue(data[0], "id"); got != providerID {
		t.Fatalf("model id = %q, want %q", got, providerID)
	}
	if got := response["first_id"]; got != providerID {
		t.Fatalf("first_id = %#v, want %q", got, providerID)
	}
	if got := response["last_id"]; got != providerID {
		t.Fatalf("last_id = %#v, want %q", got, providerID)
	}
	if got := catalog.ResolveModelID(ClientModelID(providerID)); got != providerID {
		t.Fatalf("legacy cloaked id resolved to %q, want %q", got, providerID)
	}
}

func TestCatalogUncloakedProviderIDWinsCloakedIDCollision(t *testing.T) {
	externalID := "gpt-4o"
	reservedID := ClientModelID(externalID)
	catalog := NewCatalog(
		[]map[string]any{
			{"id": externalID, "display_name": "External"},
			{"id": reservedID, "display_name": "Reserved"},
		}, true,
	)

	if got := catalog.ResolveModelID(reservedID); got != reservedID {
		t.Fatalf("raw provider id resolved to %q, want %q", got, reservedID)
	}
}

func TestClientModelIDRoundTrip(t *testing.T) {
	models := []string{
		"claude-sonnet-4-6",
		"gpt-4o",
		"gemini-2.5-pro",
		"模型/alpha",
		clientModelPrefix + "provider-model",
	}
	for _, model := range models {
		t.Run(
			model, func(t *testing.T) {
				clientID := ClientModelID(model)
				if got := resolveClientModelID(clientID); got != model {
					t.Fatalf("round trip = %q, want %q", got, model)
				}
				if got := resolveClientModelID(clientID + "(high)"); got != model+"(high)" {
					t.Fatalf("round trip with suffix = %q, want %q", got, model+"(high)")
				}
			},
		)
	}
}

func TestResolveClientModelIDRejectsNonCanonicalNamespace(t *testing.T) {
	externalWithCollisionNamespace := collisionClientModelPrefix + encodeModelID("gpt-4o")
	if got := resolveClientModelID(externalWithCollisionNamespace); got != externalWithCollisionNamespace {
		t.Fatalf("non-canonical collision namespace resolved to %q", got)
	}

	reservedWithExternalNamespace := externalClientModelPrefix + encodeModelID(clientModelPrefix+"provider-model")
	if got := resolveClientModelID(reservedWithExternalNamespace); got != reservedWithExternalNamespace {
		t.Fatalf("non-canonical external namespace resolved to %q", got)
	}
}

func TestCatalogClientIDsAreCollisionSafe(t *testing.T) {
	externalID := "gpt-4o"
	reservedID := ClientModelID(externalID)
	catalog := NewCatalog(
		[]map[string]any{
			{"id": externalID, "display_name": "External"},
			{"id": reservedID, "display_name": "Reserved"},
		}, false,
	)
	data := responseModels(t, catalog.Response())
	if len(data) != 2 {
		t.Fatalf("models length = %d, want 2", len(data))
	}

	externalClientID := ClientModelID(externalID)
	reservedClientID := ClientModelID(reservedID)
	if externalClientID == reservedClientID {
		t.Fatalf("client IDs collided at %q", externalClientID)
	}
	if got := catalog.ResolveModelID(externalClientID); got != externalID {
		t.Fatalf("external model resolved to %q, want %q", got, externalID)
	}
	if got := catalog.ResolveModelID(reservedClientID); got != reservedID {
		t.Fatalf("reserved model resolved to %q, want %q", got, reservedID)
	}
}

func TestCatalogPreservesAliasDisplayName(t *testing.T) {
	alias := "team-coder"
	catalog := NewCatalog(
		[]map[string]any{
			{
				"id":           alias,
				"display_name": "Team Coder",
				"owned_by":     "anthropic",
			},
		}, false,
	)
	data := responseModels(t, catalog.Response())
	if len(data) != 1 {
		t.Fatalf("models length = %d, want 1", len(data))
	}
	if got := stringValue(data[0], "id"); got != ClientModelID(alias) {
		t.Fatalf("alias id = %q, want %q", got, ClientModelID(alias))
	}
	if got := stringValue(data[0], "display_name"); got != "Team Coder" {
		t.Fatalf("display_name = %q, want Team Coder", got)
	}
	if got := catalog.ResolveModelID(ClientModelID(alias)); got != alias {
		t.Fatalf("resolved alias = %q, want %q", got, alias)
	}
}

func TestEmptyCatalogResponse(t *testing.T) {
	response := NewCatalog(nil, false).Response()
	if data := responseModels(t, response); len(data) != 0 {
		t.Fatalf("models length = %d, want 0", len(data))
	}
	if response["first_id"] != "" || response["last_id"] != "" {
		t.Fatalf("empty IDs = (%#v, %#v), want empty", response["first_id"], response["last_id"])
	}
}

func responseModels(t *testing.T, response map[string]any) []map[string]any {
	t.Helper()
	data, ok := response["data"].([]map[string]any)
	if !ok {
		t.Fatalf("data type = %T, want []map[string]any", response["data"])
	}
	return data
}
