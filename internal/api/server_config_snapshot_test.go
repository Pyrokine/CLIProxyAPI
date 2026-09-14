package api

import (
	"sync"
	"testing"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
)

func TestServerConfigSnapshotConcurrentPublication(t *testing.T) {
	server := &Server{cfg: &config.Config{}}
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		for i := range 10_000 {
			server.setConfig(&config.Config{Debug: i%2 == 0})
		}
	}()
	go func() {
		defer wg.Done()
		for range 10_000 {
			cfg := server.configSnapshot()
			if cfg == nil {
				t.Error("configSnapshot() returned nil")
				return
			}
			_ = cfg.Debug
		}
	}()
	wg.Wait()
}
