//go:build cgo && (linux || darwin || freebsd)

package pluginhost

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/pluginabi"
)

func TestDynamicLibraryClientPreservesNativeErrorEnvelope(t *testing.T) {
	dir := t.TempDir()
	source := filepath.Join(dir, "fixture.c")
	library := filepath.Join(dir, "fixture.so")
	code := `#include <stdint.h>
#include <stdlib.h>
#include <string.h>

typedef struct { void* ptr; size_t len; } buffer;
typedef struct { uint32_t version; void* ctx; void* call; void* free_buffer; } host_api;
typedef struct { uint32_t version; void* call; void* free_buffer; void* shutdown; } plugin_api;

static int call(const char* method, const uint8_t* request, size_t length, buffer* response) {
    const char* payload = "{\"ok\":false,\"error\":{\"code\":\"insufficient_quota\",\"message\":\"plan limit reached\",\"http_status\":403}}";
    if (strcmp(method, "fixture.invalid") == 0) {
        payload = "invalid JSON";
    } else if (strcmp(method, "fixture.success") == 0) {
        payload = "{\"ok\":true,\"result\":{}}";
    }
    response->len = strlen(payload);
    response->ptr = malloc(response->len);
    if (response->ptr == NULL) {
        response->len = 0;
        return 2;
    }
    memcpy(response->ptr, payload, response->len);
    return 1;
}

static void free_buffer(void* ptr, size_t length) { free(ptr); }

int cliproxy_plugin_init(const host_api* host, plugin_api* plugin) {
    plugin->version = host->version;
    plugin->call = (void*)call;
    plugin->free_buffer = (void*)free_buffer;
    plugin->shutdown = NULL;
    return 0;
}
`
	if err := os.WriteFile(source, []byte(code), 0o600); err != nil {
		t.Fatal(err)
	}
	flags := []string{"-shared", "-fPIC", "-o", library, source}
	if runtime.GOOS == "darwin" {
		flags[0] = "-dynamiclib"
	}
	if output, err := exec.Command("cc", flags...).CombinedOutput(); err != nil {
		t.Fatalf("compile native fixture: %v\n%s", err, output)
	}
	client, err := (dynamicLibraryLoader{}).Open(pluginFile{ID: "native-error-fixture", Path: library}, New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(client.Shutdown)
	_, err = callPlugin[rpcEmptyResponse](context.Background(), client, pluginabi.MethodExecutorExecute, rpcEmptyResponse{})
	if err == nil || err.Error() != "plan limit reached" {
		t.Fatalf("native plugin error = %v, want plan limit reached", err)
	}
	status, ok := err.(interface{ StatusCode() int })
	if !ok || status.StatusCode() != 403 {
		t.Fatalf("native plugin error %T does not directly expose status 403", err)
	}
	for _, method := range []string{"fixture.invalid", "fixture.success"} {
		if _, err := client.Call(context.Background(), method, nil); err == nil || !strings.Contains(err.Error(), "returned 1") {
			t.Fatalf("non-error envelope for %s accepted despite native failure: %v", method, err)
		}
	}
}
