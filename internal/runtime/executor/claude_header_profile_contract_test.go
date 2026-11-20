package executor

import (
	"fmt"
	"maps"
	"net/http"
	"reflect"
	"strings"
	"testing"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/runtime/executor/helps"
	cliproxyauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
)

type claudeHeaderSoftwareInput struct {
	userAgent, packageVersion, runtimeVersion, os, arch string
}

func (s claudeHeaderSoftwareInput) headers() http.Header {
	header := make(http.Header)
	for name, value := range map[string]string{
		"User-Agent": s.userAgent, "X-Stainless-Package-Version": s.packageVersion,
		"X-Stainless-Runtime-Version": s.runtimeVersion, "X-Stainless-Os": s.os, "X-Stainless-Arch": s.arch,
	} {
		if value != "" {
			header.Set(name, value)
		}
	}
	return header
}

type claudeHeaderProfileStep struct {
	baseline string
	incoming claudeHeaderSoftwareInput
}

type claudeHeaderProfileCase struct {
	name, authID, apiKey string
	stabilize            *bool
	attributes           map[string]string
	steps                []claudeHeaderProfileStep
}

var claudeHeaderProfileBaselines = map[string]config.ClaudeHeaderDefaults{
	"60": {
		UserAgent: "claude-cli/2.1.60 (external, cli)", PackageVersion: "0.70.0",
		RuntimeVersion: "v22.0.0", OS: "MacOS", Arch: "arm64",
	},
	"70": {
		UserAgent: "claude-cli/2.1.70 (external, cli)", PackageVersion: "0.80.0",
		RuntimeVersion: "v24.5.0", OS: "MacOS", Arch: "arm64",
	},
	"77": {
		UserAgent: "claude-cli/2.1.77 (external, cli)", PackageVersion: "0.87.0",
		RuntimeVersion: "v24.8.0", OS: "MacOS", Arch: "arm64",
	},
	"custom": {
		UserAgent: "my-gateway/1.0", PackageVersion: "custom-pkg",
		RuntimeVersion: "custom-runtime", OS: "MacOS", Arch: "arm64",
	},
	"timeout": {
		UserAgent: "claude-cli/2.1.70 (external, cli)", PackageVersion: "0.80.0",
		RuntimeVersion: "v24.5.0", OS: "MacOS", Arch: "arm64", Timeout: "900",
	},
	"no-platform": {
		UserAgent: "claude-cli/2.1.60 (external, cli)", PackageVersion: "0.70.0",
		RuntimeVersion: "v22.0.0",
	},
}

var claudeHeaderProfileCases = []claudeHeaderProfileCase{
	{
		name: "UsesConfiguredBaselineFingerprint", authID: "auth-baseline", apiKey: "key-baseline",
		stabilize: new(true),
		attributes: map[string]string{
			"header:User-Agent": "evil-client/9.9", "header:X-Stainless-Os": "Linux",
			"header:X-Stainless-Arch": "x64", "header:X-Stainless-Package-Version": "9.9.9",
		},
		steps: []claudeHeaderProfileStep{
			{"timeout", claudeHeaderSoftwareInput{"curl/8.7.1", "0.10.0", "v18.0.0", "Linux", "x64"}},
		},
	},
	{
		name: "TracksHighestClaudeCLIFingerprint", authID: "auth-upgrade", apiKey: "key-upgrade", stabilize: new(true),
		steps: []claudeHeaderProfileStep{
			{"60", claudeHeaderSoftwareInput{"claude-cli/2.1.62 (external, cli)", "0.74.0", "v24.3.0", "Linux", "x64"}},
			{"60", claudeHeaderSoftwareInput{"lobe-chat/1.0", "0.10.0", "v18.0.0", "Windows", "x64"}},
			{
				"60",
				claudeHeaderSoftwareInput{"claude-cli/2.1.63 (external, cli)", "0.75.0", "v24.4.0", "MacOS", "arm64"},
			},
			{
				"60",
				claudeHeaderSoftwareInput{"claude-cli/2.1.61 (external, cli)", "0.73.0", "v24.2.0", "Windows", "x64"},
			},
		},
	},
	{
		name: "DisableDeviceProfileStabilization", authID: "auth-disable-stability",
		apiKey: "key-disable-stability", stabilize: new(false),
		steps: []claudeHeaderProfileStep{
			{"60", claudeHeaderSoftwareInput{"claude-cli/2.1.62 (external, cli)", "0.74.0", "v24.3.0", "Linux", "x64"}},
			{"60", claudeHeaderSoftwareInput{"lobe-chat/1.0", "0.10.0", "v18.0.0", "Windows", "x64"}},
			{
				"60",
				claudeHeaderSoftwareInput{"claude-cli/2.1.61 (external, cli)", "0.73.0", "v24.2.0", "Windows", "x64"},
			},
		},
	},
	{
		name: "LegacyModeFallsBackToRuntimeOSArchWhenMissing", authID: "auth-legacy-runtime-os-arch",
		apiKey: "key-legacy-runtime-os-arch", stabilize: new(false),
		steps: []claudeHeaderProfileStep{{"60", claudeHeaderSoftwareInput{userAgent: "curl/8.7.1"}}},
	},
	{
		name: "DoesNotDowngradeConfiguredBaselineOnFirstClaudeClient", authID: "auth-baseline-floor",
		apiKey: "key-baseline-floor", stabilize: new(true),
		steps: []claudeHeaderProfileStep{
			{"70", claudeHeaderSoftwareInput{"claude-cli/2.1.62 (external, cli)", "0.74.0", "v24.3.0", "Linux", "x64"}},
			{"70", claudeHeaderSoftwareInput{"claude-cli/2.1.71 (external, cli)", "0.81.0", "v24.6.0", "Linux", "x64"}},
		},
	},
	{
		name: "UpgradesCachedSoftwareFingerprintWhenBaselineAdvances", authID: "auth-baseline-reload",
		apiKey: "key-baseline-reload", stabilize: new(true),
		steps: []claudeHeaderProfileStep{
			{"70", claudeHeaderSoftwareInput{"claude-cli/2.1.71 (external, cli)", "0.81.0", "v24.6.0", "Linux", "x64"}},
			{"77", claudeHeaderSoftwareInput{"curl/8.7.1", "0.10.0", "v18.0.0", "Linux", "x64"}},
		},
	},
	{
		name: "LearnsOfficialFingerprintAfterCustomBaselineFallback", authID: "auth-custom-baseline-learning",
		apiKey: "key-custom-baseline-learning", stabilize: new(true),
		steps: []claudeHeaderProfileStep{
			{"custom", claudeHeaderSoftwareInput{"curl/8.7.1", "0.10.0", "v18.0.0", "Linux", "x64"}},
			{
				"custom",
				claudeHeaderSoftwareInput{"claude-cli/2.1.77 (external, cli)", "0.87.0", "v24.8.0", "Linux", "x64"},
			},
			{"custom", claudeHeaderSoftwareInput{"curl/8.7.1", "0.10.0", "v18.0.0", "Linux", "x64"}},
		},
	},
	{
		name: "ThirdPartyBaselineThenOfficialUpgradeKeepsPinnedPlatform", authID: "auth-third-party-then-official",
		apiKey: "key-third-party-then-official", stabilize: new(true),
		steps: []claudeHeaderProfileStep{
			{"70", claudeHeaderSoftwareInput{"curl/8.7.1", "0.10.0", "v18.0.0", "Linux", "x64"}},
			{"70", claudeHeaderSoftwareInput{"claude-cli/2.1.77 (external, cli)", "0.87.0", "v24.8.0", "Linux", "x64"}},
		},
	},
	{
		name: "LegacyModePreservesConfiguredUserAgentOverrideForClaudeClients", authID: "auth-legacy-ua-override",
		apiKey: "key-legacy-ua-override", stabilize: new(false),
		attributes: map[string]string{"header:User-Agent": "config-ua/1.0"},
		steps: []claudeHeaderProfileStep{
			{
				"no-platform",
				claudeHeaderSoftwareInput{"claude-cli/2.1.62 (external, cli)", "0.74.0", "v24.3.0", "Linux", "x64"},
			},
		},
	},
	{
		name: "UnsetStabilizationAlsoUsesLegacyRuntimeOSArchFallback", authID: "auth-unset-runtime-os-arch",
		apiKey: "key-unset-runtime-os-arch",
		steps:  []claudeHeaderProfileStep{{"60", claudeHeaderSoftwareInput{userAgent: "curl/8.7.1"}}},
	},
}

