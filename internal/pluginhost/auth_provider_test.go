package pluginhost

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/pluginabi"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/pluginapi"
)

func TestAuthProviderDiscovery(t *testing.T) {
	host := newHostWithRecords(
		capabilityRecord{
			id:       "high",
			priority: 20,
			plugin: pluginapi.Plugin{
				Capabilities: pluginapi.Capabilities{
					AuthProvider: fakeAuthProvider{identifier: " High-Provider "},
				},
			},
		},
		capabilityRecord{
			id:       "low",
			priority: 10,
			plugin: pluginapi.Plugin{
				Capabilities: pluginapi.Capabilities{
					AuthProvider: fakeAuthProvider{identifier: "low-provider"},
				},
			},
		},
		capabilityRecord{
			id: "missing-auth-provider",
			plugin: pluginapi.Plugin{
				Capabilities: pluginapi.Capabilities{
					ModelRegistrar: staticModelRegistrar("provider", "model"),
				},
			},
		},
	)

	identifiers := host.authProviderIdentifiers()
	if len(identifiers) != 2 || identifiers[0] != "high-provider" || identifiers[1] != "low-provider" {
		t.Fatalf("AuthProviderIdentifiers() = %#v, want sorted normalized providers", identifiers)
	}
	if !host.HasAuthProvider(" HIGH-PROVIDER ") {
		t.Fatal("HasAuthProvider(high-provider) = false, want true")
	}
	if host.HasAuthProvider("missing-provider") {
		t.Fatal("HasAuthProvider(missing-provider) = true, want false")
	}
}

func TestParseAuthDefaultsProviderFromRequest(t *testing.T) {
	host := newHostWithRecords(
		capabilityRecord{
			id: "auth-plugin",
			plugin: pluginapi.Plugin{
				Capabilities: pluginapi.Capabilities{
					AuthProvider: fakeAuthProvider{
						identifier: "plugin-provider",
						parseAuth: func(
							ctx context.Context,
							req pluginapi.AuthParseRequest,
						) (pluginapi.AuthParseResponse, error) {
							return pluginapi.AuthParseResponse{
								Handled: true,
								Auth: pluginapi.AuthData{
									ID: "auth-1",
								},
							}, nil
						},
					},
				},
			},
		},
	)

	auth, handled, errParse := host.ParseAuth(
		context.Background(), pluginapi.AuthParseRequest{Provider: "plugin-provider"},
	)
	if errParse != nil {
		t.Fatalf("ParseAuth() error = %v", errParse)
	}
	if !handled || auth == nil {
		t.Fatalf("ParseAuth() handled=%t auth=%#v, want parsed auth", handled, auth)
	}
	if auth.Provider != "plugin-provider" || auth.Metadata["type"] != "plugin-provider" {
		t.Fatalf("ParseAuth() auth = %#v, want plugin-provider defaults", auth)
	}
}

func TestParseAuthDefaultsProviderFromAuthProviderIdentifier(t *testing.T) {
	seenProvider := ""
	host := newHostWithRecords(
		capabilityRecord{
			id: "auth-plugin",
			plugin: pluginapi.Plugin{
				Capabilities: pluginapi.Capabilities{
					AuthProvider: fakeAuthProvider{
						identifier: "Plugin-Provider",
						parseAuth: func(
							ctx context.Context,
							req pluginapi.AuthParseRequest,
						) (pluginapi.AuthParseResponse, error) {
							seenProvider = req.Provider
							return pluginapi.AuthParseResponse{
								Handled: true,
								Auth: pluginapi.AuthData{
									ID: "auth-1",
								},
							}, nil
						},
					},
				},
			},
		},
	)

	auth, handled, errParse := host.ParseAuth(context.Background(), pluginapi.AuthParseRequest{})
	if errParse != nil {
		t.Fatalf("ParseAuth() error = %v", errParse)
	}
	if !handled || auth == nil {
		t.Fatalf("ParseAuth() handled=%t auth=%#v, want parsed auth", handled, auth)
	}
	if seenProvider != "plugin-provider" {
		t.Fatalf("plugin parse request provider = %q, want plugin-provider", seenProvider)
	}
	if auth.Provider != "plugin-provider" || auth.Metadata["type"] != "plugin-provider" {
		t.Fatalf("ParseAuth() auth = %#v, want identifier provider fallback", auth)
	}
}

