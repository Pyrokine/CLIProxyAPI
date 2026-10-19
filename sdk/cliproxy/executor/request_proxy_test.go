package executor

import (
	"context"
	"testing"
)

func TestRequestProxyContextIsolation(t *testing.T) {
	parent := WithRequestProxyURL(context.Background(), " http://request-proxy.example:8080 ")
	if got := RequestProxyURL(parent); got != "http://request-proxy.example:8080" {
		t.Fatalf("request proxy = %q", got)
	}
	refresh := WithoutRequestProxyURL(parent)
	if got := RequestProxyURL(refresh); got != "" {
		t.Fatalf("refresh inherited request proxy %q", got)
	}
	if got := RequestProxyURL(parent); got != "http://request-proxy.example:8080" {
		t.Fatalf("clearing child context mutated parent proxy %q", got)
	}
	//lint:ignore SA1012 验证公开函数接受 nil context 的既有合同
	if got := RequestProxyURL(WithRequestProxyURL(nil, "direct")); got != "direct" {
		t.Fatalf("nil-context override = %q, want direct", got)
	}
	//lint:ignore SA1012 验证无 context 时代理查询返回空值
	if got := RequestProxyURL(nil); got != "" {
		t.Fatalf("nil-context proxy = %q, want empty", got)
	}
}
