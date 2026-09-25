package pluginhost

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/pluginabi"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/pluginapi"
)

func TestRegisterManagementRoutesSkipsReservedAndUsesPriority(t *testing.T) {
	high := &managementPluginDouble{
		routes: []pluginapi.ManagementRoute{
			{
				Method: http.MethodGet, Path: "/config", Handler: managementHandlerFunc(
					func(context.Context, pluginapi.ManagementRequest) (pluginapi.ManagementResponse, error) {
						return pluginapi.ManagementResponse{Body: []byte("reserved")}, nil
					},
				),
			},
			{
				Method: http.MethodGet, Path: "/plugins/shared/status", Handler: managementHandlerFunc(
					func(context.Context, pluginapi.ManagementRequest) (pluginapi.ManagementResponse, error) {
						return pluginapi.ManagementResponse{Body: []byte("high")}, nil
					},
				),
			},
		},
	}
	low := &managementPluginDouble{
		routes: []pluginapi.ManagementRoute{
			{
				Method: http.MethodGet, Path: "/plugins/shared/status", Handler: managementHandlerFunc(
					func(context.Context, pluginapi.ManagementRequest) (pluginapi.ManagementResponse, error) {
						return pluginapi.ManagementResponse{Body: []byte("low")}, nil
					},
				),
			},
			{
				Method: http.MethodPost, Path: "plugins/low/run", Handler: managementHandlerFunc(
					func(context.Context, pluginapi.ManagementRequest) (pluginapi.ManagementResponse, error) {
						return pluginapi.ManagementResponse{StatusCode: http.StatusAccepted, Body: []byte("low-only")}, nil
					},
				),
			},
		},
	}
	host := newHostWithRecords(
		capabilityRecord{
			id: "low", priority: 1, plugin: pluginapi.Plugin{Capabilities: pluginapi.Capabilities{ManagementAPI: low}},
		},
		capabilityRecord{
			id: "high", priority: 10,
			plugin: pluginapi.Plugin{Capabilities: pluginapi.Capabilities{ManagementAPI: high}},
		},
	)
	host.RegisterManagementRoutes(
		context.Background(), map[string]struct{}{
			"GET /v0/management/config": {},
		},
	)

	if !host.HasManagementRoute(http.MethodGet, "/v0/management/plugins/shared/status") {
		t.Fatal("HasManagementRoute() = false for registered management route")
	}
	if host.HasManagementRoute(http.MethodGet, "/v0/management/config") {
		t.Fatal("HasManagementRoute() = true for reserved management route")
	}

	req := httptest.NewRequest(http.MethodGet, "/v0/management/plugins/shared/status", nil)
	rec := httptest.NewRecorder()
	if !host.ServeManagementHTTP(rec, req) {
		t.Fatal("ServeManagementHTTP() = false, want true")
	}
	if rec.Body.String() != "high" {
		t.Fatalf("Body = %q, want high", rec.Body.String())
	}

	req = httptest.NewRequest(http.MethodPost, "/v0/management/plugins/low/run", nil)
	rec = httptest.NewRecorder()
	if !host.ServeManagementHTTP(rec, req) {
		t.Fatal("ServeManagementHTTP() for low route = false, want true")
	}
	if rec.Code != http.StatusAccepted || rec.Body.String() != "low-only" {
		t.Fatalf("response = %d %q, want 202 low-only", rec.Code, rec.Body.String())
	}

	req = httptest.NewRequest(http.MethodGet, "/v0/management/config", nil)
	rec = httptest.NewRecorder()
	if host.ServeManagementHTTP(rec, req) {
		t.Fatal("reserved route was served by plugin")
	}
}

