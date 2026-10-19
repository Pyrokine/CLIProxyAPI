package codex

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestCodexPlanTypeDefaultsToFreeWhenClaimIsAbsent(t *testing.T) {
	for _, tc := range []struct {
		claims *JWTClaims
		want   string
	}{
		{nil, "free"},
		{&JWTClaims{}, "free"},
		{&JWTClaims{AuthInfo: AuthInfo{ChatgptPlanType: " team "}}, "team"},
	} {
		if got := tc.claims.GetPlanType(); got != tc.want {
			t.Fatalf("GetPlanType() = %q, want %q", got, tc.want)
		}
	}
}

func TestCodexTokenStoragePersistsPlanType(t *testing.T) {
	for _, tc := range []struct{ name, plan, want string }{
		{"missing", "", "free"},
		{"specified", "plus", "plus"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			storage := (&Auth{}).CreateTokenStorage(&AuthBundle{TokenData: TokenData{PlanType: tc.plan}})
			if storage.PlanType != tc.want {
				t.Fatalf("storage plan = %q, want %q", storage.PlanType, tc.want)
			}
			path := filepath.Join(t.TempDir(), "codex.json")
			if err := storage.SaveTokenToFile(path); err != nil {
				t.Fatal(err)
			}
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			var record map[string]any
			if err := json.Unmarshal(data, &record); err != nil {
				t.Fatal(err)
			}
			if got := record["plan_type"]; got != tc.want {
				t.Fatalf("persisted plan = %v, want %q", got, tc.want)
			}
		})
	}
}
