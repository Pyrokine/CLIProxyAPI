package api

import (
	"context"
	"math"
	"testing"
	"time"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/quota"
	coreauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
)

func TestAuthQuotaSchedulingProviderProjectsSchedulerSnapshots(t *testing.T) {
	scheduler := quota.NewSchedulerWithContext(
		quota.DefaultConfig(),
		func(context.Context, *quota.Entry) ([]byte, error) {
			return []byte(`{"usage":{"five_hour":{"utilization":25,"resets_at":"2026-09-08T13:00:00Z"}}}`), nil
		},
	)
	scheduler.Register("claude.json", quota.TypeClaude, "claude-index")
	targets, unresolved := scheduler.ResolveRefreshTargets(nil)
	if len(unresolved) != 0 || len(targets) != 1 {
		t.Fatalf("ResolveRefreshTargets() = (%#v, %#v), want one target", targets, unresolved)
	}
	result := scheduler.RefreshTarget(context.Background(), targets[0])
	if result.Outcome != quota.RefreshOutcomeSucceeded {
		t.Fatalf("RefreshTarget() outcome = %q, error = %q", result.Outcome, result.Error)
	}

	now := time.Date(2026, 9, 8, 12, 0, 0, 0, time.UTC)
	provider := authQuotaSchedulingProvider{
		scheduler: scheduler,
		now:       func() time.Time { return now },
	}
	snapshots := provider.Snapshots("claude", "claude-sonnet-4-5", []string{"claude-index", "missing-index"})
	fresh := snapshots["claude-index"]
	if fresh.Status != coreauth.QuotaSchedulingStatusFresh || math.Abs(fresh.Headroom-0.75) > 1e-9 {
		t.Fatalf("fresh snapshot = %+v", fresh)
	}
	wantReset := time.Date(2026, 9, 8, 13, 0, 0, 0, time.UTC)
	if fresh.ResetAt == nil || !fresh.ResetAt.Equal(wantReset) {
		t.Fatalf("fresh reset = %v, want %v", fresh.ResetAt, wantReset)
	}
	if missing := snapshots["missing-index"]; missing.Status != coreauth.QuotaSchedulingStatusUnknown {
		t.Fatalf("missing snapshot = %+v, want unknown", missing)
	}
}

func TestAuthQuotaSchedulingProviderRejectsUnsupportedProvider(t *testing.T) {
	provider := authQuotaSchedulingProvider{scheduler: quota.NewScheduler(quota.DefaultConfig(), nil)}
	if snapshots := provider.Snapshots("unsupported", "model", []string{"auth-index"}); snapshots != nil {
		t.Fatalf("unsupported provider snapshots = %#v, want nil", snapshots)
	}
}
