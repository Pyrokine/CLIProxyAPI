package cliproxy

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/registry"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/runtime/executor"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/runtime/executor/helps"
	coreauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/config"
	sdktranslator "github.com/Pyrokine/CLIProxyAPI/v6/sdk/translator"
	"github.com/tidwall/gjson"
	"google.golang.org/protobuf/encoding/protowire"
)

func TestEnsureExecutorsForAuthRegistersDevinExecutor(t *testing.T) {
	service := &Service{
		cfg:         &config.Config{},
		coreManager: coreauth.NewManager(nil, nil, nil),
	}
	auth := &coreauth.Auth{
		ID:       "devin-auth",
		Provider: "devin",
		Status:   coreauth.StatusActive,
	}

	service.ensureExecutorsForAuth(auth)

	resolved, ok := service.coreManager.Executor("devin")
	if !ok || resolved == nil {
		t.Fatal("expected Devin executor after auth binding")
	}
	if _, ok = resolved.(*executor.DevinExecutor); !ok {
		t.Fatalf("executor = %T, want *executor.DevinExecutor", resolved)
	}
}

func TestRegisterHomeExecutorsIncludesDevin(t *testing.T) {
	service := &Service{
		cfg:         &config.Config{},
		coreManager: coreauth.NewManager(nil, nil, nil),
	}
	service.registerHomeExecutors()

	resolved, ok := service.coreManager.Executor("devin")
	if !ok || resolved == nil {
		t.Fatal("expected Devin executor after baseline registration")
	}
	if _, ok = resolved.(*executor.DevinExecutor); !ok {
		t.Fatalf("executor = %T, want *executor.DevinExecutor", resolved)
	}
}

func TestRegisterModelsForDevinAuthUsesNamespacedCatalogAndExclusions(t *testing.T) {
	const authID = "devin-model-auth"
	cfg := &config.Config{
		OAuthExcludedModels: map[string][]string{
			"devin": {"devin/swe-2"},
		},
	}
	auth := &coreauth.Auth{
		ID:         authID,
		Provider:   "devin",
		Status:     coreauth.StatusActive,
		Attributes: map[string]string{"auth_kind": "oauth"},
	}
	modelRegistry := registry.GetGlobalRegistry()
	modelRegistry.UnregisterClient(authID)
	t.Cleanup(func() { modelRegistry.UnregisterClient(authID) })

	(&Service{cfg: cfg}).registerModelsForAuthWithConfig(auth, cfg)

	models := modelRegistry.GetModelsForClient(authID)
	if len(models) == 0 {
		t.Fatal("Devin model catalog was not registered")
	}
	for _, model := range models {
		if model == nil {
			t.Fatal("registered Devin model is nil")
		}
		if model.ID == "devin/swe-2" {
			t.Fatal("excluded Devin model was registered")
		}
		if model.Type != "devin" {
			t.Fatalf("model %q type = %q, want devin", model.ID, model.Type)
		}
		if len(model.ID) < len("devin/") || model.ID[:len("devin/")] != "devin/" {
			t.Fatalf("model ID = %q, want devin namespace", model.ID)
		}
	}
	if modelRegistry.ClientSupportsModel(authID, "swe-2") {
		t.Fatal("bare Devin model ID should not be registered")
	}
	if modelRegistry.ClientSupportsModel(authID, "devin/swe-2") {
		t.Fatal("excluded namespaced Devin model should not be registered")
	}
}