func TestClaudeHeaderProfilesWithHistoricSoftwareInputs(t *testing.T) {
	for _, test := range claudeHeaderProfileCases {
		for _, cliProfile := range []bool{false, true} {
			t.Run(
				fmt.Sprintf("%s/cli=%t", test.name, cliProfile), func(t *testing.T) {
					auth := &cliproxyauth.Auth{ID: test.authID, Attributes: map[string]string{"api_key": test.apiKey}}
					maps.Copy(auth.Attributes, test.attributes)
					if cliProfile {
						auth.Attributes["fingerprint_profile"] = "claude-code-cli"
					}
					for index, step := range test.steps {
						t.Run(
							fmt.Sprintf("request_%d", index), func(t *testing.T) {
								hd := claudeHeaderProfileBaselines[step.baseline]
								hd.StabilizeDeviceProfile = test.stabilize
								cfg := &config.Config{ClaudeHeaderDefaults: hd}
								incoming := step.incoming.headers()
								before := incoming.Clone()
								want := incoming.Clone()
								if cliProfile {
									want = (claudeHeaderSoftwareInput{
										hd.UserAgent, hd.PackageVersion, hd.RuntimeVersion, hd.OS, hd.Arch,
									}).headers()
								}
								if test.stabilize == nil || !*test.stabilize {
									platform := &http.Request{Header: make(http.Header)}
									helps.ApplyClaudeLegacyPlatformHeaders(platform, incoming)
									for _, name := range []string{"X-Stainless-Os", "X-Stainless-Arch"} {
										want.Set(name, platform.Header.Get(name))
									}
								}
								for key, value := range test.attributes {
									want.Set(strings.TrimPrefix(key, "header:"), value)
								}
								request, err := http.NewRequest(
									http.MethodPost, "https://api.anthropic.com/v1/messages", nil,
								)
								if err != nil {
									t.Fatal(err)
								}
								if err := applyClaudeHeaders(
									request, auth, test.apiKey, false, nil, nil,
									cfg, incoming, false, "original-header-session",
								); err != nil {
									t.Fatal(err)
								}
								for _, name := range []string{
									"User-Agent", "X-Stainless-Package-Version", "X-Stainless-Runtime-Version",
									"X-Stainless-Os", "X-Stainless-Arch",
								} {
									if got := request.Header.Get(name); got != want.Get(name) {
										t.Errorf("%s = %q, want %q", name, got, want.Get(name))
									}
								}
								if cliProfile && hd.Timeout != "" &&
									request.Header.Get("X-Stainless-Timeout") != hd.Timeout {
									t.Error("configured timeout was not preserved")
								}
								if !reflect.DeepEqual(incoming, before) {
									t.Error("incoming client headers were modified")
								}
							},
						)
					}
				},
			)
		}
	}
}