func TestParseAuthsExpandsMultiplePluginAuths(t *testing.T) {
	host := newHostWithRecords(
		capabilityRecord{
			id: "multi-auth-plugin",
			plugin: pluginapi.Plugin{
				Capabilities: pluginapi.Capabilities{
					AuthProvider: fakeAuthProvider{
						identifier: "plugin-provider",
						parseAuth: func(
							context.Context,
							pluginapi.AuthParseRequest,
						) (pluginapi.AuthParseResponse, error) {
							return pluginapi.AuthParseResponse{
								Handled: true,
								Auths: []pluginapi.AuthData{
									{
										ID:          "plugin-a",
										StorageJSON: []byte(`{"type":"plugin-provider","account":"a"}`),
									},
									{
										ID:          "plugin-b",
										StorageJSON: []byte(`{"type":"plugin-provider","account":"b"}`),
										Metadata:    map[string]any{"account": "b", "request-retry": 2},
									},
								},
							}, nil
						},
					},
				},
			},
		},
	)

	auths, handled, errParse := host.ParseAuths(
		context.Background(),
		pluginapi.AuthParseRequest{Provider: "plugin-provider"},
	)
	if errParse != nil || !handled || len(auths) != 2 {
		t.Fatalf("ParseAuths() handled=%t len=%d err=%v", handled, len(auths), errParse)
	}
	for index, wantID := range []string{"plugin-a", "plugin-b"} {
		if auths[index].ID != wantID || auths[index].Provider != "plugin-provider" {
			t.Fatalf("auth %d = %#v, want id=%s provider fallback", index, auths[index], wantID)
		}
	}
	if auths[1].Metadata["account"] != "b" || auths[1].Metadata["request_retry"] != 2 {
		t.Fatalf("second auth metadata = %#v", auths[1].Metadata)
	}
	if _, legacy := auths[1].Metadata["request-retry"]; legacy {
		t.Fatalf("second auth retained legacy metadata = %#v", auths[1].Metadata)
	}

	first, handled, errFirst := host.ParseAuth(
		context.Background(),
		pluginapi.AuthParseRequest{Provider: "plugin-provider"},
	)
	if errFirst != nil || !handled || first == nil || first.ID != "plugin-a" {
		t.Fatalf("ParseAuth() first=%#v handled=%t err=%v", first, handled, errFirst)
	}
}

func TestParseAuthsPreservesLegacyAndExplicitEmptyContracts(t *testing.T) {
	for _, testCase := range []struct {
		name     string
		response pluginapi.AuthParseResponse
		wantIDs  []string
	}{
		{
			name: "legacy single auth",
			response: pluginapi.AuthParseResponse{
				Handled: true,
				Auth:    pluginapi.AuthData{ID: "legacy-auth"},
			},
			wantIDs: []string{"legacy-auth"},
		},
		{
			name: "explicit handled empty",
			response: pluginapi.AuthParseResponse{
				Handled: true,
				Auth:    pluginapi.AuthData{ID: "must-not-fallback"},
				Auths:   []pluginapi.AuthData{},
			},
			wantIDs: []string{},
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			host := newHostWithRecords(
				capabilityRecord{
					id: "auth-plugin",
					plugin: pluginapi.Plugin{
						Capabilities: pluginapi.Capabilities{
							AuthProvider: fakeAuthProvider{
								identifier: "plugin-provider",
								parseAuth: func(
									context.Context,
									pluginapi.AuthParseRequest,
								) (pluginapi.AuthParseResponse, error) {
									return testCase.response, nil
								},
							},
						},
					},
				},
			)
			auths, handled, errParse := host.ParseAuths(
				context.Background(),
				pluginapi.AuthParseRequest{Provider: "plugin-provider"},
			)
			if errParse != nil || !handled || len(auths) != len(testCase.wantIDs) {
				t.Fatalf("ParseAuths() handled=%t len=%d err=%v", handled, len(auths), errParse)
			}
			for index, wantID := range testCase.wantIDs {
				if auths[index].ID != wantID {
					t.Fatalf("auth %d ID = %q, want %q", index, auths[index].ID, wantID)
				}
			}
		})
	}
}

