package auth

import (
	"context"
	"testing"
)

func TestQuotaResetPreservesSDKMethodContract(t *testing.T) {
	manager := NewManager(nil, nil, nil)
	var reset func(context.Context, string) (*Auth, []string, error) = manager.ResetQuota
	auth, err := manager.Register(context.Background(), &Auth{
		ID: "synthetic-sdk-reset", Provider: "codex", Label: "original",
		Metadata: map[string]any{"access_token": "synthetic-sdk-token"},
		Status:   StatusError, Unavailable: true, Quota: QuotaState{Exceeded: true},
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx := WithQuotaResetCredential(context.Background(), auth)
	replacement := auth.Clone()
	replacement.Label = "changed"
	if _, err = manager.Update(context.Background(), replacement); err != nil {
		t.Fatal(err)
	}
	if _, _, err = reset(ctx, auth.ID); err == nil {
		t.Fatal("stale scoped reset cleared a changed credential")
	}
	current, _ := manager.GetByID(auth.ID)
	if current == nil || !current.Quota.Exceeded || current.Label != "changed" {
		t.Fatal("rejected scoped reset changed current credential state")
	}
	if _, _, err = reset(WithQuotaResetCredential(context.Background(), nil), auth.ID); err == nil {
		t.Fatal("empty expected credential bypassed scoped reset validation")
	}
	if _, _, err = reset(context.Background(), auth.ID); err != nil {
		t.Fatalf("legacy SDK reset method failed: %v", err)
	}
	current, _ = manager.GetByID(auth.ID)
	if current == nil || current.Quota.Exceeded {
		t.Fatal("legacy explicit reset did not clear its local quota state")
	}
}