func TestServeManagementPreservesRawJSONForLegacyAndSchemaSix(t *testing.T) {
	const rawJSON = `{"prompt":"Analyze <input> & preserve > output"}`
	for _, schemaVersion := range []uint32{0, pluginabi.SchemaVersionRawManagementResponse} {
		t.Run(fmt.Sprintf("schema-%d", schemaVersion), func(t *testing.T) {
			plugin := validTestPlugin("raw-json")
			plugin.Capabilities = pluginapi.Capabilities{ManagementAPI: &managementPluginDouble{}}
			lookup := newTestSymbolLookup(&testPlugin{registerResult: plugin})
			lookup.schemaVersion = schemaVersion
			lookup.schemaVersionSet = true
			lookup.methodHandlers = map[string]func(context.Context, []byte) ([]byte, error){
				pluginabi.MethodManagementRegister: func(context.Context, []byte) ([]byte, error) {
					return marshalRPCResult(rpcManagementRegistrationResponse{Routes: []pluginapi.ManagementRoute{{
						Method: http.MethodGet,
						Path:   "/plugins/raw-json/config",
					}}})
				},
				pluginabi.MethodManagementHandle: func(context.Context, []byte) ([]byte, error) {
					return marshalRPCResult(pluginapi.ManagementResponse{
						Headers: http.Header{"Content-Type": {"application/json"}},
						Body:    []byte(rawJSON),
					})
				},
			}
			registered, errRegister := registerRPCPlugin(
				context.Background(), New(), "raw-json", lookup, pluginabi.MethodPluginRegister, nil,
			)
			if errRegister != nil {
				t.Fatalf("registerRPCPlugin() error = %v", errRegister)
			}
			host := newHostWithRecords(capabilityRecord{id: "raw-json", plugin: registered})
			host.RegisterManagementRoutes(context.Background(), nil)
			recorder := httptest.NewRecorder()
			request := httptest.NewRequest(http.MethodGet, "/v0/management/plugins/raw-json/config", nil)
			if !host.ServeManagementHTTP(recorder, request) {
				t.Fatal("ServeManagementHTTP() = false, want true")
			}
			if recorder.Body.String() != rawJSON {
				t.Fatalf("body = %q, want raw %q", recorder.Body.String(), rawJSON)
			}
		})
	}
}

func TestRegisterManagementRoutesDropsResultsAfterRuntimeInvalidation(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	host := newHostWithRecords(
		capabilityRecord{
			id: "stale-management",
			plugin: pluginapi.Plugin{
				Capabilities: pluginapi.Capabilities{
					ManagementAPI: &managementPluginDouble{
						register: func(
							context.Context,
							pluginapi.ManagementRegistrationRequest,
						) (pluginapi.ManagementRegistrationResponse, error) {
							close(started)
							<-release
							return pluginapi.ManagementRegistrationResponse{
								Routes: []pluginapi.ManagementRoute{
									{
										Method: http.MethodGet,
										Path:   "/plugins/stale/status",
										Handler: managementHandlerFunc(
											func(
												context.Context,
												pluginapi.ManagementRequest,
											) (pluginapi.ManagementResponse, error) {
												return pluginapi.ManagementResponse{}, nil
											},
										),
									},
								},
							}, nil
						},
					},
				},
			},
		},
	)

	done := make(chan struct{})
	go func() {
		host.RegisterManagementRoutes(context.Background(), nil)
		close(done)
	}()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("management registration did not start")
	}
	host.InvalidateRuntime()
	close(release)
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("management registration did not finish")
	}
	if host.HasManagementRoute(http.MethodGet, "/v0/management/plugins/stale/status") {
		t.Fatal("stale management route was published after runtime invalidation")
	}
}

func TestManagementHandlerPanicFusesPlugin(t *testing.T) {
	host := newHostWithRecords(
		capabilityRecord{
			id: "panic",
			plugin: pluginapi.Plugin{
				Capabilities: pluginapi.Capabilities{
					ManagementAPI: &managementPluginDouble{
						routes: []pluginapi.ManagementRoute{
							{
								Method: http.MethodGet,
								Path:   "/plugins/panic",
								Handler: managementHandlerFunc(
									func(context.Context, pluginapi.ManagementRequest) (
										pluginapi.ManagementResponse,
										error,
									) {
										panic("boom")
									},
								),
							},
						},
					},
				},
			},
		},
	)
	host.RegisterManagementRoutes(context.Background(), nil)

	req := httptest.NewRequest(http.MethodGet, "/v0/management/plugins/panic", nil)
	rec := httptest.NewRecorder()
	if !host.ServeManagementHTTP(rec, req) {
		t.Fatal("ServeManagementHTTP() = false, want true")
	}
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", rec.Code)
	}
	if !host.isPluginFused("panic") {
		t.Fatal("plugin was not fused after panic")
	}
}

