package logging

import (
	"fmt"
	"net/http"
	"sync"
	"testing"
)

func TestResponseHeadersMergePreservesHandshakeAndCopiesValues(t *testing.T) {
	ctx := WithResponseHeadersHolder(t.Context())
	SetResponseHeaders(ctx, http.Header{"X-Request-Id": {"handshake"}, "X-Codex-Secondary-Used-Percent": {"17"}})
	quota := http.Header{"x-codex-primary-used-percent": {"75"}, "": {"ignored"}}
	MergeResponseHeaders(ctx, quota)
	//lint:ignore SA1008 保留小写原始响应头，验证合并结果与原始容器隔离
	quota["x-codex-primary-used-percent"][0] = "mutated-source"
	first := GetResponseHeaders(ctx)
	if first.Get("X-Request-Id") != "handshake" || first.Get("X-Codex-Primary-Used-Percent") != "75" ||
		first.Get("X-Codex-Secondary-Used-Percent") != "17" || len(first) != 3 {
		t.Fatalf("merged headers=%#v", first)
	}
	first.Set("X-Codex-Primary-Used-Percent", "mutated-snapshot")
	if GetResponseHeaders(ctx).Get("X-Codex-Primary-Used-Percent") != "75" {
		t.Fatal("returned snapshot changed stored quota")
	}
	MergeResponseHeaders(ctx, http.Header{"X-Codex-Primary-Used-Percent": {"80"}})
	if GetResponseHeaders(ctx).Get("X-Codex-Primary-Used-Percent") != "80" || first.Get("X-Request-Id") != "handshake" {
		t.Fatal("later observation lost the current value or changed an old snapshot")
	}
}

func TestResponseHeadersMergeConcurrentUpdatesStayWithinAttempt(t *testing.T) {
	parent := WithResponseHeadersHolder(t.Context())
	SetResponseHeaders(parent, http.Header{"X-Request-Id": {"previous-attempt"}})
	ctx := WithResponseHeadersHolder(parent)
	var workers sync.WaitGroup
	for index := range 8 {
		workers.Go(
			func() {
				MergeResponseHeaders(ctx, http.Header{fmt.Sprintf("X-Observation-%d", index): {fmt.Sprint(index)}})
				_ = GetResponseHeaders(ctx)
			},
		)
	}
	workers.Wait()
	merged := GetResponseHeaders(ctx)
	if len(merged) != 8 || merged.Get("X-Request-Id") != "" {
		t.Fatalf("current attempt merged headers=%#v", merged)
	}
	for index := range 8 {
		if merged.Get(fmt.Sprintf("X-Observation-%d", index)) != fmt.Sprint(index) {
			t.Fatalf("concurrent observation %d was lost: %#v", index, merged)
		}
	}
	if previous := GetResponseHeaders(parent); len(previous) != 1 ||
		previous.Get("X-Request-Id") != "previous-attempt" {
		t.Fatalf("current observation changed previous attempt: %#v", previous)
	}
}
