package auth

import (
	"slices"
	"sync"
	"testing"
	"time"
)

func TestEnsureIndexConcurrentReadersAndSnapshots(t *testing.T) {
	for _, assigned := range []bool{false, true} {
		for _, mode := range []string{"ensure", "ensure with clone", "ensure with assign and clone"} {
			name := "derived/" + mode
			if assigned {
				name = "preassigned/" + mode
			}
			t.Run(
				name, func(t *testing.T) {
					credential := &Auth{
						ID: t.Name(), Provider: "gemini",
						Attributes: map[string]string{AttributeAuthIndexSeed: "synthetic-stable-seed"},
					}
					want := stableAuthIndex(credential.indexSeed())
					if assigned {
						credential.Index = "synthetic-preassigned-index"
						want = credential.Index
					}
					start := make(chan struct{})
					var workers sync.WaitGroup
					for worker := range 32 {
						workers.Go(
							func() {
								<-start
								for range 100 {
									if mode == "ensure with assign and clone" && worker%3 == 0 {
										if got := credential.AssignIndex(want); got != want {
											t.Errorf("assigned index=%q expected=%q", got, want)
										}
									}
									if got := credential.EnsureIndex(); got != want {
										t.Errorf("index=%q expected=%q", got, want)
									}
									if mode != "ensure" {
										snapshot := credential.Clone()
										if snapshot.Index != want || snapshot.EnsureIndex() != want {
											t.Errorf("snapshot index=%q expected=%q", snapshot.Index, want)
										}
									}
								}
							},
						)
					}
					close(start)
					workers.Wait()
					if credential.Index != want || !credential.indexAssigned {
						t.Fatalf("published index=%q assigned=%t", credential.Index, credential.indexAssigned)
					}
				},
			)
		}
	}
}

func TestMatchesIndexRetainsLegacyReferencesWithoutPublishingIdentity(t *testing.T) {
	legacy := stableAuthIndex("gemini-api-key:https://synthetic.invalid/v1+synthetic-key")
	for _, entry := range []struct {
		name       string
		credential *Auth
		want       bool
	}{
		{"derived", &Auth{Provider: "gemini"}, true},
		{"explicit published index", &Auth{Provider: "gemini", Index: "public-index"}, false},
		{
			"explicit seed", &Auth{
				Provider: "gemini", Attributes: map[string]string{AttributeAuthIndexSeed: "selected-identity"},
			}, false,
		},
		{
			"file identity", &Auth{
				Provider: "gemini", FileName: "synthetic-credential.json",
				Attributes: map[string]string{"path": "synthetic-credential.json"},
			}, false,
		},
		{"new provider", &Auth{Provider: "meta"}, false},
		{"nil", nil, false},
	} {
		t.Run(
			entry.name, func(t *testing.T) {
				credential := entry.credential
				if credential != nil {
					if credential.Attributes == nil {
						credential.Attributes = make(map[string]string)
					}
					credential.Attributes["base_url"] = "https://synthetic.invalid/v1"
					credential.Attributes["api_key"] = "synthetic-key"
					credential.Attributes["source"] = "synthetic-source"
				}
				before := credential.Clone()
				if got := credential.MatchesIndex(" " + legacy + " "); got != entry.want {
					t.Fatalf("legacy match=%t expected=%t", got, entry.want)
				}
				if credential == nil {
					return
				}
				if credential.Index != before.Index || credential.indexAssigned != before.indexAssigned {
					t.Fatal("read-only matching published a different credential identity")
				}
				current := credential.EnsureIndex()
				if !credential.MatchesIndex(current) || credential.MatchesIndex("") ||
					credential.MatchesIndex("unknown-index") {
					t.Fatal("current and invalid index matching changed")
				}
			},
		)
	}
}

func TestRecentRequestsSnapshotsKeepIndependentCloneCaches(t *testing.T) {
	now := time.Unix(recentRequestBucketSeconds*100, 0)
	credential := &Auth{ID: "synthetic-snapshot"}
	credential.recordRecentRequest(now, true)
	before := credential.RecentRequestsSnapshot(now)
	clone := credential.Clone()
	clone.RecentRequestsSnapshot(now.Add(time.Duration(recentRequestBucketSeconds) * time.Second))
	if got := credential.RecentRequestsSnapshot(now); !slices.Equal(got, before) {
		t.Fatalf("clone cache changed the original history: got=%+v expected=%+v", got, before)
	}
	clone.recordRecentRequest(now, false)
	if got := credential.RecentRequestsSnapshot(now); !slices.Equal(got, before) {
		t.Fatal("clone observation changed the original history")
	}
	cloned := clone.RecentRequestsSnapshot(now)
	if latest := cloned[len(cloned)-1]; latest.Success != 1 || latest.Failed != 1 {
		t.Fatalf("clone lost its independent observation: %+v", latest)
	}
	before[len(before)-1].Success = 99
	if got := credential.RecentRequestsSnapshot(now); got[len(got)-1].Success != 1 {
		t.Fatal("returned snapshot changed the cached history")
	}
}

func TestRecentRequestsConcurrentRecordingAndSnapshots(t *testing.T) {
	now := time.Unix(recentRequestBucketSeconds*100, 0)
	credential := &Auth{ID: "synthetic-concurrent-history"}
	start := make(chan struct{})
	var workers sync.WaitGroup
	for worker := range 32 {
		workers.Go(
			func() {
				<-start
				for range 100 {
					if worker%3 == 0 {
						credential.recordRecentRequest(now, true)
					} else {
						credential.RecentRequestsSnapshot(now)
						credential.Clone().RecentRequestsSnapshot(now)
					}
				}
			},
		)
	}
	close(start)
	workers.Wait()
	got := credential.RecentRequestsSnapshot(now)
	if latest := got[len(got)-1]; latest.Success != 1100 || latest.Failed != 0 {
		t.Fatalf("concurrent snapshots lost observations: %+v", latest)
	}
}
