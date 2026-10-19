//go:build windows

package pluginhost

import (
	"context"
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"unsafe"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/pluginabi"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/pluginapi"
	"golang.org/x/sys/windows"
)

func TestShadowPluginDirIsProcessScoped(t *testing.T) {
	dir, errDir := shadowPluginDir()
	if errDir != nil {
		t.Fatalf("shadowPluginDir() error = %v", errDir)
	}
	want := filepath.Join(os.TempDir(), "cliproxy-pluginhost", fmt.Sprintf("pid-%d", os.Getpid()))
	if dir != want {
		t.Fatalf("shadowPluginDir() = %q, want %q", dir, want)
	}
}

func TestShadowCopyPluginUsesUniquePathForSameContent(t *testing.T) {
	dir := t.TempDir()
	source := filepath.Join(t.TempDir(), "alpha.dll")
	content := []byte("plugin-v1")
	if errWrite := os.WriteFile(source, content, 0o644); errWrite != nil {
		t.Fatalf("WriteFile() error = %v", errWrite)
	}
	file := pluginFile{ID: "alpha", Path: source}

	first, errFirst := shadowCopyPluginToDir(file, dir)
	if errFirst != nil {
		t.Fatalf("shadowCopyPluginToDir() first error = %v", errFirst)
	}
	second, errSecond := shadowCopyPluginToDir(file, dir)
	if errSecond != nil {
		t.Fatalf("shadowCopyPluginToDir() second error = %v", errSecond)
	}

	if second == first {
		t.Fatalf("second shadow path reused active module path %q", first)
	}
	for _, path := range []string{first, second} {
		gotContent, errRead := os.ReadFile(path)
		if errRead != nil {
			t.Fatalf("ReadFile(%s) error = %v", path, errRead)
		}
		if string(gotContent) != string(content) {
			t.Fatalf("shadow content = %q, want %q", gotContent, content)
		}
		if name := filepath.Base(path); !strings.HasPrefix(name, shadowPluginPrefix+"alpha-") {
			t.Fatalf("shadow file name = %q, want unique alpha prefix", name)
		}
	}
	if count := countShadowPluginFiles(t, dir); count != 2 {
		t.Fatalf("shadow file count = %d, want 2 isolated modules", count)
	}
}

func TestShadowCopyPluginCreatesNewPathForChangedContent(t *testing.T) {
	dir := t.TempDir()
	source := filepath.Join(t.TempDir(), "alpha.dll")
	file := pluginFile{ID: "alpha", Path: source}
	if errWrite := os.WriteFile(source, []byte("plugin-v1"), 0o644); errWrite != nil {
		t.Fatalf("WriteFile() v1 error = %v", errWrite)
	}
	first, errFirst := shadowCopyPluginToDir(file, dir)
	if errFirst != nil {
		t.Fatalf("shadowCopyPluginToDir() v1 error = %v", errFirst)
	}

	if errWrite := os.WriteFile(source, []byte("plugin-v2"), 0o644); errWrite != nil {
		t.Fatalf("WriteFile() v2 error = %v", errWrite)
	}
	second, errSecond := shadowCopyPluginToDir(file, dir)
	if errSecond != nil {
		t.Fatalf("shadowCopyPluginToDir() v2 error = %v", errSecond)
	}

	if second == first {
		t.Fatalf("second shadow path reused %q after content changed", second)
	}
	if count := countShadowPluginFiles(t, dir); count != 2 {
		t.Fatalf("shadow file count = %d, want 2 versions", count)
	}
}

func TestShadowCopyPluginRejectsSnapshotDigestMismatch(t *testing.T) {
	dir := t.TempDir()
	source := filepath.Join(t.TempDir(), "alpha.dll")
	content := []byte("plugin-v1")
	if errWrite := os.WriteFile(source, content, 0o644); errWrite != nil {
		t.Fatalf("WriteFile() source error = %v", errWrite)
	}

	gotPath, errCopy := shadowCopyPluginToDir(
		pluginFile{ID: "alpha", Path: source, digest: strings.Repeat("0", sha256.Size*2)},
		dir,
	)
	if errCopy == nil || gotPath != "" {
		t.Fatalf("shadowCopyPluginToDir() = (%q, %v), want digest mismatch", gotPath, errCopy)
	}
	if count := countShadowPluginFiles(t, dir); count != 0 {
		t.Fatalf("shadow file count = %d, want rejected copy removed", count)
	}
}

