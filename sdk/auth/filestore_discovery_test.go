package auth

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"

	cliproxyauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
)

type discoveryRoundTripFunc func(*http.Request) (*http.Response, error)

func (f discoveryRoundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

func TestFileTokenStoreListIsLocalOnlyForAntigravityAndGemini(t *testing.T) {
	for _, fixture := range []struct {
		name string
		raw  string
	}{
		{
			name: "antigravity",
			raw:  `{"type":"antigravity","access_token":"access","refresh_token":"refresh"}`,
		},
		{
			name: "gemini",
			raw: `{"type":"gemini","token":{"access_token":"expired","refresh_token":"refresh",` +
				`"client_id":"client","client_secret":"secret"}}`,
		},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, fixture.name+".json")
			original := []byte(fixture.raw)
			if errWrite := os.WriteFile(path, original, 0o600); errWrite != nil {
				t.Fatal(errWrite)
			}

			var calls atomic.Int64
			previousClient := http.DefaultClient
			http.DefaultClient = &http.Client{
				Transport: discoveryRoundTripFunc(func(request *http.Request) (*http.Response, error) {
					calls.Add(1)
					return nil, fmt.Errorf("unexpected network request to %s", request.URL)
				}),
			}
			t.Cleanup(func() { http.DefaultClient = previousClient })

			store := NewFileTokenStore()
			store.SetBaseDir(dir)
			auths, errList := store.List(context.Background())
			if errList != nil {
				t.Fatal(errList)
			}
			if len(auths) != 1 || auths[0] == nil {
				t.Fatalf("List() = %#v", auths)
			}
			if calls.Load() != 0 {
				t.Fatalf("List() made %d network requests", calls.Load())
			}
			current, errRead := os.ReadFile(path)
			if errRead != nil {
				t.Fatal(errRead)
			}
			if string(current) != string(original) {
				t.Fatalf("List() rewrote credential: %s", current)
			}
			if _, exists := auths[0].Metadata["project_id"]; exists {
				t.Fatalf("List() synthesized project_id: %#v", auths[0].Metadata)
			}
		})
	}
}

func TestFileTokenStoreListHonorsCancelledContextWithoutIO(t *testing.T) {
	dir := t.TempDir()
	if errWrite := os.WriteFile(
		filepath.Join(dir, "antigravity.json"),
		[]byte(`{"type":"antigravity","access_token":"token"}`),
		0o600,
	); errWrite != nil {
		t.Fatal(errWrite)
	}
	store := NewFileTokenStore()
	store.SetBaseDir(dir)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	var auths []*cliproxyauth.Auth
	auths, errList := store.List(ctx)
	if !errors.Is(errList, context.Canceled) || auths != nil {
		t.Fatalf("List() auths=%#v error=%v, want context.Canceled", auths, errList)
	}
}
