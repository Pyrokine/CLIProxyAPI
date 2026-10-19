# 插件示例

- [主机模型回调](host-model-callback/README.md)：从插件资源调用主机模型执行器
- [调度器](scheduler/README.md)：选择凭证或委托内置调度器
- [Codex 服务等级](codex-service-tier/README.md)：规范化发往 Codex 的请求
- [JS 处理器](jshandler/README.md)：通过脚本拦截请求和响应

## 主机 HTTP 回调取消

需要在 `host.http.do` 或 `host.http.do_stream` 返回响应或 `stream_id` 之前取消请求时：

1. 调用 `host.http.operation_open`，若 HTTP 请求携带 `host_callback_id`，此处使用同一个仍有效且属于调用插件实例的 ID
2. 调用 `host.http.do` 或 `host.http.do_stream`，传入返回的 `operation_id` 和相同的 `host_callback_id`
3. 从同一插件实例调用 `host.http.cancel` 并传入该 `operation_id`，已打开的主机 HTTP 数据流也会被移除

每项 operation 只能被使用一次，HTTP 数据流属于创建它的插件实例，未使用的 operation 需要取消，使用完的数据流通过 `host.http.stream_close` 关闭，打开 operation 不会设置新的单次调用期限，插件自行指定期限时需要安排取消调用，主机现有的 HTTP 超时及父 context 期限继续生效

## 执行器错误状态

动态库插件的 `executor.execute` 和 `executor.execute_stream` 发生上游调用失败时，需要在 RPC 错误信封中序列化错误，如果上游返回了 HTTP 状态码，将其写入 `error.http_status`，Go 插件可使用 `pluginabi.NewErrorEnvelope`：

```go
raw, err := pluginabi.NewErrorEnvelope("rate_limit", "upstream rate limit", http.StatusTooManyRequests)
```

主机将该字段解码为带 `StatusCode()` 的错误，缺少状态码时，错误到达 HTTP 处理器后默认使用 500，路由层也可能先尝试其他凭证，流式请求在打开数据流之前返回的错误可以设置 HTTP 状态码；响应一旦提交，后续流错误无法修改该状态，单独返回 Go `error` 不会把状态码传过动态库 ABI，必须写入 JSON 错误信封
