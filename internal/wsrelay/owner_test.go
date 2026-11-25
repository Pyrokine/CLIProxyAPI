package wsrelay

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestRelayRequiresAndEnforcesOwner(t *testing.T) {
	type identityKey struct{}
	manager := NewManager(
		Options{
			OwnerIdentity: func(ctx context.Context) string {
				identity, _ := ctx.Value(identityKey{}).(string)
				return identity
			},
		},
	)
	response := httptest.NewRecorder()
	manager.Handler().ServeHTTP(response, httptest.NewRequest(http.MethodGet, manager.Path(), nil))
	if response.Code != http.StatusUnauthorized {
		t.Fatalf("anonymous relay registration returned %d", response.Code)
	}

	manager.sessions["relay"] = &session{owner: "client-a"}
	other := context.WithValue(t.Context(), identityKey{}, "client-b")
	if _, err := manager.send(other, "relay", message{}); err == nil {
		t.Fatal("a relay send was allowed for a different client identity")
	}
}
