package pluginhost

import (
	"context"
	"testing"

	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/pluginapi"
)

func TestRequestInterceptorPreservesMetadataViewLengths(t *testing.T) {
	shared := []string{"first", "second", "third"}
	views := [][]string{shared[:1], shared}
	called := false
	host := newHostWithRecords(
		capabilityRecord{
			id: "metadata-view-fixture",
			plugin: pluginapi.Plugin{
				Capabilities: pluginapi.Capabilities{
					RequestInterceptor: requestInterceptorFunc(
						func(
							_ context.Context,
							request pluginapi.RequestInterceptRequest,
						) (pluginapi.RequestInterceptResponse, error) {
							called = true
							cloned, ok := request.Metadata["views"].([][]string)
							if !ok || len(cloned) != 2 || len(cloned[0]) != 1 || len(cloned[1]) != 3 {
								t.Errorf("metadata views changed shape: %#v", request.Metadata["views"])
								return pluginapi.RequestInterceptResponse{}, nil
							}
							cloned[0][0] = "plugin changed short view"
							cloned[1][2] = "plugin changed long view"
							return pluginapi.RequestInterceptResponse{}, nil
						},
					),
				},
			},
		},
	)
	host.InterceptRequestBeforeAuth(
		t.Context(), pluginapi.RequestInterceptRequest{Metadata: map[string]any{"views": views}},
	)
	if !called {
		t.Fatal("metadata did not reach the active interceptor")
	}
	if shared[0] != "first" || shared[2] != "third" || len(views[0]) != 1 || len(views[1]) != 3 {
		t.Fatalf("plugin changed caller metadata: %#v", views)
	}
}
