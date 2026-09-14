package geminicli

import "testing"

func TestMetadataRefreshCommitRejectsStalePublication(t *testing.T) {
	shared := NewSharedCredential(
		"primary.json",
		"user@example.com",
		map[string]any{"access_token": "old"},
		nil,
	)
	staged, staleCommit := shared.StageMetadata(map[string]any{"access_token": "staged"})
	if staged["access_token"] != "staged" {
		t.Fatalf("staged metadata = %#v", staged)
	}
	shared.MergeMetadata(map[string]any{"access_token": "newer"})
	if staleCommit.Current() {
		t.Fatal("stale metadata commit reported current")
	}
	called := false
	if staleCommit.CommitRefresh(
		func() bool {
			called = true
			return true
		},
	) {
		t.Fatal("stale metadata commit succeeded")
	}
	if called {
		t.Fatal("stale metadata commit invoked manager publication")
	}
	if got := shared.MetadataSnapshot()["access_token"]; got != "newer" {
		t.Fatalf("shared access token = %#v, want newer", got)
	}
}

func TestMetadataRefreshCommitPublishesWithManagerState(t *testing.T) {
	shared := NewSharedCredential(
		"primary.json",
		"user@example.com",
		map[string]any{"access_token": "old"},
		nil,
	)
	_, commit := shared.StageMetadata(map[string]any{"access_token": "new"})
	if got := shared.MetadataSnapshot()["access_token"]; got != "old" {
		t.Fatalf("shared metadata changed before manager publication: %#v", got)
	}
	managerPublished := false
	if !commit.CommitRefresh(
		func() bool {
			managerPublished = true
			return true
		},
	) {
		t.Fatal("metadata commit failed")
	}
	if !managerPublished {
		t.Fatal("manager publication callback was not invoked")
	}
	if got := shared.MetadataSnapshot()["access_token"]; got != "new" {
		t.Fatalf("shared access token = %#v, want new", got)
	}
}
