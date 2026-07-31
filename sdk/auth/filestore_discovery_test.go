package auth

import (
	"context"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	cliproxyauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
)

type discoveryRoundTripFunc func(*http.Request) (*http.Response, error)

func (f discoveryRoundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

func TestFileTokenStoreProjectDiscoveryPreservesConcurrentSave(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "antigravity.json")
	original := []byte(`{"type":"antigravity","access_token":"old-token","refresh_token":"old-refresh"}`)
	if errWrite := os.WriteFile(path, original, 0o600); errWrite != nil {
		t.Fatal(errWrite)
	}

	started := make(chan struct{})
	release := make(chan struct{})
	store := NewFileTokenStore()
	store.SetBaseDir(dir)
	store.httpClient = &http.Client{Transport: discoveryRoundTripFunc(func(*http.Request) (*http.Response, error) {
		close(started)
		<-release
		return &http.Response{
			StatusCode: http.StatusOK,
			Body: io.NopCloser(strings.NewReader(
				`{"cloudaicompanionProject":"discovered-project"}`,
			)),
			Header: make(http.Header),
		}, nil
	})}

	type listResult struct {
		auths []*cliproxyauth.Auth
		err   error
	}
	result := make(chan listResult, 1)
	go func() {
		auths, errList := store.List(context.Background())
		result <- listResult{auths: auths, err: errList}
	}()
	<-started

	_, errSave := store.Save(context.Background(), &cliproxyauth.Auth{
		ID:       "antigravity.json",
		FileName: "antigravity.json",
		Metadata: map[string]any{
			"type":          "antigravity",
			"access_token":  "new-token",
			"refresh_token": "new-refresh",
		},
	})
	if errSave != nil {
		t.Fatalf("Save() error = %v", errSave)
	}
	close(release)

	listed := <-result
	if listed.err != nil {
		t.Fatalf("List() error = %v", listed.err)
	}
	if len(listed.auths) != 1 || listed.auths[0] == nil {
		t.Fatalf("List() = %#v", listed.auths)
	}
	if got := listed.auths[0].Metadata["access_token"]; got != "new-token" {
		t.Fatalf("listed access_token = %#v, want new-token", got)
	}
	if got := listed.auths[0].Metadata["refresh_token"]; got != "new-refresh" {
		t.Fatalf("listed refresh_token = %#v, want new-refresh", got)
	}
	if _, exists := listed.auths[0].Metadata["project_id"]; exists {
		t.Fatalf("stale discovery project_id was published: %#v", listed.auths[0].Metadata)
	}

	current, errRead := os.ReadFile(path)
	if errRead != nil {
		t.Fatal(errRead)
	}
	if strings.Contains(string(current), "discovered-project") || !strings.Contains(string(current), "new-refresh") {
		t.Fatalf("concurrent credential was overwritten: %s", current)
	}
}

func TestFileTokenStoreProjectDiscoveryPersistsRefreshedGeminiToken(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "gemini.json")
	if errWrite := os.WriteFile(
		path,
		[]byte(`{"type":"gemini","token":{"access_token":"expired-token","refresh_token":"refresh","client_id":"client","client_secret":"secret","token_uri":"https://oauth.test/token"}}`),
		0o600,
	); errWrite != nil {
		t.Fatal(errWrite)
	}

	store := NewFileTokenStore()
	store.SetBaseDir(dir)
	store.httpClient = &http.Client{Transport: discoveryRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		body := `{"cloudaicompanionProject":"gemini-project"}`
		if req.URL.Host == "oauth.test" {
			body = `{"access_token":"refreshed-token"}`
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Body:       io.NopCloser(strings.NewReader(body)),
			Header:     make(http.Header),
		}, nil
	})}

	auths, errList := store.List(context.Background())
	if errList != nil {
		t.Fatal(errList)
	}
	if len(auths) != 1 || auths[0] == nil {
		t.Fatalf("List() = %#v", auths)
	}
	if got := auths[0].Metadata["project_id"]; got != "gemini-project" {
		t.Fatalf("project_id = %#v, want gemini-project", got)
	}
	current, errRead := os.ReadFile(path)
	if errRead != nil {
		t.Fatal(errRead)
	}
	if !strings.Contains(string(current), "refreshed-token") || !strings.Contains(string(current), "gemini-project") {
		t.Fatalf("persisted Gemini credential = %s", current)
	}
}

func TestFileTokenStoreListCancelsProjectDiscovery(t *testing.T) {
	dir := t.TempDir()
	if errWrite := os.WriteFile(
		filepath.Join(dir, "antigravity.json"),
		[]byte(`{"type":"antigravity","access_token":"token"}`),
		0o600,
	); errWrite != nil {
		t.Fatal(errWrite)
	}

	started := make(chan struct{})
	store := NewFileTokenStore()
	store.SetBaseDir(dir)
	store.httpClient = &http.Client{Transport: discoveryRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		close(started)
		<-req.Context().Done()
		return nil, req.Context().Err()
	})}

	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() {
		_, errList := store.List(ctx)
		result <- errList
	}()
	<-started
	cancel()

	select {
	case errList := <-result:
		if !errors.Is(errList, context.Canceled) {
			t.Fatalf("List() error = %v, want context.Canceled", errList)
		}
	case <-time.After(time.Second):
		t.Fatal("List() did not return after context cancellation")
	}
}

func TestFileTokenStoreProjectDiscoveryHasInternalTimeout(t *testing.T) {
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
	store.projectDiscoveryTimeout = 25 * time.Millisecond
	store.httpClient = &http.Client{Transport: discoveryRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		<-req.Context().Done()
		return nil, req.Context().Err()
	})}

	startedAt := time.Now()
	auths, errList := store.List(context.Background())
	if errList != nil {
		t.Fatalf("List() error = %v", errList)
	}
	if elapsed := time.Since(startedAt); elapsed > time.Second {
		t.Fatalf("List() elapsed = %v, want bounded project discovery", elapsed)
	}
	if len(auths) != 1 || auths[0] == nil {
		t.Fatalf("List() = %#v, want credential without discovered project", auths)
	}
}
