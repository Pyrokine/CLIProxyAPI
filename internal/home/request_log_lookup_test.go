package home

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"testing"
)

func requestLogRedisHandler(entries []string) func([]string) string {
	return func(args []string) string {
		if len(args) == 0 {
			return "-ERR missing command\r\n"
		}
		switch strings.ToUpper(args[0]) {
		case "LLEN":
			return fmt.Sprintf(":%d\r\n", len(entries))
		case "LRANGE":
			if len(args) != 4 {
				return "-ERR invalid LRANGE\r\n"
			}
			start, errStart := strconv.Atoi(args[2])
			end, errEnd := strconv.Atoi(args[3])
			if errStart != nil || errEnd != nil || start < 0 || end < start {
				return "-ERR invalid range\r\n"
			}
			if start >= len(entries) {
				return "*0\r\n"
			}
			end = min(end, len(entries)-1)
			return redisBulkStringArray(entries[start : end+1])
		default:
			return "-ERR unexpected command\r\n"
		}
	}
}

func redisBulkStringArray(values []string) string {
	var response strings.Builder
	_, _ = fmt.Fprintf(&response, "*%d\r\n", len(values))
	for _, value := range values {
		_, _ = fmt.Fprintf(&response, "$%d\r\n%s\r\n", len(value), value)
	}
	return response.String()
}

func TestFindRequestLogScansNewestEntriesFirst(t *testing.T) {
	entries := []string{
		`{"request_id":"older","request_log":"old"}`,
		`not-json`,
		`{"request_id":"target","request_log":"older target"}`,
		`{"request_id":"other-1","request_log":"1"}`,
		`{"request_id":"other-2","request_log":"2"}`,
		`{"request_id":"other-3","request_log":"3"}`,
		`{"request_id":"other-4","request_log":"4"}`,
		`{"request_id":"other-5","request_log":"5"}`,
		`{"request_id":"target","request_log":"newer target"}`,
		`{"request_id":"latest","request_log":"latest"}`,
	}
	client, commands := newC08RedisCommandClient(t, requestLogRedisHandler(entries))

	content, found, err := client.FindRequestLog(context.Background(), " target ")
	if err != nil {
		t.Fatalf("FindRequestLog: %v", err)
	}
	if !found || content != "newer target" {
		t.Fatalf("FindRequestLog = (%q, %t), want newer target", content, found)
	}
	if got := commands.count("LRANGE", redisKeyRequestLog); got != 1 {
		t.Fatalf("LRANGE calls = %d, want 1", got)
	}
}

func TestFindRequestLogReadsOlderBatches(t *testing.T) {
	entries := make([]string, 12)
	for index := range entries {
		entries[index] = fmt.Sprintf(`{"request_id":"other-%d","request_log":"%d"}`, index, index)
	}
	entries[2] = `{"request_id":"target","request_log":"from older batch"}`
	client, commands := newC08RedisCommandClient(t, requestLogRedisHandler(entries))

	content, found, err := client.FindRequestLog(context.Background(), "target")
	if err != nil {
		t.Fatalf("FindRequestLog: %v", err)
	}
	if !found || content != "from older batch" {
		t.Fatalf("FindRequestLog = (%q, %t), want older batch entry", content, found)
	}
	if got := commands.count("LRANGE", redisKeyRequestLog); got != 2 {
		t.Fatalf("LRANGE calls = %d, want 2", got)
	}
}

func TestFindRequestLogHonorsScanLimit(t *testing.T) {
	entries := make([]string, homeRequestLogLookupMaxEntries+1)
	entries[0] = `{"request_id":"outside-limit","request_log":"oldest"}`
	for index := 1; index < len(entries); index++ {
		entries[index] = fmt.Sprintf(`{"request_id":"other-%d","request_log":"%d"}`, index, index)
	}
	client, commands := newC08RedisCommandClient(t, requestLogRedisHandler(entries))

	content, found, err := client.FindRequestLog(context.Background(), "outside-limit")
	if err != nil {
		t.Fatalf("FindRequestLog: %v", err)
	}
	if found || content != "" {
		t.Fatalf("FindRequestLog = (%q, %t), want not found beyond scan limit", content, found)
	}
	wantCalls := homeRequestLogLookupMaxEntries / homeRequestLogLookupBatchSize
	if got := commands.count("LRANGE", redisKeyRequestLog); got != wantCalls {
		t.Fatalf("LRANGE calls = %d, want %d", got, wantCalls)
	}
}

func TestFindRequestLogRejectsMatchedEntryWithoutLogContent(t *testing.T) {
	client, _ := newC08RedisCommandClient(
		t,
		requestLogRedisHandler([]string{`{"request_id":"target"}`}),
	)

	if _, found, err := client.FindRequestLog(context.Background(), "target"); err == nil || found ||
		!strings.Contains(err.Error(), "has no request_log field") {
		t.Fatalf("FindRequestLog = found %t, error %v; want missing field error", found, err)
	}
}
