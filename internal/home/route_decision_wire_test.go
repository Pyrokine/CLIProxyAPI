package home

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

func TestHomeAuthDispatchLegacyWirePreservesSelectedOnlyResponse(t *testing.T) {
	const payload = `{"model":"home-upstream-model","provider":"codex","auth_index":"home-wire-selected-` +
		`index","auth":{"id":"home-wire-selected-id","provider":"codex"}}`
	client, commands := newC08RedisCommandClient(
		t, func(args []string) string {
			if len(args) == 0 {
				return "-ERR empty command\r\n"
			}
			switch strings.ToUpper(args[0]) {
			case "PING":
				return "+PONG\r\n"
			case "RPOP":
				return fmt.Sprintf("$%d\r\n%s\r\n", len(payload), payload)
			default:
				return "-ERR unexpected command\r\n"
			}
		},
	)
	constraints := AuthDispatchConstraints{RetryRound: 2, ExcludedAuthIDs: []string{"previous-auth"}}
	raw, err := client.RPopAuthWithConstraints(context.Background(), "home-wire-model", "session", nil, 1, constraints)
	if err != nil {
		t.Fatalf("RPopAuthWithConstraints: %v", err)
	}
	if string(raw) != payload {
		t.Fatalf("Home response changed: %s", raw)
	}
	command := commands.last()
	if len(command) != 2 || command[0] != "rpop" {
		t.Fatalf("dispatch command = %v", command)
	}
	var request authDispatchRequest
	if errDecode := json.Unmarshal([]byte(command[1]), &request); errDecode != nil {
		t.Fatalf("decode dispatch request: %v", errDecode)
	}
	if request.Type != "auth" || request.Model != "home-wire-model" || request.Count != 1 ||
		request.RetryRound == nil || *request.RetryRound != 2 ||
		len(request.ExcludedAuthIDs) != 1 || request.ExcludedAuthIDs[0] != "previous-auth" {
		t.Fatalf("dispatch request = %+v", request)
	}
	if got := commands.count("RPOP", command[1]); got != 1 {
		t.Fatalf("dispatch round trips = %d, want 1", got)
	}
}
