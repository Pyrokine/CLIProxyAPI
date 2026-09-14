package auth

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/home"
)

type isolatedHomeClient struct {
	label string
}

func (*isolatedHomeClient) Enabled() bool { return true }

func (*isolatedHomeClient) HeartbeatOK() bool { return true }

func (*isolatedHomeClient) KVGet(context.Context, string) ([]byte, bool, error) {
	return nil, false, nil
}

func (*isolatedHomeClient) KVSet(context.Context, string, []byte, home.KVSetOptions) (bool, error) {
	return true, nil
}

func (*isolatedHomeClient) KVDel(context.Context, ...string) (int64, error) {
	return 0, nil
}

func (*isolatedHomeClient) KVExpire(context.Context, string, time.Duration) (bool, error) {
	return true, nil
}

func (c *isolatedHomeClient) RPopAuth(
	context.Context,
	string,
	string,
	http.Header,
	int,
) ([]byte, error) {
	return []byte(c.label + "-dispatch"), nil
}

func (c *isolatedHomeClient) GetRefreshAuth(context.Context, string, string) ([]byte, error) {
	return []byte(c.label + "-refresh"), nil
}

func TestManagerHomeClientsAreIsolated(t *testing.T) {
	globalClient := &isolatedHomeClient{label: "global"}
	previousDispatcher := currentHomeDispatcher
	previousRefreshClient := currentHomeRuntimeRefreshClient
	currentHomeDispatcher = func() homeAuthDispatcher { return globalClient }
	currentHomeRuntimeRefreshClient = func() HomeRefreshClient { return globalClient }
	t.Cleanup(
		func() {
			currentHomeDispatcher = previousDispatcher
			currentHomeRuntimeRefreshClient = previousRefreshClient
		},
	)

	clientA := &isolatedHomeClient{label: "service-a"}
	clientB := &isolatedHomeClient{label: "service-b"}
	managerA := NewManager(nil, nil, nil)
	managerB := NewManager(nil, nil, nil)
	managerNil := NewManager(nil, nil, nil)
	managerA.SetHomeClientProvider(func() HomeClient { return clientA })
	managerB.SetHomeClientProvider(func() HomeClient { return clientB })
	managerNil.SetHomeClientProvider(func() HomeClient { return nil })

	for name, test := range map[string]struct {
		manager *Manager
		want    string
	}{
		"service-a":     {manager: managerA, want: "service-a-dispatch"},
		"service-b":     {manager: managerB, want: "service-b-dispatch"},
		"compatibility": {manager: NewManager(nil, nil, nil), want: "global-dispatch"},
	} {
		payload, errDispatch := test.manager.homeDispatcher().RPopAuth(
			context.Background(), "model", "session", nil, 1,
		)
		if errDispatch != nil || string(payload) != test.want {
			t.Fatalf("%s dispatch = (%q, %v)", name, payload, errDispatch)
		}
	}

	ctxA := managerA.withHomeRefreshClient(context.Background())
	refreshClientA, ok := HomeRefreshClientFromContext(ctxA)
	if !ok || refreshClientA != clientA {
		t.Fatalf("service A refresh client = (%#v, %t)", refreshClientA, ok)
	}
	ctxB := managerB.withHomeRefreshClient(context.Background())
	refreshClientB, ok := HomeRefreshClientFromContext(ctxB)
	if !ok || refreshClientB != clientB {
		t.Fatalf("service B refresh client = (%#v, %t)", refreshClientB, ok)
	}
	scopeA, scopeAOK := home.ClientScopeFromContext(ctxA)
	scopeB, scopeBOK := home.ClientScopeFromContext(ctxB)
	if !scopeAOK || !scopeBOK || !scopeA.Enabled || !scopeB.Enabled || scopeA.ID == "" || scopeB.ID == "" {
		t.Fatalf("service scopes = (%+v, %t) and (%+v, %t)", scopeA, scopeAOK, scopeB, scopeBOK)
	}
	if scopeA.ID == scopeB.ID {
		t.Fatalf("different managers share runtime scope %q", scopeA.ID)
	}
	if scopeA.Client != clientA || scopeB.Client != clientB {
		t.Fatalf("service-scoped KV clients = %#v and %#v", scopeA.Client, scopeB.Client)
	}

	ctxNil := managerNil.withHomeRefreshClient(context.Background())
	refreshClientNil, ok := HomeRefreshClientFromContext(ctxNil)
	if !ok || refreshClientNil != nil {
		t.Fatalf("explicit nil refresh client = (%#v, %t), want explicit nil", refreshClientNil, ok)
	}
	if managerNil.homeDispatcher() != nil {
		t.Fatal("explicit nil Home provider inherited compatibility dispatcher")
	}
	scopeNil, scopeNilOK := home.ClientScopeFromContext(ctxNil)
	if !scopeNilOK || scopeNil.Enabled {
		t.Fatalf("local nil Home provider scope = (%+v, %t), want disabled", scopeNil, scopeNilOK)
	}
}
