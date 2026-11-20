package auth

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	internalconfig "github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/logging"
	cliproxyexecutor "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
)

func TestManagerHomeLegacyWireDecisionReachesLogWithoutLocalCandidates(t *testing.T) {
	const model = "home-wire-model"
	const payload = `{"model":"home-upstream-model","provider":"codex","auth_index":"home-wire-selected-` +
		`index","auth":{"id":"home-wire-selected-id","provider":"codex"}}`
	for _, mode := range []string{"execute", "count", "stream"} {
		t.Run(
			mode, func(t *testing.T) {
				manager := NewManager(nil, nil, nil)
				executor := &routeTraceMetadataExecutor{
					routeTraceTestExecutor: &routeTraceTestExecutor{provider: "codex"},
				}
				manager.RegisterExecutor(executor)
				manager.SetConfig(&internalconfig.Config{Home: internalconfig.HomeConfig{Enabled: true}})
				registerRouteTraceTestAuths(
					t, manager, "codex", model, &Auth{
						ID: "home-wire-local-only", Provider: "codex", Disabled: true,
						Attributes: map[string]string{"priority": "99"},
					},
				)
				dispatcher := &testHomeDispatcher{heartbeat: true, payload: []byte(payload)}
				previous := currentHomeDispatcher
				currentHomeDispatcher = func() homeAuthDispatcher { return dispatcher }
				t.Cleanup(func() { currentHomeDispatcher = previous })
				trace := logging.NewRouteTrace()
				opts := routeTraceTestOptions(trace.Callback())
				req := cliproxyexecutor.Request{Model: model}
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				var err error
				switch mode {
				case "execute":
					_, err = manager.Execute(ctx, []string{"codex"}, req, opts)
				case "count":
					_, err = manager.ExecuteCount(ctx, []string{"codex"}, req, opts)
				case "stream":
					var result *cliproxyexecutor.StreamResult
					result, err = manager.ExecuteStream(ctx, []string{"codex"}, req, opts)
					if err == nil {
						for chunk := range result.Chunks {
							if chunk.Err != nil {
								t.Fatalf("stream chunk: %v", chunk.Err)
							}
						}
					}
				}
				if err != nil {
					t.Fatalf("%s: %v", mode, err)
				}
				if dispatcher.calls != 1 {
					t.Fatalf("dispatch calls = %d, want 1", dispatcher.calls)
				}

				var output bytes.Buffer
				if errWrite := logging.WriteRouteTrace(&output, trace.Snapshot()); errWrite != nil {
					t.Fatalf("WriteRouteTrace: %v", errWrite)
				}
				if strings.Contains(output.String(), "home-wire-selected") ||
					strings.Contains(output.String(), "home-wire-local-only") {
					t.Fatalf("route log exposed raw Home identity or a local-only candidate: %s", output.String())
				}
				scanner := bufio.NewScanner(&output)
				if !scanner.Scan() || scanner.Text() != "=== ROUTE TRACE ===" {
					t.Fatal("route trace marker missing")
				}
				decisions := 0
				for scanner.Scan() {
					if strings.TrimSpace(scanner.Text()) == "" {
						continue
					}
					var event cliproxyexecutor.RouteTraceEvent
					if errDecode := json.Unmarshal(scanner.Bytes(), &event); errDecode != nil {
						t.Fatalf("decode route trace line: %v", errDecode)
					}
					if event.Action != "route.decision" {
						continue
					}
					decisions++
					decision := event.Decision
					if decision == nil || decision.CandidateScope != "selected_only" ||
						len(decision.Candidates) != 1 || decision.Selected == nil ||
						decision.TruncatedCandidates != 0 || decision.MatchedRule != "home_dispatch" {
						t.Fatalf("legacy Home decision = %+v", decision)
					}
					candidate := decision.Candidates[0]
					if !candidate.Selected || candidate.AuthIndex != decision.Selected.AuthIndex ||
						candidate.AuthIndex != stableAuthIndex("id:home-wire-selected-id") ||
						len(
							candidate.ExcludedReasons,
						) != 0 || candidate.QuotaHeadroom != nil || candidate.InFlight != nil {
						t.Fatalf("legacy Home candidate = %+v", candidate)
					}
				}
				if errScan := scanner.Err(); errScan != nil {
					t.Fatalf("scan route trace: %v", errScan)
				}
				if decisions != 1 {
					t.Fatalf("logged decisions = %d, want 1", decisions)
				}
			},
		)
	}
}
