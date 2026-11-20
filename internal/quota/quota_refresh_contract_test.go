package quota

import (
	"fmt"
	"math"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"
)

func TestQuotaInitialRegistrationStaggersAndPreservesHistory(t *testing.T) {
	s := NewScheduler(DefaultConfig(), nil)
	before := time.Now().UTC()
	for i := range 3 {
		s.RegisterBinding(fmt.Sprintf("new-%d.json", i), TypeCodex, fmt.Sprintf("auth-%d", i), "revision")
	}
	after := time.Now().UTC()
	for i := range 3 {
		state := s.entries[EntryKey(TypeCodex, fmt.Sprintf("new-%d.json", i))].snapshot()
		minimum := before.Add(time.Duration(i) * 2 * time.Second)
		maximum := after.Add(time.Duration(i) * 2 * time.Second)
		if state.NextRefresh == nil || state.NextRefresh.Before(minimum) || state.NextRefresh.After(maximum) {
			t.Errorf("entry %d next=%v, want within [%s, %s]", i, state.NextRefresh, minimum, maximum)
		}
	}

	next := after.Add(time.Hour)
	key := EntryKey(TypeCodex, "restored.json")
	s.Restore(
		map[string]EntryState{
			key: {
				FileName: "restored.json", Type: TypeCodex, AuthIndex: "restored-auth",
				CredentialRevision: "old-revision",
				Generation:         7, StateRevision: 3, Status: StatusSuccess, Freshness: FreshnessFresh,
				NextRefresh: &next, Data: []byte(`{"rate_limit":{"primary_window":{"used_percent":25}}}`),
			},
		},
	)
	s.RegisterBinding("restored.json", TypeCodex, "restored-auth", "old-revision")
	state := s.entries[key].snapshot()
	if state.NextRefresh == nil || !state.NextRefresh.Equal(next) || state.Generation != 7 || len(state.Data) == 0 {
		t.Fatalf("restored schedule or retained data changed: %+v", state)
	}
	before = time.Now().UTC()
	s.RegisterBinding("restored.json", TypeCodex, "restored-auth", "new-revision")
	after = time.Now().UTC()
	state = s.entries[key].snapshot()
	if state.NextRefresh == nil || state.NextRefresh.Before(before) || state.NextRefresh.After(after) ||
		state.Generation != 8 ||
		len(state.Data) != 0 {
		t.Fatalf("changed binding did not retain immediate refresh/reset semantics: %+v", state)
	}
}

func TestQuotaDispatcherStaggersAndSkipsRestrictedCredentials(t *testing.T) {
	synctest.Test(
		t, func(t *testing.T) {
			type call struct {
				file string
				at   time.Time
			}
			calls := make(chan call, 6)
			cfg := DefaultConfig()
			cfg.Enabled = true
			s := NewScheduler(
				cfg, func(entry *Entry) ([]byte, error) {
					calls <- call{entry.FileName, time.Now()}
					return []byte(`{"rate_limit":{"allowed":true}}`), nil
				},
			)
			start := time.Now()
			for i := range 3 {
				s.Register(fmt.Sprintf("healthy-%d.json", i), TypeCodex, fmt.Sprintf("healthy-%d", i))
			}
			for _, status := range []Status{StatusSuccess, StatusBanned, StatusQuotaExceeded} {
				name := "restricted-" + string(status) + ".json"
				s.Register(name, TypeCodex, name)
				entry := s.entries[EntryKey(TypeCodex, name)]
				entry.Status = status
				entry.NextRefresh = &start
				if status == StatusSuccess {
					s.SetDisabled(name, TypeCodex, true)
				}
			}
			s.Start()
			defer s.Stop()
			for i := range 3 {
				synctest.Wait()
				select {
				case got := <-calls:
					if want := fmt.Sprintf(
						"healthy-%d.json", i,
					); got.file != want || got.at.Sub(start) != time.Duration(i)*2*time.Second {
						t.Fatalf("dispatch %d = %+v, want %s at %s", i, got, want, time.Duration(i)*2*time.Second)
					}
				default:
					t.Fatalf("dispatch %d did not execute", i)
				}
				if i < 2 {
					time.Sleep(2 * time.Second)
				}
			}
			time.Sleep(10 * time.Second)
			synctest.Wait()
			select {
			case got := <-calls:
				t.Fatalf("unexpected automatic quota request: %+v", got)
			default:
			}
		},
	)
}

