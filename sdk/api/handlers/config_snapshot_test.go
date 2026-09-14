package handlers

import (
	"sync"
	"testing"

	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/config"
)

func TestBaseAPIHandlerConfigSnapshotConcurrentPublication(t *testing.T) {
	handler := NewBaseAPIHandlers(&config.SDKConfig{}, nil)
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		for i := range 10_000 {
			handler.UpdateClients(&config.SDKConfig{RequestLog: i%2 == 0})
		}
	}()
	go func() {
		defer wg.Done()
		for range 10_000 {
			cfg := handler.ConfigSnapshot()
			if cfg == nil {
				t.Error("ConfigSnapshot() returned nil")
				return
			}
			_ = cfg.RequestLog
		}
	}()
	wg.Wait()
}
