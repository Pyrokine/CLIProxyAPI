package home

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/homeplugins"
)

const pluginStatusReportTimeout = 10 * time.Second

type PluginStatusClient interface {
	RPushPluginStatus(ctx context.Context, payload []byte) error
}

func ReportPluginStatus(
	ctx context.Context,
	client PluginStatusClient,
	nodeID string,
	report homeplugins.SyncReport,
) error {
	if client == nil {
		return fmt.Errorf("home plugin status client is unavailable")
	}
	nodeID = strings.TrimSpace(nodeID)
	if nodeID == "" {
		return fmt.Errorf("home plugin status node id is empty")
	}
	report.NodeID = nodeID
	report.UpdatedAt = time.Now().UTC()
	raw, errMarshal := json.Marshal(report)
	if errMarshal != nil {
		return errMarshal
	}
	if ctx == nil {
		ctx = context.Background()
	}
	reportCtx, cancel := context.WithTimeout(ctx, pluginStatusReportTimeout)
	defer cancel()
	return client.RPushPluginStatus(reportCtx, raw)
}
