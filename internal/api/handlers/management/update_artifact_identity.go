package management

import (
	"bytes"
	gobuildinfo "debug/buildinfo"
	"fmt"
	"os"
	"regexp"
	"runtime"
	"strings"
)

type updateArtifactIdentity struct {
	Tag            string `json:"tag"`
	Commit         string `json:"commit,omitempty"`
	SHA256         string `json:"sha256"`
	OS             string `json:"os"`
	Arch           string `json:"arch"`
	Legacy         bool   `json:"legacy,omitempty"`
	UpdateProtocol string `json:"update_protocol,omitempty"`
}

var updateCommitPattern = regexp.MustCompile(`^[0-9a-f]{40}$`)
var updateLinkerIdentityPattern = regexp.MustCompile(`(?:^|\s)-X(?:=|\s+)["']?(main\.(?:version|commit)|github\.com/Pyrokine/CLIProxyAPI/v6/internal/buildinfo\.UpdateProtocol)=([^\s"']+)`)

// inspectUpdateArtifact 直接读取 Go 构建元信息，不执行下载的候选程序
func inspectUpdateArtifact(data []byte, currentVersion, currentCommit string) (updateArtifactIdentity, error) {
	info, err := gobuildinfo.Read(bytes.NewReader(data))
	if err != nil {
		return updateArtifactIdentity{}, fmt.Errorf("read executable build identity: %w", err)
	}
	identity := updateArtifactIdentity{Tag: currentVersion, Commit: currentCommit, SHA256: sha256Hex(data)}
	for _, setting := range info.Settings {
		switch setting.Key {
		case "GOOS":
			identity.OS = setting.Value
		case "GOARCH":
			identity.Arch = setting.Value
		case "vcs.revision":
			if !updateCommitPattern.MatchString(identity.Commit) {
				identity.Commit = setting.Value
			}
		case "-ldflags":
			for _, match := range updateLinkerIdentityPattern.FindAllStringSubmatch(setting.Value, -1) {
				switch match[1] {
				case "main.version":
					identity.Tag = match[2]
				case "main.commit":
					if updateCommitPattern.MatchString(match[2]) {
						identity.Commit = match[2]
					}
				case "github.com/Pyrokine/CLIProxyAPI/v6/internal/buildinfo.UpdateProtocol":
					identity.UpdateProtocol = match[2]
				}
			}
		}
	}
	if identity.OS != runtime.GOOS || identity.Arch != runtime.GOARCH || !validBuildVersion.MatchString(identity.Tag) {
		return updateArtifactIdentity{}, fmt.Errorf("executable version or platform mismatch")
	}
	if !updateCommitPattern.MatchString(identity.Commit) {
		if currentVersion == "" {
			return updateArtifactIdentity{}, fmt.Errorf("release executable does not contain a full commit identity")
		}
		identity.Commit = ""
		identity.Legacy = true
	}
	if currentVersion == "" && identity.UpdateProtocol != fmt.Sprint(updateGuardStateSchemaVersion) {
		return updateArtifactIdentity{}, fmt.Errorf("release executable does not support the required update protocol")
	}
	return identity, nil
}

func readUpdateArtifact(path, currentVersion, currentCommit string) (updateArtifactIdentity, error) {
	if err := requireRegularFile(path); err != nil {
		return updateArtifactIdentity{}, err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return updateArtifactIdentity{}, err
	}
	return inspectUpdateArtifact(data, currentVersion, currentCommit)
}

func validateUpdateArtifact(identity updateArtifactIdentity, version string, allowLegacy bool) error {
	if normalizeUpdateHealthVersion(identity.Tag) != normalizeUpdateHealthVersion(version) ||
		identity.OS != runtime.GOOS || identity.Arch != runtime.GOARCH || !isUpdateHealthToken(identity.SHA256) {
		return fmt.Errorf("invalid executable identity")
	}
	if !allowLegacy && identity.UpdateProtocol != fmt.Sprint(updateGuardStateSchemaVersion) {
		return fmt.Errorf("executable update protocol is incompatible")
	}
	if identity.Legacy {
		if !allowLegacy || identity.Commit != "" {
			return fmt.Errorf("legacy executable identity is not permitted")
		}
	} else if !updateCommitPattern.MatchString(identity.Commit) {
		return fmt.Errorf("invalid executable commit identity")
	}
	return nil
}

func verifyUpdateArtifactFile(path string, identity updateArtifactIdentity) error {
	if err := requireRegularFile(path); err != nil {
		return err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	if !strings.EqualFold(sha256Hex(data), identity.SHA256) {
		return fmt.Errorf("executable digest does not match the update identity")
	}
	return nil
}

func updateBackupPath(executablePath, attemptID string) string {
	if executablePath == "" || !isUpdateAttemptID(attemptID) {
		return ""
	}
	return executablePath + ".update-" + attemptID + ".bak"
}