func TestHostWindowsRejectedCandidateRequiresRestartBeforeRetry(t *testing.T) {
	loader := newTestSymbolLoader()
	lookup := newTestSymbolLookup(&testPlugin{})
	lookup.registerOverride = func([]byte) pluginapi.Plugin { return pluginapi.Plugin{} }
	loader.lookups["alpha"] = lookup
	host := newForTest(loader)
	cfg := &config.Config{
		Plugins: config.PluginsConfig{
			Enabled: true,
			Dir:     makePluginDir(t, "alpha"),
		},
	}
	if errApply := host.ApplyConfig(context.Background(), cfg); errApply != nil {
		t.Fatalf("initial ApplyConfig() error = %v", errApply)
	}
	if errApply := host.ApplyConfig(context.Background(), cfg); errApply == nil ||
		!strings.Contains(errApply.Error(), "requires process restart") {
		t.Fatalf("retry ApplyConfig() error = %v, want restart requirement", errApply)
	}
	if loader.openCalls != 1 {
		t.Fatalf("Open calls = %d, want one DLL load attempt per process", loader.openCalls)
	}
}

func TestHostWindowsActivePluginRequiresRestartForReplacement(t *testing.T) {
	loader := newTestSymbolLoader()
	loader.lookups["alpha"] = newTestSymbolLookup(&testPlugin{registerResult: validTestPlugin("alpha")})
	host := newForTest(loader)
	cfg := &config.Config{
		Plugins: config.PluginsConfig{
			Enabled: true,
			Dir:     makePluginDir(t, "alpha"),
		},
	}
	if errApply := host.ApplyConfig(context.Background(), cfg); errApply != nil {
		t.Fatalf("initial ApplyConfig() error = %v", errApply)
	}
	t.Cleanup(host.shutdownAll)
	enabled := true
	cfg.Plugins.Configs = map[string]config.PluginInstanceConfig{
		"alpha": {Enabled: &enabled, Priority: 1},
	}
	if errApply := host.ApplyConfig(context.Background(), cfg); errApply == nil ||
		!strings.Contains(errApply.Error(), "requires process restart") {
		t.Fatalf("replacement ApplyConfig() error = %v, want restart requirement", errApply)
	}
	if loader.openCalls != 1 || !host.pluginLoaded("alpha") {
		t.Fatalf("replacement load calls = %d, active = %v", loader.openCalls, host.pluginLoaded("alpha"))
	}
}

func TestHostWindowsLoadAttemptIsSharedAcrossHosts(t *testing.T) {
	const pluginID = "process-load-attempt"
	processPluginLoadAttempts.attempts.Delete(pluginID)
	t.Cleanup(func() { processPluginLoadAttempts.attempts.Delete(pluginID) })
	firstLoader := newTestSymbolLoader()
	firstLoader.lookups[pluginID] = newTestSymbolLookup(
		&testPlugin{registerResult: validTestPlugin(pluginID)},
	)
	firstHost := New()
	firstHost.loader = firstLoader
	cfg := &config.Config{
		Plugins: config.PluginsConfig{
			Enabled: true,
			Dir:     makePluginDir(t, pluginID),
		},
	}
	if errApply := firstHost.ApplyConfig(context.Background(), cfg); errApply != nil {
		t.Fatalf("first ApplyConfig() error = %v", errApply)
	}
	t.Cleanup(firstHost.shutdownAll)

	secondLoader := newTestSymbolLoader()
	secondLoader.lookups[pluginID] = newTestSymbolLookup(
		&testPlugin{registerResult: validTestPlugin(pluginID)},
	)
	secondHost := New()
	secondHost.loader = secondLoader
	if errApply := secondHost.ApplyConfig(context.Background(), cfg); errApply == nil ||
		!strings.Contains(errApply.Error(), "requires process restart") {
		t.Fatalf("second ApplyConfig() error = %v, want restart requirement", errApply)
	}
	if firstLoader.openCalls != 1 || secondLoader.openCalls != 0 {
		t.Fatalf(
			"Open calls first=%d second=%d, want 1/0",
			firstLoader.openCalls,
			secondLoader.openCalls,
		)
	}
}

