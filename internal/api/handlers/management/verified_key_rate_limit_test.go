package management

import (
	"net/http"
	"testing"
	"time"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	"golang.org/x/crypto/bcrypt"
)

func TestManagementBcryptKeyCanRecoverDuringSourceCooldown(t *testing.T) {
	const ip = "203.0.113.1"
	const key = "test-configured-management-key"
	hash, err := bcrypt.GenerateFromPassword([]byte(key), bcrypt.MinCost)
	if err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{AuthDir: t.TempDir()}
	cfg.RemoteManagement.AllowRemote = true
	cfg.RemoteManagement.SecretKey = string(hash)
	h := NewHandler(cfg, "", nil, withEnvSecret(""))
	t.Cleanup(func() { _ = h.Close() })
	h.attemptsMu.Lock()
	h.failedAttempts[ip] = &attemptInfo{blockedUntil: time.Now().Add(time.Minute)}
	h.attemptsMu.Unlock()

	for range 2 {
		if ok, status, _ := h.AuthenticateManagementKey(ip, false, key); !ok || status != 0 {
			t.Fatalf("valid key during source cooldown: allowed=%v status=%d", ok, status)
		}
	}
	if ok, status, _ := h.AuthenticateManagementKey(
		ip, false, "unrecognized",
	); ok || status != http.StatusTooManyRequests {
		t.Fatalf("unverified key during source cooldown: allowed=%v status=%d", ok, status)
	}

	rotated := *cfg
	rotatedHash, err := bcrypt.GenerateFromPassword([]byte("rotated-test-key"), bcrypt.MinCost)
	if err != nil {
		t.Fatal(err)
	}
	rotated.RemoteManagement.SecretKey = string(rotatedHash)
	h.SetConfig(&rotated)
	if ok, _, _ := h.AuthenticateManagementKey(ip, false, key); ok {
		t.Fatal("accepting a previously verified key after rotation")
	}
}
