package management

import (
	"strings"
	"time"

	coreauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
)

type accountInspectionExpectedProbe struct {
	AuthGeneration   uint64    `json:"auth_generation"`
	CredentialDigest string    `json:"credential_digest"`
	ProbeRevision    string    `json:"probe_revision"`
	CheckedAt        time.Time `json:"checked_at"`
}

func (expected accountInspectionExpectedProbe) complete() bool {
	return expected.AuthGeneration != 0 && strings.TrimSpace(expected.CredentialDigest) != "" &&
		strings.TrimSpace(expected.ProbeRevision) != "" && !expected.CheckedAt.IsZero()
}

func (s *accountInspectionService) expectedProbeCurrentLocked(
	target *coreauth.Auth,
	expected *accountInspectionExpectedProbe,
) bool {
	if target == nil || expected == nil || expected.ProbeRevision != accountInspectionProbeRevision ||
		target.Generation != expected.AuthGeneration ||
		!inspectionAuthBindingCurrent(s.handler.authManager, target, expected.CredentialDigest) {
		return false
	}
	var latest *accountInspectionResult
	for i := range s.results {
		result := &s.results[i]
		if result.ID == target.ID && (latest == nil || !result.CheckedAt.Before(latest.CheckedAt)) {
			latest = result
		}
	}
	return latest != nil && latest.ProbeStatus == accountProbePassed &&
		latest.AuthIndex == target.Index && strings.TrimSpace(latest.FileName) == strings.TrimSpace(target.FileName) &&
		strings.EqualFold(latest.Provider, target.Provider) &&
		latest.AuthGeneration == expected.AuthGeneration &&
		strings.EqualFold(latest.CredentialDigest, expected.CredentialDigest) &&
		latest.ProbeRevision == expected.ProbeRevision && latest.CheckedAt.Equal(expected.CheckedAt)
}
