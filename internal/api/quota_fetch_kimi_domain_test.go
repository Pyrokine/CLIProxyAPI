package api

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/quota"
	coreauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
)

func TestKimiQuotaFetchRejectsUnverifiedAIDomain(t *testing.T) {
	manager := coreauth.NewManager(nil, nil, nil)
	for _, test := range []struct {
		provider string
		domain   string
		allowed  bool
	}{
		{"kimi", "kimi.com", true},
		{"kimi-ai", "kimi.ai", false},
		{"kimi", "kimi.ai", false},
	} {
		t.Run(test.provider+"/"+test.domain, func(t *testing.T) {
			auth, errRegister := manager.Register(context.Background(), &coreauth.Auth{
				ID:       "kimi-quota-" + test.provider + "-" + test.domain,
				FileName: "kimi-quota-" + test.provider + "-" + test.domain + ".json",
				Provider: test.provider, Attributes: map[string]string{"domain": test.domain},
			})
			if errRegister != nil {
				t.Fatal(errRegister)
			}
			calls := 0
			fetch := func(_ context.Context, request quota.APICallRequest) (*quota.APICallResponse, error) {
				calls++
				if request.URL != kimiUsageURL || request.AuthIndex != auth.Index {
					t.Fatalf("Kimi quota request = %+v", request)
				}
				return &quota.APICallResponse{StatusCode: 200, Body: json.RawMessage(`{"usages":{}}`)}, nil
			}
			_, err := fetchKimiQuotaResultContext(context.Background(), fetch, manager, auth.Index)
			if test.allowed {
				if err != nil || calls != 1 {
					t.Fatalf("Kimi.com quota: error=%v calls=%d", err, calls)
				}
			} else if err == nil || !strings.Contains(err.Error(), "not verified") || calls != 0 {
				t.Fatalf("Kimi.ai quota: error=%v calls=%d", err, calls)
			}
		})
	}
	if _, err := fetchKimiQuotaResultContext(context.Background(),
		func(context.Context, quota.APICallRequest) (*quota.APICallResponse, error) {
			t.Fatal("unknown credential triggered an outbound request")
			return nil, nil
		}, manager, "missing-index"); err == nil {
		t.Fatal("unknown Kimi auth index was accepted")
	}
}

func TestKimiAIQuotaBindingUsesKimiType(t *testing.T) {
	manager := coreauth.NewManager(nil, nil, nil)
	auth, err := manager.Register(context.Background(), &coreauth.Auth{
		ID: "kimi-quota-binding", FileName: "kimi-ai-quota.json", Provider: "kimi-ai",
	})
	if err != nil {
		t.Fatal(err)
	}
	scheduler := quota.NewScheduler(quota.DefaultConfig(), nil)
	if count := registerAuthManagerForQuota(scheduler, manager); count != 1 {
		t.Fatalf("registered quota bindings = %d, want 1", count)
	}
	key := quota.BindingEntryKey(quota.TypeKimi, auth.FileName, auth.Index)
	if scheduler.GetStatus().Credentials[key] == nil {
		t.Fatalf("Kimi.ai quota binding %q missing", key)
	}
}
