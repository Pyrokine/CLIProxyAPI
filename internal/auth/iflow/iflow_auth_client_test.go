package iflow

import (
	"net/http"
	"testing"
	"time"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
)

func TestNewAuthPreservesDefaultAndProvidedClientContracts(t *testing.T) {
	for _, selection := range []string{"original", "nil-client", "provided-client"} {
		t.Run(
			selection, func(t *testing.T) {
				cfg := &config.Config{}
				var auth *Auth
				if selection == "provided-client" {
					cfg.ProxyURL = "invalid-global-proxy"
					client := &http.Client{Timeout: 7 * time.Second, Transport: &http.Transport{}}
					auth = NewAuth(cfg, client)
					if auth.httpClient != client || client.Timeout != 7*time.Second {
						t.Fatal("provided HTTP client or its timeout was replaced")
					}
					return
				}
				if selection == "nil-client" {
					auth = NewAuth(cfg, nil)
				} else {
					auth = NewAuth(cfg)
				}
				if auth.httpClient == nil || auth.httpClient.Timeout != 30*time.Second {
					t.Fatal("original constructor no longer retains its 30-second client")
				}
			},
		)
	}
}