func TestStartLoginPassesProviderBaseURLHostAndHTTPClient(t *testing.T) {
	authDir := t.TempDir()
	expiresAt := time.Now().Add(time.Minute).UTC()
	called := false
	host := newHostWithRecords(
		capabilityRecord{
			id: "auth-plugin",
			plugin: pluginapi.Plugin{
				Capabilities: pluginapi.Capabilities{
					AuthProvider: fakeAuthProvider{
						identifier: "plugin-provider",
						startLogin: func(
							ctx context.Context,
							req pluginapi.AuthLoginStartRequest,
						) (pluginapi.AuthLoginStartResponse, error) {
							called = true
							if req.Provider != "plugin-provider" || req.BaseURL != "http://localhost:8080/login" {
								t.Fatalf("StartLogin request = %#v, want provider/baseURL", req)
							}
							if req.Host.AuthDir != authDir || req.Host.ProxyURL != "http://proxy.local" || !req.Host.ForceModelPrefix {
								t.Fatalf("StartLogin host = %#v, want configured summary", req.Host)
							}
							if req.HTTPClient == nil {
								t.Fatal("StartLogin HTTPClient = nil, want host HTTP bridge")
							}
							return pluginapi.AuthLoginStartResponse{
								Provider:  req.Provider,
								URL:       "http://provider/login",
								State:     "state-1",
								ExpiresAt: expiresAt,
							}, nil
						},
					},
				},
			},
		},
	)
	host.runtimeConfig = &config.Config{
		ProxyURL:         "http://proxy.local",
		ForceModelPrefix: true,
		AuthDir:          authDir,
	}

	resp, handled, errStart := host.startLogin(context.Background(), " Plugin-Provider ", "http://localhost:8080/login")
	if errStart != nil {
		t.Fatalf("StartLogin() error = %v", errStart)
	}
	if !handled || !called {
		t.Fatalf("StartLogin() handled=%t called=%t, want handled call", handled, called)
	}
	if resp.Provider != "plugin-provider" || resp.URL != "http://provider/login" || resp.State != "state-1" || !resp.ExpiresAt.Equal(expiresAt) {
		t.Fatalf("StartLogin() response = %#v, want plugin response", resp)
	}
}

func TestStartLoginPassesSingleAndMultiValueMetadataByDefensiveCopy(t *testing.T) {
	metadata := map[string]any{
		"single": "one",
		"multi":  []string{"first", "second"},
	}
	var captured map[string]any
	host := newHostWithRecords(capabilityRecord{
		id: "auth-plugin",
		plugin: pluginapi.Plugin{Capabilities: pluginapi.Capabilities{
			AuthProvider: fakeAuthProvider{
				identifier: "plugin-provider",
				startLogin: func(
					_ context.Context,
					req pluginapi.AuthLoginStartRequest,
				) (pluginapi.AuthLoginStartResponse, error) {
					captured = req.Metadata
					req.Metadata["single"] = "mutated"
					req.Metadata["multi"].([]string)[0] = "mutated"
					return pluginapi.AuthLoginStartResponse{}, nil
				},
			},
		}},
	})

	_, handled, errStart := host.startLogin(
		context.Background(), "plugin-provider", "http://localhost/login", metadata,
	)
	if errStart != nil || !handled {
		t.Fatalf("startLogin() handled=%t error=%v", handled, errStart)
	}
	if captured == nil {
		t.Fatal("StartLogin metadata = nil")
	}
	if metadata["single"] != "one" || metadata["multi"].([]string)[0] != "first" {
		t.Fatalf("caller metadata was mutated: %#v", metadata)
	}
}