func TestQuotaDispatcherRechecksRestrictionsAfterWaiting(t *testing.T) {
	for _, status := range []int{403, 429, 402} {
		t.Run(
			fmt.Sprint(status), func(t *testing.T) {
				synctest.Test(
					t, func(t *testing.T) {
						var calls atomic.Int32
						var blocked atomic.Bool
						cfg := DefaultConfig()
						cfg.Enabled = true
						s := NewScheduler(
							cfg, func(*Entry) ([]byte, error) {
								calls.Add(1)
								if blocked.Load() {
									return nil, fmt.Errorf("upstream status %d", status)
								}
								return []byte(`{"rate_limit":{"allowed":true}}`), nil
							},
						)
						s.Register("restricted-while-waiting.json", TypeCodex, "auth")
						s.Start()
						defer s.Stop()
						synctest.Wait()
						if got := calls.Load(); got != 1 {
							t.Fatalf("initial scheduled requests=%d, want 1", got)
						}
						blocked.Store(true)
						s.RefreshNow([]string{"restricted-while-waiting.json"})
						if got := calls.Load(); got != 2 {
							t.Fatalf("explicit recovery requests=%d, want 2 total", got)
						}
						time.Sleep(time.Duration(cfg.Interval+1) * time.Second)
						synctest.Wait()
						if got := calls.Load(); got != 2 {
							t.Fatalf("restricted account received an automatic request after waiting: requests=%d", got)
						}
						blocked.Store(false)
						s.RefreshNow([]string{"restricted-while-waiting.json"})
						if got := calls.Load(); got != 3 {
							t.Fatalf("explicit recovery requests=%d, want 3 total", got)
						}
						time.Sleep(time.Duration(cfg.Interval+1) * time.Second)
						synctest.Wait()
						if got := calls.Load(); got != 4 {
							t.Fatalf("recovered account did not resume automatic scheduling: requests=%d, want 4", got)
						}
					},
				)
			},
		)
	}
}

func TestQuotaRelativeResetsStayAtObservation(t *testing.T) {
	observed := time.Date(2026, 9, 8, 1, 0, 0, 0, time.UTC)
	for _, test := range []struct {
		name     string
		kind     CredentialType
		data     string
		legacy   bool
		missing  bool
		absolute bool
	}{
		{
			name: "codex", kind: TypeCodex,
			data: `{"rate_limit":{"primary_window":{"used_percent":25,"reset_after_seconds":60}}}`,
		},
		{
			name: "codex camel case", kind: TypeCodex,
			data: `{"rateLimit":{"secondaryWindow":{"usedPercent":25,"resetAfterSeconds":60}}}`,
		},
		{name: "kimi reset_in", kind: TypeKimi, data: `{"limits":[{"detail":{"limit":100,"used":25,"reset_in":60}}]}`},
		{name: "kimi ttl", kind: TypeKimi, data: `{"usage":{"limit":100,"used":25,"ttl":60}}`},
		{
			name: "legacy last refresh", kind: TypeCodex, legacy: true,
			data: `{"rate_limit":{"primary_window":{"used_percent":25,"reset_after_seconds":60}}}`,
		},
		{
			name: "no observation", kind: TypeCodex, missing: true,
			data: `{"rate_limit":{"primary_window":{"used_percent":25,"reset_after_seconds":60}}}`,
		},
		{
			name: "absolute reset", kind: TypeCodex, absolute: true,
			data: `{"rate_limit":{"primary_window":{"used_percent":25,"reset_at":"2026-09-08T01:03:00Z"}}}`,
		},
	} {
		t.Run(
			test.name, func(t *testing.T) {
				s := NewScheduler(DefaultConfig(), nil)
				s.Register("relative.json", test.kind, "relative-auth")
				entry := s.entries[EntryKey(test.kind, "relative.json")]
				entry.Status, entry.Freshness = StatusSuccess, FreshnessFresh
				entry.Data = []byte(test.data)
				if test.legacy {
					entry.LastRefresh = &observed
				} else if !test.missing {
					entry.ObservedAt = &observed
				}
				for _, readAt := range []time.Time{observed, observed.Add(time.Hour)} {
					got := s.SchedulingSnapshots(test.kind, []string{"relative-auth"}, "model", readAt)["relative-auth"]
					if got.Status != SchedulingStatusFresh || math.Abs(got.Headroom-.75) > 1e-9 {
						t.Fatalf("headroom/status changed: %+v", got)
					}
					if test.missing {
						if got.ResetAt != nil {
							t.Errorf("missing observation fabricated reset deadline: %v", got.ResetAt)
						}
						continue
					}
					want := observed.Add(time.Minute)
					if test.absolute {
						want = observed.Add(3 * time.Minute)
					}
					if got.ResetAt == nil || !got.ResetAt.Equal(want) {
						t.Errorf("read at %s: reset=%v, want %s", readAt, got.ResetAt, want)
					}
				}
			},
		)
	}
}

func TestQuotaAntigravityGroupedSchedulingRetainsScopeAndStrictestWindow(t *testing.T) {
	data := []byte((`{"quota":{"groups":[{"displayName":"Shared limits","buckets":[{"bucketId":"five","window"` +
		`:"5h","modelId":"wanted","remainingFraction":0.8,"resetTime":"2026-11-06T05:00:00Z"},{"bucket_id":"` +
		`weekly","window":"weekly","model_id":"wanted","remaining_fraction":0.2,"reset_time":"2026-11-13T00:` +
		`00:00Z"},{"modelId":"other","remainingFraction":0}]}]}}`))
	window, err := parseSchedulingWindow(TypeAntigravity, data, "wanted", time.Time{})
	wantReset := time.Date(2026, 11, 13, 0, 0, 0, 0, time.UTC)
	if err != nil || math.Abs(window.headroom-.2) > 1e-9 || window.resetAt == nil || !window.resetAt.Equal(wantReset) {
		t.Fatalf("grouped scheduling = %+v, error=%v", window, err)
	}
	if _, err := parseSchedulingWindow(TypeAntigravity, data, "unmatched", time.Time{}); err == nil {
		t.Fatal("grouped model mismatch was accepted")
	}
}