func TestServeResourceHTTPDispatchesPluginResource(t *testing.T) {
	host := newHostWithRecords(
		capabilityRecord{
			id: "resource",
			plugin: pluginapi.Plugin{
				Capabilities: pluginapi.Capabilities{
					ManagementAPI: &managementPluginDouble{
						resources: []pluginapi.ResourceRoute{
							{
								Path:        "/status",
								Menu:        "Status",
								Description: "Shows plugin status.",
								Handler: managementHandlerFunc(
									func(
										_ context.Context,
										req pluginapi.ManagementRequest,
									) (pluginapi.ManagementResponse, error) {
										if req.Path != "/v0/resource/plugins/resource/status" {
											t.Fatalf(
												"resource request path = %q, want normalized resource path", req.Path,
											)
										}
										if got := req.Headers.Get("Authorization"); got != "" {
											t.Fatalf("resource Authorization header = %q, want empty", got)
										}
										if got := req.Headers.Get("X-Management-Key"); got != "" {
											t.Fatalf("resource X-Management-Key header = %q, want empty", got)
										}
										if got := req.Headers.Get("Cookie"); got != "plugin_state=ok" {
											t.Fatalf("resource Cookie header = %q, want plugin_state=ok", got)
										}
										return pluginapi.ManagementResponse{
											Headers: http.Header{"Content-Type": []string{"text/html; charset=utf-8"}},
											Body:    []byte("<!doctype html><title>resource</title>"),
										}, nil
									},
								),
							},
						},
					},
				},
			},
		},
	)
	host.RegisterManagementRoutes(context.Background(), nil)

	if !host.HasResourceRoute(http.MethodGet, "/v0/resource/plugins/resource/status") {
		t.Fatal("HasResourceRoute() = false for registered resource route")
	}
	if host.HasResourceRoute(http.MethodPost, "/v0/resource/plugins/resource/status") {
		t.Fatal("HasResourceRoute() = true for non-GET resource route")
	}

	req := httptest.NewRequest(http.MethodGet, "/v0/resource/plugins/resource/status", nil)
	req.Header.Set("Authorization", "Bearer management-key")
	req.Header.Set("X-Management-Key", "management-key")
	req.AddCookie(&http.Cookie{Name: "cpa_plugin_resource_session", Value: "resource-session"})
	req.AddCookie(&http.Cookie{Name: "plugin_state", Value: "ok"})
	rec := httptest.NewRecorder()
	if !host.ServeResourceHTTP(rec, req) {
		t.Fatal("ServeResourceHTTP() = false, want true")
	}
	if rec.Code != http.StatusOK || rec.Body.String() != "<!doctype html><title>resource</title>" {
		t.Fatalf("response = %d %q, want 200 html", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("Content-Type"); got != "text/html; charset=utf-8" {
		t.Fatalf("Content-Type = %q, want text/html; charset=utf-8", got)
	}
}

func TestLegacyGETManagementMenuRegistersAsResource(t *testing.T) {
	host := newHostWithRecords(
		capabilityRecord{
			id: "legacy",
			plugin: pluginapi.Plugin{
				Capabilities: pluginapi.Capabilities{
					ManagementAPI: &managementPluginDouble{
						routes: []pluginapi.ManagementRoute{
							{
								Method:      http.MethodGet,
								Path:        "/plugins/legacy/status",
								Menu:        "Legacy Status",
								Description: "Shows legacy plugin status.",
								Handler: managementHandlerFunc(
									func(context.Context, pluginapi.ManagementRequest) (
										pluginapi.ManagementResponse,
										error,
									) {
										return pluginapi.ManagementResponse{Body: []byte("legacy")}, nil
									},
								),
							},
						},
					},
				},
			},
		},
	)
	host.RegisterManagementRoutes(context.Background(), nil)

	managementReq := httptest.NewRequest(http.MethodGet, "/v0/management/plugins/legacy/status", nil)
	managementRec := httptest.NewRecorder()
	if host.ServeManagementHTTP(managementRec, managementReq) {
		t.Fatal("legacy menu route was served as Management API route")
	}

	resourceReq := httptest.NewRequest(http.MethodGet, "/v0/resource/plugins/legacy/status", nil)
	resourceRec := httptest.NewRecorder()
	if !host.ServeResourceHTTP(resourceRec, resourceReq) {
		t.Fatal("legacy menu route was not served as resource route")
	}
	if resourceRec.Body.String() != "legacy" {
		t.Fatalf("resource body = %q, want legacy", resourceRec.Body.String())
	}
}

func TestRegisteredPluginsIncludesResourceMenus(t *testing.T) {
	plugin := &managementPluginDouble{
		routes: []pluginapi.ManagementRoute{
			{
				Method: http.MethodGet,
				Path:   "/plugins/menu/hidden",
				Handler: managementHandlerFunc(
					func(context.Context, pluginapi.ManagementRequest) (pluginapi.ManagementResponse, error) {
						return pluginapi.ManagementResponse{}, nil
					},
				),
			},
		},
		resources: []pluginapi.ResourceRoute{
			{
				Path:        "/status",
				Menu:        "Status",
				Description: "Shows plugin status.",
				Handler: managementHandlerFunc(
					func(context.Context, pluginapi.ManagementRequest) (pluginapi.ManagementResponse, error) {
						return pluginapi.ManagementResponse{}, nil
					},
				),
			},
		},
	}
	host := newHostWithRecords(
		capabilityRecord{
			id: "menu",
			meta: pluginapi.Metadata{
				Name: "menu", Version: "1.0.0", Author: "test",
				GitHubRepository: "https://github.com/router-for-me/CLIProxyAPI",
			},
			plugin: pluginapi.Plugin{Capabilities: pluginapi.Capabilities{ManagementAPI: plugin}},
		},
	)
	host.RegisterManagementRoutes(context.Background(), nil)

	plugins := host.RegisteredPlugins()
	if len(plugins) != 1 {
		t.Fatalf("RegisteredPlugins() len = %d, want 1", len(plugins))
	}
	if len(plugins[0].Menus) != 1 {
		t.Fatalf("RegisteredPlugins()[0].Menus = %#v, want one visible GET menu", plugins[0].Menus)
	}
	menu := plugins[0].Menus[0]
	if menu.Path != "/v0/resource/plugins/menu/status" || menu.Menu != "Status" || menu.Description != "Shows plugin status." {
		t.Fatalf("menu = %#v, want normalized status menu", menu)
	}
}

type managementPluginDouble struct {
	routes    []pluginapi.ManagementRoute
	resources []pluginapi.ResourceRoute
	register  func(
		context.Context,
		pluginapi.ManagementRegistrationRequest,
	) (pluginapi.ManagementRegistrationResponse, error)
}

func (p *managementPluginDouble) RegisterManagement(
	ctx context.Context,
	req pluginapi.ManagementRegistrationRequest,
) (pluginapi.ManagementRegistrationResponse, error) {
	if p.register != nil {
		return p.register(ctx, req)
	}
	return pluginapi.ManagementRegistrationResponse{Routes: p.routes, Resources: p.resources}, nil
}

type managementHandlerFunc func(context.Context, pluginapi.ManagementRequest) (pluginapi.ManagementResponse, error)

func (f managementHandlerFunc) HandleManagement(
	ctx context.Context,
	req pluginapi.ManagementRequest,
) (pluginapi.ManagementResponse, error) {
	return f(ctx, req)
}
