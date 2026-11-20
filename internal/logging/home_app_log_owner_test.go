package logging

import (
	"encoding/json"
	"testing"
	"time"

	log "github.com/sirupsen/logrus"
)

func TestHomeAppLogQueueRetainsItsOriginalClient(t *testing.T) {
	oldClient := &stubHomeAppLogClient{heartbeatOK: true}
	newClient := &stubHomeAppLogClient{heartbeatOK: true}
	forwarder := &HomeAppLogForwarder{
		client: oldClient,
		queue:  make(chan homeAppLogPayload, 1),
		stop:   make(chan struct{}),
	}
	forwarder.enabled.Store(true)
	t.Cleanup(forwarder.Stop)
	entry := log.NewEntry(log.StandardLogger())
	entry.Time, entry.Level, entry.Message = time.Now(), log.InfoLevel, "original owner log"
	if err := forwarder.Fire(entry); err != nil {
		t.Fatal(err)
	}
	var queued homeAppLogPayload
	select {
	case queued = <-forwarder.queue:
	default:
		t.Fatal("actual Fire did not queue the entry")
	}
	forwarder.bind(newClient)
	forwarder.forward(queued)
	if oldClient.pushedCount() != 1 || newClient.pushedCount() != 0 {
		t.Fatalf("queued owner changed: old pushes=%d, new pushes=%d", oldClient.pushedCount(), newClient.pushedCount())
	}
	var body map[string]any
	if err := json.Unmarshal(oldClient.pushedAt(0), &body); err != nil {
		t.Fatal(err)
	}
	for field := range body {
		if field != "line" && field != "level" && field != "timestamp" && field != "request_id" {
			t.Fatalf("internal owner was serialized as %s", field)
		}
	}
}

func TestStoppedHomeAppLogForwardersReleaseHookReferences(t *testing.T) {
	before := len(log.StandardLogger().Hooks[log.InfoLevel])
	for index := range 3 {
		forwarder := startHomeAppLogForwarder(1, &stubHomeAppLogClient{heartbeatOK: true})
		forwarder.Stop()
		forwarder.Stop()
		after := len(log.StandardLogger().Hooks[log.InfoLevel])
		if after > before+1 {
			t.Fatalf("hooks accumulated after %d start/stop cycles: before=%d, after=%d", index+1, before, after)
		}
		for _, hooks := range log.StandardLogger().Hooks {
			for _, hook := range hooks {
				if hook == forwarder {
					t.Fatal("stopped forwarder is retained by a global logger hook")
				}
			}
		}
		activeHomeAppLogForwarders.RLock()
		_, retained := activeHomeAppLogForwarders.items[forwarder]
		activeHomeAppLogForwarders.RUnlock()
		if retained {
			t.Fatal("stopped forwarder is retained by the active registry")
		}
	}
}
