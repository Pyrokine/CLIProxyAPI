package home

import (
	"context"
	"errors"
	"math"
	"reflect"
	"strings"
	"testing"
	"time"
)

type publishedKVOperations interface {
	KVTTL(context.Context, string) (time.Duration, bool, error)
	KVIncrBy(context.Context, string, int64) (int64, error)
	KVMGet(context.Context, ...string) ([][]byte, []bool, error)
	KVMSet(context.Context, map[string][]byte) error
}

func TestPublishedKVOperationsPreserveWireAndResults(t *testing.T) {
	client, commands := newC08RedisCommandClient(
		t, func(args []string) string {
			switch strings.ToUpper(args[0]) {
			case "TTL":
				switch args[1] {
				case "missing":
					return ":-2\r\n"
				case "persistent":
					return ":-1\r\n"
				case "expires-now":
					return ":0\r\n"
				default:
					return ":12\r\n"
				}
			case "INCRBY":
				return ":9007199254740993\r\n"
			case "MGET":
				return "*3\r\n$5\r\nvalue\r\n$-1\r\n$0\r\n\r\n"
			case "MSET":
				return "+OK\r\n"
			default:
				return "-ERR unexpected command\r\n"
			}
		},
	)
	operations, ok := any(client).(publishedKVOperations)
	if !ok {
		t.Fatal("Home client is missing published TTL, INCRBY, MGET or MSET operations")
	}
	for _, sample := range []struct {
		key   string
		ttl   time.Duration
		found bool
	}{
		{"key", 12 * time.Second, true},
		{"missing", 0, false},
		{"persistent", 0, true},
		{"expires-now", 0, true},
	} {
		ttl, found, err := operations.KVTTL(t.Context(), sample.key)
		if err != nil || ttl != sample.ttl || found != sample.found {
			t.Fatalf(
				"TTL %q = (%s, %t, %v), want (%s, %t, nil)",
				sample.key, ttl, found, err, sample.ttl, sample.found,
			)
		}
	}
	value, err := operations.KVIncrBy(t.Context(), "key", -3)
	if err != nil || value != 9007199254740993 {
		t.Fatalf("INCRBY = (%d, %v)", value, err)
	}
	if got, want := commands.last(), []string{"incrby", "key", "-3"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("INCRBY command = %q, want %q", got, want)
	}
	values, found, err := operations.KVMGet(t.Context(), "hit", "miss", "empty")
	if err != nil || !reflect.DeepEqual(found, []bool{true, false, true}) ||
		len(values) != 3 || string(values[0]) != "value" || values[1] != nil || len(values[2]) != 0 {
		t.Fatalf("MGET = (%q, %v, %v)", values, found, err)
	}
	if err := operations.KVMSet(t.Context(), map[string][]byte{"b": []byte("2"), "a": []byte("1")}); err != nil {
		t.Fatal(err)
	}
	if got, want := commands.last(), []string{"MSET", "a", "1", "b", "2"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("MSET command = %q, want %q", got, want)
	}
}

func TestPublishedKVOperationsKeepEmptyAndFailureSemantics(t *testing.T) {
	client, _ := newC08RedisCommandClient(t, func([]string) string { return "-ERR home kv unavailable\r\n" })
	operations, ok := any(client).(publishedKVOperations)
	if !ok {
		t.Fatal("Home client is missing published KV operations")
	}
	if values, found, err := operations.KVMGet(t.Context()); values != nil || found != nil || err != nil {
		t.Fatalf("empty MGET = (%v, %v, %v)", values, found, err)
	}
	if err := operations.KVMSet(t.Context(), nil); err != nil {
		t.Fatalf("empty MSET = %v", err)
	}
	for name, call := range map[string]func() error{
		"TTL":    func() error { _, _, err := operations.KVTTL(t.Context(), "key"); return err },
		"INCRBY": func() error { _, err := operations.KVIncrBy(t.Context(), "key", 1); return err },
		"MGET":   func() error { _, _, err := operations.KVMGet(t.Context(), "key"); return err },
		"MSET":   func() error { return operations.KVMSet(t.Context(), map[string][]byte{"key": []byte("value")}) },
	} {
		t.Run(
			name, func(t *testing.T) {
				if err := call(); err == nil || !strings.Contains(err.Error(), "ERR home kv unavailable") {
					t.Fatalf("error = %v, want original Home error", err)
				}
			},
		)
	}
	client.Close()
	if _, _, err := operations.KVTTL(t.Context(), "key"); !errors.Is(err, errDispatchFenced) {
		t.Fatalf("closed client error = %v", err)
	}
}

func TestPublishedKVTTLConversionDoesNotOverflow(t *testing.T) {
	for _, unit := range []time.Duration{time.Second, time.Millisecond} {
		want := int64(time.Duration(math.MaxInt64)/unit + 1)
		if got := durationCeil(time.Duration(math.MaxInt64), unit); got != want {
			t.Fatalf("durationCeil(max, %s) = %d, want %d", unit, got, want)
		}
	}
}
