package pluginhost

import "testing"

func TestAuthModelResultInvalidAfterPluginFuse(t *testing.T) {
	host := New()
	host.mu.Lock()
	host.applyGeneration = 1
	host.mu.Unlock()
	record := capabilityRecord{id: "sample"}
	token, ok := host.capturePluginOperationToken(record, 1)
	if !ok {
		t.Fatal("capturePluginOperationToken() failed")
	}
	result := AuthModelResult{
		Snapshot:       host.Snapshot(),
		operationToken: token,
		Handled:        true,
	}
	if !host.AuthModelResultCurrent(result) {
		t.Fatal("fresh plugin model result is not current")
	}

	host.fusePlugin("sample", "test", "panic")
	if host.AuthModelResultCurrent(result) {
		t.Fatal("fused plugin model result remained current")
	}
}