func TestRPCStartLoginPreservesSingleAndMultiValueMetadata(t *testing.T) {
	plugin := validTestPlugin("rpc-auth")
	plugin.Capabilities.AuthProvider = fakeAuthProvider{identifier: "plugin-provider"}
	lookup := newTestSymbolLookup(&testPlugin{registerResult: plugin})
	var captured rpcAuthLoginStartRequest
	lookup.methodHandlers = map[string]func(context.Context, []byte) ([]byte, error){
		pluginabi.MethodAuthLoginStart: func(_ context.Context, raw []byte) ([]byte, error) {
			if errUnmarshal := json.Unmarshal(raw, &captured); errUnmarshal != nil {
				return nil, errUnmarshal
			}
			captured.Metadata["single"] = "mutated"
			captured.Metadata["multi"].([]any)[0] = "mutated"
			return marshalRPCResult(pluginapi.AuthLoginStartResponse{State: "state"})
		},
	}
	registered, errRegister := registerRPCPlugin(
		context.Background(), New(), "rpc-auth", lookup, pluginabi.MethodPluginRegister, nil,
	)
	if errRegister != nil {
		t.Fatalf("registerRPCPlugin() error = %v", errRegister)
	}
	metadata := map[string]any{
		"single": "one",
		"multi":  []string{"first", "second"},
	}
	response, errStart := registered.Capabilities.AuthProvider.StartLogin(
		context.Background(), pluginapi.AuthLoginStartRequest{
			Provider: "plugin-provider",
			Metadata: metadata,
		},
	)
	if errStart != nil {
		t.Fatalf("StartLogin() error = %v", errStart)
	}
	if response.State != "state" {
		t.Fatalf("StartLogin() state = %q, want state", response.State)
	}
	if captured.Metadata["single"] != "mutated" || len(captured.Metadata["multi"].([]any)) != 2 {
		t.Fatalf("RPC metadata = %#v", captured.Metadata)
	}
	if metadata["single"] != "one" || metadata["multi"].([]string)[0] != "first" {
		t.Fatalf("caller metadata was mutated through RPC: %#v", metadata)
	}
}

func TestPollLoginPassesProviderStateHostAndHTTPClient(t *testing.T) {
	authDir := t.TempDir()
	called := false
	host := newHostWithRecords(
		capabilityRecord{
			id: "auth-plugin",
			plugin: pluginapi.Plugin{
				Capabilities: pluginapi.Capabilities{
					AuthProvider: fakeAuthProvider{
						identifier: "plugin-provider",
						pollLogin: func(
							ctx context.Context,
							req pluginapi.AuthLoginPollRequest,
						) (pluginapi.AuthLoginPollResponse, error) {
							called = true
							if req.Provider != "plugin-provider" || req.State != "state-1" {
								t.Fatalf("PollLogin request = %#v, want provider/state", req)
							}
							if req.Host.AuthDir != authDir || req.Host.ProxyURL != "http://proxy.local" || !req.Host.ForceModelPrefix {
								t.Fatalf("PollLogin host = %#v, want configured summary", req.Host)
							}
							if req.HTTPClient == nil {
								t.Fatal("PollLogin HTTPClient = nil, want host HTTP bridge")
							}
							return pluginapi.AuthLoginPollResponse{
								Status:  pluginapi.AuthLoginStatusSuccess,
								Message: "done",
								Auth: pluginapi.AuthData{
									Provider: "plugin-provider",
									ID:       "auth-1",
								},
							}, nil
						},
					},
				},
			},
		},
	)
	host.runtimeConfig = &config.Config{
		ProxyURL:         "http://proxy.local",
		ForceModelPrefix: true,
		AuthDir:          authDir,
	}

	resp, handled, errPoll := host.pollLogin(context.Background(), " Plugin-Provider ", " state-1 ")
	if errPoll != nil {
		t.Fatalf("PollLogin() error = %v", errPoll)
	}
	if !handled || !called {
		t.Fatalf("PollLogin() handled=%t called=%t, want handled call", handled, called)
	}
	if resp.Status != pluginapi.AuthLoginStatusSuccess || resp.Message != "done" || resp.Auth.ID != "auth-1" {
		t.Fatalf("PollLogin() response = %#v, want plugin response", resp)
	}
}

