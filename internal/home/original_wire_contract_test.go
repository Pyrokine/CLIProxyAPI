package home

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

func TestHomeAuthWirePreservesLegacyAndExplicitRetryRoundInputs(t *testing.T) {
	for _, sample := range []struct {
		name, entry string
		constraints AuthDispatchConstraints
	}{
		{name: "legacy", entry: "legacy"},
		{name: "legacy fixed policy", entry: "policy"},
		{name: "initial round", entry: "round"},
		{
			name: "explicit empty exclusions", entry: "round",
			constraints: AuthDispatchConstraints{ExcludedAuthIDs: []string{}},
		},
		{
			name: "additional round", entry: "round",
			constraints: AuthDispatchConstraints{RetryRound: 2, ExcludedAuthIDs: []string{"auth-a"}},
		},
		{name: "negative round normalization", entry: "round", constraints: AuthDispatchConstraints{RetryRound: -1}},
	} {
		t.Run(
			sample.name, func(t *testing.T) {
				const response = `{"auth":{"id":"synthetic-auth"}}`
				client, commands := newC08RedisCommandClient(
					t, func(args []string) string {
						switch strings.ToUpper(args[0]) {
						case "HELLO":
							return "-ERR unknown command 'HELLO'\r\n"
						case "PING":
							return "+PONG\r\n"
						case "RPOP":
							return fmt.Sprintf("$%d\r\n%s\r\n", len(response), response)
						default:
							return "+OK\r\n"
						}
					},
				)
				var raw []byte
				var err error
				switch sample.entry {
				case "legacy":
					raw, err = client.RPopAuth(t.Context(), "gpt-5.4", "session", nil, 3)
				case "policy":
					raw, err = client.RPopAuthWithPolicy(t.Context(), "gpt-5.4", "session", nil, 3, "fixed-policy")
				case "round":
					raw, err = client.RPopAuthWithConstraints(
						t.Context(), "gpt-5.4", "session", nil, 3, sample.constraints,
					)
				}
				if err != nil || string(raw) != response {
					t.Fatalf("dispatch response=%s error=%v", raw, err)
				}
				command := commands.last()
				if len(command) != 2 || !strings.EqualFold(command[0], "RPOP") {
					t.Fatalf("dispatch command=%v", command)
				}
				var wire map[string]any
				if err := json.Unmarshal([]byte(command[1]), &wire); err != nil {
					t.Fatal(err)
				}
				wantCount := float64(3)
				if sample.entry == "round" {
					wantCount = 1
				}
				if wire["count"] != wantCount || wire["model"] != "gpt-5.4" || wire["session_id"] != "session" {
					t.Errorf("dispatch count or identity changed: %s", command[1])
				}
				round, present := wire["retry_round"]
				if present != (sample.entry == "round") || (present && round != float64(
					max(
						sample.constraints.RetryRound, 0,
					),
				)) {
					t.Errorf(
						"retry_round=%v present=%t, want explicit round only for round protocol: %s", round, present,
						command[1],
					)
				}
				exclusions, present := wire["excluded_auth_ids"]
				if present != (sample.constraints.ExcludedAuthIDs != nil) {
					t.Fatalf(
						"excluded_auth_ids=%v present=%t lost absent/empty distinction: %s", exclusions, present,
						command[1],
					)
				}
				if present {
					ids, valid := exclusions.([]any)
					if !valid || len(ids) != len(sample.constraints.ExcludedAuthIDs) {
						t.Fatalf("excluded_auth_ids=%v, want %v", exclusions, sample.constraints.ExcludedAuthIDs)
					}
					for index, id := range ids {
						if id != sample.constraints.ExcludedAuthIDs[index] {
							t.Fatalf("exclusion %d=%v, want %s", index, id, sample.constraints.ExcludedAuthIDs[index])
						}
					}
				}
				if sample.entry == "policy" && wire["credential_policy"] != "fixed-policy" {
					t.Fatalf("legacy policy missing: %s", command[1])
				}
			},
		)
	}
}
