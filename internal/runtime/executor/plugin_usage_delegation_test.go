package executor

import (
	"sync/atomic"
	"testing"

	cliproxyauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
)

func TestPluginUsageDelegationStartsWithActualReporter(t *testing.T) {
	parent := new(atomic.Bool)
	//lint:ignore SA1012 验证已支持的 nil 父 context 输入
	ctx := WithPluginExecutorUsageDelegation(nil, parent)
	if parent.Load() {
		t.Fatal("preparing a nested call marked an attempt that has not started")
	}
	if reporter := NewPluginExecutorUsageReporter(
		ctx, "fixture-provider", "fixture-model", nil,
	); reporter != nil || parent.Load() {
		t.Fatal("a missing selected credential marked delegated usage")
	}
	auth := &cliproxyauth.Auth{ID: "fixture-selected", Index: "fixture-index"}
	child := new(atomic.Bool)
	childCtx := WithPluginExecutorUsageDelegation(ctx, child)
	if reporter := NewPluginExecutorUsageReporter(
		childCtx, "fixture-provider", "fixture-model", auth,
	); reporter == nil || !child.Load() || parent.Load() {
		t.Fatal("nested reporter did not claim only its own delegation")
	}
	if reporter := newUsageReporter(
		ctx, "fixture-native-provider", "fixture-model", auth,
	); reporter == nil || !parent.Load() {
		t.Fatal("native selected reporter did not mark the parent delegation")
	}
}
