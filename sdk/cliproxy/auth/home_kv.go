package auth

import (
	"context"
	"fmt"
	"time"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/home"
)

// HomeKVSetOptions 指定 SET 的过期时间及 NX、XX 条件
//
//goland:noinspection GoUnnecessarilyExportedIdentifiers
type HomeKVSetOptions = home.KVSetOptions

// HomeKVClient 提供当前请求所属 Home 的常用 KV 操作
//
//goland:noinspection GoUnnecessarilyExportedIdentifiers
type HomeKVClient interface {
	home.KVClient
	KVTTL(context.Context, string) (time.Duration, bool, error)
	KVIncrBy(context.Context, string, int64) (int64, error)
	KVMGet(context.Context, ...string) ([][]byte, []bool, error)
	KVMSet(context.Context, map[string][]byte) error
}

// HomeKVClientFromContext 返回请求的 KV 客户端，Home 不可用时显式返回错误
//
//goland:noinspection GoUnnecessarilyExportedIdentifiers
func HomeKVClientFromContext(ctx context.Context) (HomeKVClient, bool, error) {
	client, homeMode, err := home.KVClientFromContext(ctx)
	if err != nil || !homeMode {
		return nil, homeMode, err
	}
	operations, ok := client.(HomeKVClient)
	if !ok {
		return nil, true, fmt.Errorf("home kv: request client does not implement common KV operations")
	}
	return operations, true, nil
}