func TestDevinManagerExecutionUsesPinnedSelectedIdentity(t *testing.T) {
	var callsA, callsB int
	newServer := func(label string, calls *int) *httptest.Server {
		return httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
			*calls++
			if request.URL.Path != helps.DevinChatPath {
				t.Fatalf("%s request path = %q", label, request.URL.Path)
			}
			token := "devin-session-token$" + label
			if got := request.Header.Get("Authorization"); got != "Basic "+token+"-"+token {
				t.Fatalf("%s Authorization = %q", label, got)
			}
			writer.Header().Set("Content-Type", "application/connect+proto")
			_, _ = writer.Write(devinBindingTestResponse())
		}))
	}
	serverA := newServer("A", &callsA)
	defer serverA.Close()
	serverB := newServer("B", &callsB)
	defer serverB.Close()

	cfg := &config.Config{}
	manager := coreauth.NewManager(nil, nil, nil)
	service := &Service{cfg: cfg, coreManager: manager}
	modelRegistry := registry.GetGlobalRegistry()
	registered := make(map[string]*coreauth.Auth)
	for _, candidate := range []*coreauth.Auth{
		{
			ID: "devin-a", Provider: "devin", FileName: "devin-shared.json", Status: coreauth.StatusActive,
			Attributes: map[string]string{
				"api_key": "devin-session-token$A", "base_url": serverA.URL, "auth_kind": "oauth",
				coreauth.AttributeAuthIndexSeed: "devin-user-a",
			},
		},
		{
			ID: "devin-b", Provider: "devin", FileName: "devin-shared.json", Status: coreauth.StatusActive,
			Attributes: map[string]string{
				"api_key": "devin-session-token$B", "base_url": serverB.URL, "auth_kind": "oauth",
				coreauth.AttributeAuthIndexSeed: "devin-user-b",
			},
		},
	} {
		auth, err := manager.Register(context.Background(), candidate)
		if err != nil {
			t.Fatalf("register %s: %v", candidate.ID, err)
		}
		registered[auth.ID] = auth
		modelRegistry.UnregisterClient(auth.ID)
		t.Cleanup(func() { modelRegistry.UnregisterClient(auth.ID) })
		service.registerModelsForAuthWithConfig(auth, cfg)
		manager.RefreshSchedulerEntry(auth.ID)
	}
	service.ensureExecutorsForAuth(registered["devin-b"])

	selectedIndex := ""
	response, err := manager.Execute(
		context.Background(),
		[]string{"devin"},
		cliproxyexecutor.Request{
			Model:   "devin/swe-2",
			Payload: []byte(`{"input":[{"type":"user_input","content":"hi"}]}`),
		},
		cliproxyexecutor.Options{
			SourceFormat:   sdktranslator.FormatInteractions,
			ResponseFormat: sdktranslator.FormatInteractions,
			Metadata: map[string]any{
				cliproxyexecutor.PinnedAuthMetadataKey: "devin-b",
				cliproxyexecutor.SelectedAuthIndexCallbackMetadataKey: func(index string) {
					selectedIndex = index
				},
			},
		},
	)
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if callsA != 0 || callsB != 1 {
		t.Fatalf("Devin calls A/B = %d/%d, want 0/1", callsA, callsB)
	}
	if selectedIndex != registered["devin-b"].Index {
		t.Fatalf("selected auth index = %q, want %q", selectedIndex, registered["devin-b"].Index)
	}
	if got := gjson.GetBytes(response.Payload, "steps.0.content.0.text").String(); got != "answer" {
		t.Fatalf("response text = %q, payload = %s", got, response.Payload)
	}
}

func devinBindingTestResponse() []byte {
	frame := protowire.AppendTag(nil, 3, protowire.BytesType)
	frame = protowire.AppendBytes(frame, []byte("answer"))
	usage := protowire.AppendTag(nil, 2, protowire.VarintType)
	usage = protowire.AppendVarint(usage, 2)
	usage = protowire.AppendTag(usage, 3, protowire.VarintType)
	usage = protowire.AppendVarint(usage, 3)
	usage = protowire.AppendTag(usage, 5, protowire.VarintType)
	usage = protowire.AppendVarint(usage, 1)
	frame = protowire.AppendTag(frame, 7, protowire.BytesType)
	frame = protowire.AppendBytes(frame, usage)
	return append(
		helps.WrapConnectEnvelope(frame),
		helps.WrapConnectEnvelopeWithFlag(helps.ConnectFlagEndStream, []byte(`{}`))...,
	)
}
