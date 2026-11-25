package managementasset

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// ManagementHTMLState 比较磁盘面板与目标发布的 SHA-256 摘要，读取期间不下载或发布资产
type ManagementHTMLState struct {
	Installed     bool   `json:"installed"`
	Digest        string `json:"digest"`
	TargetDigest  string `json:"target_digest"`
	MatchesTarget bool   `json:"matches_target"`
}

func ReadManagementHTMLState(
	ctx context.Context,
	staticDir, proxyURL, panelRepository, version string,
) (ManagementHTMLState, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return ManagementHTMLState{}, err
	}
	staticDir = strings.TrimSpace(staticDir)
	if staticDir == "" {
		return ManagementHTMLState{}, errors.New("management asset static directory is empty")
	}
	_, targetHash, err := getReleaseAsset(ctx, proxyURL, panelRepository, strings.TrimSpace(version))
	if err != nil {
		return ManagementHTMLState{}, fmt.Errorf("fetch management release: %w", err)
	}
	state := ManagementHTMLState{TargetDigest: "sha256:" + targetHash}
	localHash, err := fileSHA256(filepath.Join(staticDir, managementAssetName))
	if errors.Is(err, os.ErrNotExist) {
		return state, nil
	}
	if err != nil {
		return ManagementHTMLState{}, fmt.Errorf("read management asset: %w", err)
	}
	state.Installed = true
	state.Digest = "sha256:" + localHash
	state.MatchesTarget = strings.EqualFold(localHash, targetHash)
	return state, nil
}
