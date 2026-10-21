package quota

import (
	"encoding/json"
	"os"
	"testing"
	"time"
)

func TestCodexResetCreditSharedContract(t *testing.T) {
	data, err := os.ReadFile("testdata/codex_reset_credit_cases.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixture struct {
		CheckedAt string `json:"checked_at"`
		Cases     []struct {
			Name     string          `json:"name"`
			Expected string          `json:"expected"`
			Data     json.RawMessage `json:"data"`
		} `json:"cases"`
	}
	if err = json.Unmarshal(data, &fixture); err != nil {
		t.Fatal(err)
	}
	now, err := time.Parse(time.RFC3339Nano, fixture.CheckedAt)
	if err != nil {
		t.Fatal(err)
	}
	for _, item := range fixture.Cases {
		t.Run(item.Name, func(t *testing.T) {
			actual, errCheck := CodexResetCreditAvailability(item.Data, now)
			if item.Expected == "error" {
				if errCheck == nil {
					t.Fatalf("expected failed current credit query, got %s", actual)
				}
				return
			}
			if errCheck != nil || actual != item.Expected {
				t.Fatalf("availability=%q error=%v, want %q", actual, errCheck, item.Expected)
			}
		})
	}
}
