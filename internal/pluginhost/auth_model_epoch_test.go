package pluginhost

import (
	"context"
	"testing"
	"time"
)

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

func TestPublishAuthModelsRejectsStaleSnapshotAndOperation(t *testing.T) {
	for _, scenario := range []string{"snapshot", "operation"} {
		t.Run(scenario, func(t *testing.T) {
			host := New()
			host.mu.Lock()
			host.applyGeneration = 1
			host.mu.Unlock()
			token, ok := host.capturePluginOperationToken(capabilityRecord{id: "sample"}, 1)
			if !ok {
				t.Fatal("could not capture model publication ownership")
			}
			result := AuthModelResult{Snapshot: host.Snapshot(), operationToken: token, Handled: true}
			calls := 0
			publish := func() bool { calls++; return true }
			if !host.PublishAuthModelsIfCurrent(result, publish) || calls != 1 {
				t.Fatal("current model publication was rejected")
			}
			if scenario == "snapshot" {
				host.InvalidateRuntime()
			} else {
				host.publicationMu.Lock()
				host.mu.Lock()
				host.advancePluginOperationLocked("sample")
				host.mu.Unlock()
				host.publicationMu.Unlock()
				if host.Snapshot() != result.Snapshot {
					t.Fatal("operation invalidation changed the snapshot unexpectedly")
				}
			}
			if host.PublishAuthModelsIfCurrent(result, publish) || calls != 1 {
				t.Fatal("stale plugin ownership published credential models")
			}
		})
	}
}

func TestPublishAuthModelsSerializesRuntimeInvalidation(t *testing.T) {
	host := New()
	result := AuthModelResult{Snapshot: host.Snapshot()}
	started, release, published := make(chan struct{}), make(chan struct{}), make(chan bool, 1)
	go func() {
		published <- host.PublishAuthModelsIfCurrent(result, func() bool {
			close(started)
			<-release
			return host.AuthModelResultCurrent(result)
		})
	}()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("model publication did not start")
	}
	invalidated := make(chan struct{})
	go func() {
		host.InvalidateRuntime()
		close(invalidated)
	}()
	select {
	case <-invalidated:
		close(release)
		t.Fatal("runtime invalidated in the middle of model publication")
	case <-time.After(20 * time.Millisecond):
	}
	close(release)
	select {
	case ok := <-published:
		if !ok {
			t.Fatal("plugin ownership changed during model publication")
		}
	case <-time.After(time.Second):
		t.Fatal("model publication did not complete")
	}
	select {
	case <-invalidated:
	case <-time.After(time.Second):
		t.Fatal("runtime invalidation did not complete")
	}
	if host.PublishAuthModelsIfCurrent(result, func() bool { return true }) {
		t.Fatal("invalidated model publication remained active")
	}
	if err := host.ShutdownAllContext(context.Background()); err != nil {
		t.Fatal(err)
	}
}
