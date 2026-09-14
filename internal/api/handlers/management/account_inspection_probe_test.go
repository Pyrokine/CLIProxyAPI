package management

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/quota"
	coreauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
)

func TestAccountInspectionUsesReadOnlyProviderSafeProbe(t *testing.T) {
	manager := coreauth.NewManager(nil, nil, nil)
	digest := strings.Repeat("a", 64)
	registered, err := manager.Register(
		context.Background(), &coreauth.Auth{
			ID:         "claude-auth",
			Provider:   "claude",
			FileName:   "claude.json",
			Status:     coreauth.StatusActive,
			Attributes: map[string]string{coreauth.PersistedSHA256Attribute: digest},
		},
	)
	if err != nil {
		t.Fatalf("register auth: %v", err)
	}
	probeCalls := 0
	finalizerCalls := 0
	scheduler := quota.NewSchedulerWithContext(
		quota.DefaultConfig(), func(_ context.Context, entry *quota.Entry) ([]byte, error) {
			probeCalls++
			if entry.AuthIndex != registered.Index {
				t.Fatalf("probe auth index = %q, want %q", entry.AuthIndex, registered.Index)
			}
			return []byte(`{"usage":"ok"}`), nil
		},
	)
	scheduler.SetRefreshContextFinalizer(
		func(_ context.Context, _ quota.RefreshAttempt) error {
			finalizerCalls++
			return nil
		},
	)
	scheduler.Register(registered.FileName, quota.TypeClaude, registered.Index)
	h := &Handler{authManager: manager, quotaScheduler: scheduler}

	results := h.inspectionService().inspectAccounts(context.Background(), "")
	if len(results) != 1 {
		t.Fatalf("inspection results = %+v", results)
	}
	result := results[0]
	if result.ProbeStatus != accountProbePassed || result.Status != accountHealthNormal {
		t.Fatalf("inspection result = %+v", result)
	}
	if result.AuthGeneration != registered.Generation || result.CredentialDigest != digest ||
		result.ProbeRevision != accountInspectionProbeRevision || result.CheckedAt.IsZero() {
		t.Fatalf("probe binding = %+v, auth = %+v", result, registered)
	}
	if probeCalls != 1 || finalizerCalls != 0 {
		t.Fatalf("probe calls = %d, finalizer calls = %d", probeCalls, finalizerCalls)
	}
	entry := scheduler.GetStatus().Credentials[quota.EntryKey(quota.TypeClaude, registered.FileName)]
	if entry.Status != quota.StatusIdle || entry.LastRefresh != nil || entry.FailureCount != 0 || len(entry.Data) != 0 {
		t.Fatalf("provider-safe probe changed quota state: %+v", entry)
	}
}

func TestAccountInspectionMarksChangedAuthBindingOutdated(t *testing.T) {
	manager := coreauth.NewManager(nil, nil, nil)
	registered, err := manager.Register(
		context.Background(), &coreauth.Auth{
			ID:         "claude-auth",
			Provider:   "claude",
			FileName:   "claude.json",
			Status:     coreauth.StatusActive,
			Attributes: map[string]string{coreauth.PersistedSHA256Attribute: strings.Repeat("a", 64)},
		},
	)
	if err != nil {
		t.Fatalf("register auth: %v", err)
	}
	entered := make(chan struct{})
	release := make(chan struct{})
	scheduler := quota.NewSchedulerWithContext(
		quota.DefaultConfig(), func(_ context.Context, _ *quota.Entry) ([]byte, error) {
			close(entered)
			<-release
			return []byte(`{"usage":"ok"}`), nil
		},
	)
	scheduler.Register(registered.FileName, quota.TypeClaude, registered.Index)
	h := &Handler{authManager: manager, quotaScheduler: scheduler}
	service := h.inspectionService()

	resultsCh := make(chan []accountInspectionResult, 1)
	go func() {
		resultsCh <- service.inspectAccounts(context.Background(), "")
	}()
	<-entered
	updated := registered.Clone()
	updated.Attributes[coreauth.PersistedSHA256Attribute] = strings.Repeat("b", 64)
	if _, err = manager.Update(context.Background(), updated); err != nil {
		t.Fatalf("update auth: %v", err)
	}
	close(release)

	results := <-resultsCh
	if len(results) != 1 {
		t.Fatalf("inspection results = %+v", results)
	}
	result := results[0]
	if result.ProbeStatus != accountProbeOutdated || result.Status != accountHealthUnknown ||
		result.Reason != "probe_outdated" || result.Advice != "none" || result.AutoHandling != "none" {
		t.Fatalf("outdated inspection result = %+v", result)
	}
}

func TestAccountInspectionClassifiesProbeFailures(t *testing.T) {
	for _, test := range []struct {
		name         string
		err          error
		probeStatus  string
		healthStatus string
		reason       string
	}{
		{
			name:         "credential rejected",
			err:          errors.New("claude usage returned status 401"),
			probeStatus:  accountProbeFailed,
			healthStatus: accountHealthUnavailable,
			reason:       "probe_failed",
		},
		{
			name:         "provider unavailable",
			err:          errors.New("claude usage returned status 503"),
			probeStatus:  accountProbeInconclusive,
			healthStatus: accountHealthUnknown,
			reason:       "probe_inconclusive",
		},
	} {
		t.Run(
			test.name, func(t *testing.T) {
				manager := coreauth.NewManager(nil, nil, nil)
				registered, err := manager.Register(
					context.Background(), &coreauth.Auth{
						ID:       "claude-auth",
						Provider: "claude",
						FileName: "claude.json",
						Status:   coreauth.StatusActive,
					},
				)
				if err != nil {
					t.Fatalf("register auth: %v", err)
				}
				scheduler := quota.NewSchedulerWithContext(
					quota.DefaultConfig(), func(_ context.Context, _ *quota.Entry) ([]byte, error) {
						return nil, test.err
					},
				)
				scheduler.Register(registered.FileName, quota.TypeClaude, registered.Index)
				h := &Handler{authManager: manager, quotaScheduler: scheduler}

				results := h.inspectionService().inspectAccounts(context.Background(), "")
				if len(results) != 1 {
					t.Fatalf("inspection results = %+v", results)
				}
				result := results[0]
				if result.ProbeStatus != test.probeStatus || result.Status != test.healthStatus || result.Reason != test.reason {
					t.Fatalf("inspection result = %+v", result)
				}
			},
		)
	}
}

func TestInspectionAuthRequiresExplicitProbe(t *testing.T) {
	auth := &coreauth.Auth{Status: coreauth.StatusActive}
	for _, entryStatus := range []quota.Status{quota.StatusBanned, quota.StatusQuotaExceeded} {
		if !inspectionAuthRequiresExplicitProbe(auth, &quota.Entry{Status: entryStatus}) {
			t.Fatalf("status %s did not require explicit probe", entryStatus)
		}
	}
	auth.Disabled = true
	if !inspectionAuthRequiresExplicitProbe(auth, nil) {
		t.Fatal("disabled auth did not require explicit probe")
	}
}
