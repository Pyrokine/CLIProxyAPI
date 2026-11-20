package helps

import (
	"fmt"
	"net/http"
	"testing"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
)

func TestApplyClaudeLegacyDeviceHeadersPreservesPlatformInputs(t *testing.T) {
	for _, configured := range []bool{false, true} {
		for _, confirmed := range []bool{false, true} {
			for _, platform := range []string{"missing", "incoming", "prepared"} {
				t.Run(
					fmt.Sprintf(
						"configured=%t/confirmed=%t/%s",
						configured,
						confirmed,
						platform,
					),
					func(t *testing.T) {
						cfg := &config.Config{
							ClaudeHeaderDefaults: config.ClaudeHeaderDefaults{
								UserAgent: "claude-cli/2.1.60 (external, cli)", PackageVersion: "0.70.0",
								RuntimeVersion: "v22.0.0",
								OS:             "MacOS", Arch: "arm64",
							},
						}
						if configured {
							cfg.ClaudeHeaderDefaults.StabilizeDeviceProfile = new(false)
						}
						request, err := http.NewRequest(
							http.MethodPost, "https://platform-contract.invalid/v1/messages", nil,
						)
						if err != nil {
							t.Fatal(err)
						}
						incoming := http.Header{"User-Agent": {"curl/8.7.1"}}
						wantOS, wantArch := mapStainlessOS(), mapStainlessArch()
						if platform != "missing" {
							incoming["X-Stainless-OS"], incoming["X-Stainless-Arch"] = []string{
								"Windows",
							}, []string{
								"x64",
							}
							wantOS, wantArch = "Windows", "x64"
						}
						if platform == "prepared" {
							request.Header.Set("X-Stainless-Os", "FreeBSD")
							request.Header.Set("X-Stainless-Arch", "other::riscv64")
							wantOS, wantArch = "FreeBSD", "other::riscv64"
						}
						ApplyClaudeLegacyDeviceHeaders(request, incoming, cfg, confirmed)
						if gotOS, gotArch := request.Header.Get("X-Stainless-Os"), request.Header.Get(
							"X-Stainless-Ar"+
								"ch",
						); gotOS !=
							wantOS ||
							gotArch != wantArch {
							t.Fatalf("platform=%s/%s, want %s/%s", gotOS, gotArch, wantOS, wantArch)
						}
						if request.Header.Get("User-Agent") != cfg.ClaudeHeaderDefaults.UserAgent ||
							request.Header.Get(
								"X-Stainless-Package-Version",
							) != cfg.ClaudeHeaderDefaults.PackageVersion ||
							request.Header.Get(
								"X-Stainless-Runtime-Version",
							) != cfg.ClaudeHeaderDefaults.RuntimeVersion {
							t.Fatalf(
								"platform compatibility changed the configured software profile: %v",
								request.Header,
							)
						}
						//lint:ignore SA1008 验证原始 OS 大写请求头输入保持不变
						if platform != "missing" && incoming["X-Stainless-OS"][0] != "Windows" {
							t.Fatal("incoming platform headers were modified")
						}
					},
				)
			}
		}
	}
}