func TestRemoveStaleShadowPluginsOnlyRemovesShadowFiles(t *testing.T) {
	dir := t.TempDir()
	stale := filepath.Join(dir, shadowPluginPrefix+"alpha-deadbeef.dll")
	temp := filepath.Join(dir, shadowPluginTempPrefix+"alpha-temp.dll")
	keep := filepath.Join(dir, "keep.dll")
	for _, path := range []string{stale, temp, keep} {
		if errWrite := os.WriteFile(path, []byte("x"), 0o644); errWrite != nil {
			t.Fatalf("WriteFile(%s) error = %v", path, errWrite)
		}
	}

	removeStaleShadowPlugins(dir)

	for _, path := range []string{stale, temp} {
		if _, errStat := os.Stat(path); !os.IsNotExist(errStat) {
			t.Fatalf("Stat(%s) error = %v, want not exist", path, errStat)
		}
	}
	if _, errStat := os.Stat(keep); errStat != nil {
		t.Fatalf("Stat(%s) error = %v, want kept", keep, errStat)
	}
}

func countShadowPluginFiles(t *testing.T, dir string) int {
	t.Helper()
	entries, errRead := os.ReadDir(dir)
	if errRead != nil {
		t.Fatalf("ReadDir(%s) error = %v", dir, errRead)
	}
	count := 0
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), shadowPluginPrefix) {
			count++
		}
		if strings.HasPrefix(entry.Name(), shadowPluginTempPrefix) {
			t.Fatalf("temporary shadow file was not cleaned up: %s", entry.Name())
		}
	}
	return count
}

var testReentrantHostCallback uintptr

func TestDynamicLibraryClientCallSurvivesReentrantCallbackStackGrowth(t *testing.T) {
	testReentrantHostCallback = syscall.NewCallback(testGrowHostCallbackStack)
	client := newGuardedPluginClient(&dynamicLibraryClient{api: windowsPluginAPI{
		call:       syscall.NewCallback(testReentrantPluginCall),
		freeBuffer: syscall.NewCallback(testReentrantPluginFree),
	}})
	t.Cleanup(client.Shutdown)

	got, errCall := client.Call(context.Background(), "model.route", []byte(`{}`))
	if errCall != nil {
		t.Fatalf("Call() error = %v", errCall)
	}
	want := `{"ok":true,"result":{"Handled":true}}`
	if string(got) != want {
		t.Fatalf("Call() response = %q, want %q", got, want)
	}
}

func testReentrantPluginCall(_, _, _, responsePtr uintptr) uintptr {
	if testReentrantHostCallback == 0 || responsePtr == 0 {
		return 1
	}
	_, _, _ = syscall.SyscallN(testReentrantHostCallback)

	raw := []byte(`{"ok":true,"result":{"Handled":true}}`)
	mem, errAlloc := windows.LocalAlloc(windows.LMEM_FIXED, uint32(len(raw)))
	if errAlloc != nil || mem == 0 {
		return 1
	}
	copy(unsafe.Slice((*byte)(unsafe.Pointer(mem)), len(raw)), raw)
	response := (*windowsBuffer)(unsafe.Pointer(responsePtr))
	response.ptr = mem
	response.len = uintptr(len(raw))
	return 0
}

func testReentrantPluginFree(ptr, _ uintptr) uintptr {
	if ptr != 0 {
		_, _ = windows.LocalFree(windows.Handle(ptr))
	}
	return 0
}

func testGrowHostCallbackStack() uintptr {
	return uintptr(testGrowStack(64))
}

//go:noinline
func testGrowStack(depth int) int {
	var padding [1024]byte
	for index := range padding {
		padding[index] = byte(index + depth)
	}
	if depth == 0 {
		return int(padding[0])
	}
	return testGrowStack(depth-1) + int(padding[depth%len(padding)])
}

var (
	testLivenessFinalizedDuringCallback atomic.Bool
	testLivenessCallbackReadCorrupted   atomic.Bool
	testLivenessCurrentOwnerID          atomic.Uint64
	testLivenessFinalizedOwnerID        atomic.Uint64
)

type testLivenessRequestBuffer [2 * 1024 * 1024]byte

