package home

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/homeplugins"
)

type recordingHomePluginStatusClient struct {
	payload []byte
	err     error
}

func (c *recordingHomePluginStatusClient) RPushPluginStatus(_ context.Context, payload []byte) error {
	c.payload = append([]byte(nil), payload...)
	return c.err
}

func decodePluginStatusReport(t *testing.T, raw string) homeplugins.SyncReport {
	t.Helper()
	var report homeplugins.SyncReport
	if err := json.Unmarshal([]byte(raw), &report); err != nil {
		t.Fatal(err)
	}
	return report
}

func TestReportHomePluginStatusPushesNodeReport(t *testing.T) {
	client := &recordingHomePluginStatusClient{}
	report := decodePluginStatusReport(
		t,
		`{"task":"plugin-sync","status":"success","ok":true,"plugins":[{"id":"sample","install_status":"installed"}]}`,
	)
	if err := ReportPluginStatus(context.Background(), client, " node-1 ", report); err != nil {
		t.Fatal(err)
	}
	var payload homeplugins.SyncReport
	if err := json.Unmarshal(client.payload, &payload); err != nil {
		t.Fatal(err)
	}
	if payload.NodeID != "node-1" || !payload.OK || len(payload.Plugins) != 1 {
		t.Fatalf("payload = %+v, want node report", payload)
	}
	if payload.UpdatedAt.IsZero() {
		t.Fatal("payload UpdatedAt is zero")
	}
}

func TestReportHomePluginStatusPushesEmptyReport(t *testing.T) {
	client := &recordingHomePluginStatusClient{}
	report := decodePluginStatusReport(t, `{"task":"plugin-sync","status":"success","ok":true,"plugins":[]}`)
	if err := ReportPluginStatus(context.Background(), client, "node-1", report); err != nil {
		t.Fatal(err)
	}
	var payload homeplugins.SyncReport
	if err := json.Unmarshal(client.payload, &payload); err != nil {
		t.Fatal(err)
	}
	if payload.NodeID != "node-1" || len(payload.Plugins) != 0 {
		t.Fatalf("payload = %+v, want empty node report", payload)
	}
}

func TestReportHomePluginStatusRequiresNodeID(t *testing.T) {
	client := &recordingHomePluginStatusClient{}
	report := decodePluginStatusReport(t, `{"plugins":[{"id":"sample","install_status":"failed"}]}`)
	if err := ReportPluginStatus(
		context.Background(), client, " ", report,
	); err == nil || !strings.Contains(err.Error(), "node id") {
		t.Fatalf("report error = %v, want node id error", err)
	}
	if len(client.payload) != 0 {
		t.Fatalf("payload = %s, want none", client.payload)
	}
}

func TestReportHomePluginStatusPropagatesPushError(t *testing.T) {
	client := &recordingHomePluginStatusClient{err: errors.New("push failed")}
	report := decodePluginStatusReport(t, `{"plugins":[{"id":"sample","install_status":"installed"}]}`)
	if err := ReportPluginStatus(context.Background(), client, "node-1", report); !errors.Is(err, client.err) {
		t.Fatalf("report error = %v, want push failed", err)
	}
}