func TestHostAuthDataToCoreAuthRejectsMissingProviderAndUsesAuthDir(t *testing.T) {
	authDir := t.TempDir()
	host := New()
	host.runtimeConfig = &config.Config{AuthDir: authDir}
	path := filepath.Join(authDir, "nested", "auth.json")

	if auth := host.authDataToCoreAuth(pluginapi.AuthData{ID: "auth-1"}, path, "auth.json"); auth != nil {
		t.Fatalf("AuthDataToCoreAuth() = %#v, want nil for missing provider", auth)
	}
	auth := host.authDataToCoreAuth(pluginapi.AuthData{Provider: "Plugin-Provider"}, path, "")
	if auth == nil {
		t.Fatal("AuthDataToCoreAuth() = nil, want auth")
	}
	if auth.Provider != "plugin-provider" || auth.ID != "nested/auth.json" {
		t.Fatalf("AuthDataToCoreAuth() auth = %#v, want normalized provider and relative ID", auth)
	}
	if auth.Metadata["type"] != "plugin-provider" || auth.Attributes["path"] != path || auth.Attributes["source"] != path {
		t.Fatalf(
			"AuthDataToCoreAuth() metadata=%#v attributes=%#v, want path/source/type", auth.Metadata, auth.Attributes,
		)
	}
}

func TestPluginTokenStorageMergesRawMetadataAndProviderType(t *testing.T) {
	storage := &pluginTokenStorage{
		provider: "plugin-provider",
		rawJSON:  []byte(`{"old":"value","type":"old-provider"}`),
	}
	storage.SetMetadata(
		map[string]any{
			"new": "value",
			"old": "override",
		},
	)

	raw := storage.RawJSON()
	var decoded map[string]any
	if errUnmarshal := json.Unmarshal(raw, &decoded); errUnmarshal != nil {
		t.Fatalf("RawJSON() decode error = %v", errUnmarshal)
	}
	if decoded["old"] != "override" || decoded["new"] != "value" || decoded["type"] != "plugin-provider" {
		t.Fatalf("RawJSON() decoded = %#v, want merged metadata and provider type", decoded)
	}

	path := filepath.Join(t.TempDir(), "auth.json")
	if errSave := storage.SaveTokenToFile(path); errSave != nil {
		t.Fatalf("SaveTokenToFile() error = %v", errSave)
	}
	saved, errReadFile := os.ReadFile(path)
	if errReadFile != nil {
		t.Fatalf("ReadFile(saved token) error = %v", errReadFile)
	}
	decoded = nil
	if errUnmarshal := json.Unmarshal(saved, &decoded); errUnmarshal != nil {
		t.Fatalf("saved token decode error = %v", errUnmarshal)
	}
	if decoded["old"] != "override" || decoded["new"] != "value" || decoded["type"] != "plugin-provider" {
		t.Fatalf("saved token decoded = %#v, want merged metadata and provider type", decoded)
	}
}

func TestPluginTokenStorageSkipsUnchangedFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "auth.json")
	if errWriteFile := os.WriteFile(
		path, []byte(`{"disabled":false,"token":"secret","type":"plugin-provider"}`), 0o600,
	); errWriteFile != nil {
		t.Fatalf("WriteFile() error = %v", errWriteFile)
	}
	before, errStatBefore := os.Stat(path)
	if errStatBefore != nil {
		t.Fatalf("Stat(before) error = %v", errStatBefore)
	}
	storage := &pluginTokenStorage{
		provider: "plugin-provider",
		rawJSON:  []byte(`{"token":"secret"}`),
	}
	storage.SetMetadata(map[string]any{"disabled": false})

	if errSave := storage.SaveTokenToFile(path); errSave != nil {
		t.Fatalf("SaveTokenToFile() error = %v", errSave)
	}
	after, errStatAfter := os.Stat(path)
	if errStatAfter != nil {
		t.Fatalf("Stat(after) error = %v", errStatAfter)
	}
	if !os.SameFile(before, after) {
		t.Fatal("SaveTokenToFile() replaced unchanged auth file, want write skipped")
	}
}

func TestPluginTokenStorageRejectsEmptyPayload(t *testing.T) {
	storage := &pluginTokenStorage{}
	if raw := storage.RawJSON(); raw != nil {
		t.Fatalf("RawJSON() = %q, want nil for empty payload", raw)
	}
	if errSave := storage.SaveTokenToFile(filepath.Join(t.TempDir(), "auth.json")); errSave == nil {
		t.Fatal("SaveTokenToFile() error = nil, want empty payload error")
	}
}