func TestDynamicLibraryClientCallKeepsRequestMemoryAlive(t *testing.T) {
	testLivenessFinalizedDuringCallback.Store(false)
	testLivenessCallbackReadCorrupted.Store(false)

	client := newGuardedPluginClient(&dynamicLibraryClient{api: windowsPluginAPI{
		call:       syscall.NewCallback(testLivenessPluginCall),
		freeBuffer: syscall.NewCallback(testReentrantPluginFree),
	}})
	t.Cleanup(client.Shutdown)

	req := new(testLivenessRequestBuffer)
	req[0] = 0xAA
	req[len(req)-1] = 0x55
	ownerID := testLivenessCurrentOwnerID.Add(1)
	runtime.SetFinalizer(req, func(_ *testLivenessRequestBuffer) {
		testLivenessFinalizedOwnerID.Store(ownerID)
	})

	got, errCall := client.Call(context.Background(), "test.liveness", req[:])
	if errCall != nil {
		t.Fatalf("Call() error = %v", errCall)
	}
	want := `{"ok":true}`
	if string(got) != want {
		t.Fatalf("Call() response = %q, want %q", got, want)
	}
	if testLivenessFinalizedDuringCallback.Load() {
		t.Fatalf("request memory was finalized during native callback execution")
	}
	if testLivenessCallbackReadCorrupted.Load() {
		t.Fatalf("request buffer was corrupted during native callback execution")
	}
}

func testLivenessPluginCall(methodPtr, requestPtr, requestLen, responsePtr uintptr) uintptr {
	if methodPtr == 0 || requestPtr == 0 || requestLen == 0 || responsePtr == 0 {
		return 1
	}
	for range 5 {
		runtime.GC()
		runtime.Gosched()
	}
	currentID := testLivenessCurrentOwnerID.Load()
	if testLivenessFinalizedOwnerID.Load() == currentID {
		testLivenessFinalizedDuringCallback.Store(true)
		return 2
	}
	buf := unsafe.Slice((*byte)(unsafe.Pointer(requestPtr)), requestLen)
	if buf[0] != 0xAA || buf[requestLen-1] != 0x55 {
		testLivenessCallbackReadCorrupted.Store(true)
		return 3
	}
	raw := []byte(`{"ok":true}`)
	mem, errAlloc := windows.LocalAlloc(windows.LMEM_FIXED, uint32(len(raw)))
	if errAlloc != nil || mem == 0 {
		return 1
	}
	copy(unsafe.Slice((*byte)(unsafe.Pointer(mem)), len(raw)), raw)
	response := (*windowsBuffer)(unsafe.Pointer(responsePtr))
	response.ptr = mem
	response.len = uintptr(len(raw))
	return 0
}

func TestDynamicLibraryClientCallPreservesPluginErrorEnvelope(t *testing.T) {
	client := newGuardedPluginClient(&dynamicLibraryClient{api: windowsPluginAPI{
		call:       syscall.NewCallback(testPluginErrorCall),
		freeBuffer: syscall.NewCallback(testReentrantPluginFree),
	}})
	t.Cleanup(client.Shutdown)
	_, err := callPlugin[rpcEmptyResponse](context.Background(), client, pluginabi.MethodExecutorExecute, rpcEmptyResponse{})
	if err == nil || err.Error() != "plan limit reached" {
		t.Fatalf("plugin error = %v, want plan limit reached", err)
	}
	status, ok := err.(interface{ StatusCode() int })
	if !ok || status.StatusCode() != 403 {
		t.Fatalf("plugin error %T does not directly expose status 403", err)
	}
}

func testPluginErrorCall(_, _, _, responsePtr uintptr) uintptr {
	raw, err := pluginabi.NewErrorEnvelope("insufficient_quota", "plan limit reached", 403)
	if err != nil || responsePtr == 0 {
		return 1
	}
	mem, err := windows.LocalAlloc(windows.LMEM_FIXED, uint32(len(raw)))
	if err != nil || mem == 0 {
		return 1
	}
	copy(unsafe.Slice((*byte)(unsafe.Pointer(mem)), len(raw)), raw)
	response := (*windowsBuffer)(unsafe.Pointer(responsePtr))
	response.ptr = mem
	response.len = uintptr(len(raw))
	return 1
}
