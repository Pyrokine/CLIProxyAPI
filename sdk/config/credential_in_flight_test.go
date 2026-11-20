package config_test

import (
	"testing"
	"time"

	sdkconfig "github.com/Pyrokine/CLIProxyAPI/v6/sdk/config"
)

func TestDefaultCredentialInFlightConfigPublicContract(t *testing.T) {
	var factory = sdkconfig.DefaultCredentialInFlightConfig
	defaults := factory()
	want := sdkconfig.CredentialInFlightConfig{
		SnapshotInterval: "2s", StaleAfter: "10s", StagingRetention: "1m",
		MaxPartBytes: 256 * 1024, MaxPartCount: 64, MaxRevisionBytes: 16 * 1024 * 1024,
		MaxAggregateGroups: 100000, MaxDetails: 10000, MaxStringBytes: 256,
	}
	if defaults != want {
		t.Fatalf("public defaults=%+v, want %+v", defaults, want)
	}
	if err := defaults.Validate(); err != nil {
		t.Fatal(err)
	}
	snapshot, stale, retention, err := defaults.Durations()
	if err != nil || snapshot != 2*time.Second || stale != 10*time.Second || retention != time.Minute {
		t.Fatalf("durations=%v/%v/%v error=%v", snapshot, stale, retention, err)
	}
	defaults.MaxDetails = 0
	if next := factory(); next != want {
		t.Fatalf("caller mutation changed later defaults: %+v", next)
	}
}
