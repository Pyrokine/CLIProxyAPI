# 生产部署

[English](production-deployment.md) | 中文

## 适用范围与状态

本文提供一个公开、与具体 release 解耦的模板，用于在 Nginx 和可选的 Cloudflare 后运行 CLIProxyAPI；内容说明代理行为和验证方法，不保证每个
release 都具有相同的应用请求超时或流式 heartbeat 行为

部署前核对当前版本的 `config.example.yaml`、release notes 和客户端行为，改变生产流量前验证完整链路

## 参考拓扑

```text
Client
  -> Cloudflare (optional)
  -> Nginx and TLS termination
  -> CLIProxyAPI at 127.0.0.1:8317
  -> Upstream provider
```

客户端、Cloudflare、Nginx、CLIProxyAPI 和上游提供商都可能产生超时；给每层记录关联标识和 timing 后，再判断状态码来自哪一层

## 前置条件

- 公网 TLS 使用客户端信任的证书
- Nginx 使用 Cloudflare Origin CA 证书时，保持 DNS 记录经过代理；直接客户端通常不信任 Origin CA 证书，切换 DNS-only 前需要为
  Origin 配置公网可信证书
- Nginx 作为公网入口时，把 CLIProxyAPI 绑定到 loopback 或私有接口
- 按所用 release 的 `config.example.yaml` 配置可信代理范围
- 保持 API 认证启用，远程管理没有独立访问策略时继续禁用
- 根据已测流量、可用内存、连接上限和允许的最长操作选择正文上限与 timeout
- 修改 Nginx 或 Cloudflare 前保存当前生效配置
- 保留不向公网暴露认证 API 或管理路由的直接健康检查

## Nginx 精确路径模板

把以下 location 放入现有 TLS `server` 块；timeout 和正文大小只是示例值，需要根据部署环境调整，且不能超过应用限制

```nginx
location = /v1/messages {
    proxy_pass http://127.0.0.1:8317;
    proxy_http_version 1.1;
    proxy_set_header Connection "";
    proxy_set_header Host $host;
    proxy_set_header X-Real-IP $remote_addr;
    proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;
    proxy_set_header X-Forwarded-Proto $scheme;

    client_max_body_size 32m;
    client_body_timeout 120s;
    proxy_connect_timeout 10s;
    proxy_send_timeout 120s;
    proxy_read_timeout 300s;
    send_timeout 120s;

    proxy_request_buffering off;
    proxy_buffering off;
    proxy_cache off;
    gzip off;
    add_header X-Accel-Buffering no always;
}

location = /v1/responses {
    proxy_pass http://127.0.0.1:8317;
    proxy_http_version 1.1;
    proxy_set_header Connection "";
    proxy_set_header Host $host;
    proxy_set_header X-Real-IP $remote_addr;
    proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;
    proxy_set_header X-Forwarded-Proto $scheme;

    client_max_body_size 32m;
    client_body_timeout 120s;
    proxy_connect_timeout 10s;
    proxy_send_timeout 120s;
    proxy_read_timeout 300s;
    send_timeout 120s;

    proxy_request_buffering off;
    proxy_buffering off;
    proxy_cache off;
    gzip off;
    add_header X-Accel-Buffering no always;
}
```

使用精确 location，避免流式策略影响管理路由或无关端点；客户端使用其他已记录别名时，验证后再逐项加入对应路径

CLIProxyAPI 的 SSE handler 会发送 `Content-Type: text/event-stream`、`Cache-Control: no-cache, no-transform` 和
`X-Accel-Buffering: no`；HTTP/1.x 响应还会发送 `Connection: keep-alive`，HTTP/2 响应不发送这个逐连接 header。这些应用层
header 不能替代上面的 Origin 配置；公网路由经过的每层 Nginx 都要保持 `proxy_buffering off`、`proxy_cache off`、`gzip off`，
并把 `proxy_read_timeout` 设为大于预期响应字节间隔的值

### Timing 日志

在 Nginx `http` 上下文定义 `log_format`，然后在 API 的 `server` 或精确 location 中启用日志：

```nginx
log_format cli_proxy_timing escape=json
    '{"time":"$time_iso8601",'
    '"request_id":"$request_id",'
    '"cf_ray":"$http_cf_ray",'
    '"method":"$request_method",'
    '"uri":"$uri",'
    '"status":$status,'
    '"request_length":$request_length,'
    '"body_bytes_sent":$body_bytes_sent,'
    '"request_time":$request_time,'
    '"upstream_status":"$upstream_status",'
    '"upstream_connect_time":"$upstream_connect_time",'
    '"upstream_header_time":"$upstream_header_time",'
    '"upstream_response_time":"$upstream_response_time"}';

access_log /var/log/nginx/cli-proxy-api-timing.log cli_proxy_timing;
```

示例只记录不含 query 的路径和 timing，不要加入请求体、`Authorization`、Cookie、API Key 或 query string

## 缓冲与超时语义

| 层或指令                       | 控制对象                                        | 重要边界                                                     |
|--------------------------------|-------------------------------------------------|--------------------------------------------------------------|
| `client_body_timeout`          | 从客户端连续读取请求体之间的空闲时间            | 超时后 Nginx 返回 408，不是完整上传总时长                    |
| `proxy_send_timeout`           | Nginx 向 CLIProxyAPI 连续写入请求之间的空闲时间 | 不是请求总时长                                               |
| `proxy_read_timeout`           | 从 CLIProxyAPI 连续读取响应之间的空闲时间       | 流式数据或 heartbeat 必须在该间隔内到达                      |
| `send_timeout`                 | Nginx 向客户端连续写入响应之间的空闲时间        | 慢客户端或已断开的客户端可能结束下游链路                     |
| Cloudflare Proxy Read Timeout  | 等待 Origin 响应进展的时间                      | 文档默认值为 125 秒，只有符合条件的 Enterprise zone 可以调整 |
| Cloudflare Proxy Write Timeout | Cloudflare 向 Origin 写入请求的时间             | 文档上限为 30 秒，不能调整                                   |

`proxy_request_buffering off` 让 Nginx 在收到请求体时继续向 CLIProxyAPI 传递，`proxy_buffering off` 让 Nginx
直接向客户端传递响应块，不等待完整响应；`proxy_cache off` 阻止代理缓存处理这些 API 响应，`gzip off` 避免压缩层先聚合较小的
heartbeat frame 再向下游发送

延长代理 timeout 不能替代正文大小限制、认证、连接或速率限制、容量规划和客户端取消处理

## Cloudflare Configuration Rule

Configuration Rule 只有在 DNS 记录经过 Cloudflare 代理时才会生效

创建一个精确匹配 host、method 和 path 的 Configuration Rule，把示例 host 替换为公网 API host：

```text
(http.host eq "api.example.com" and http.request.method eq "POST" and (http.request.uri.path eq "/v1/messages" or http.request.uri.path eq "/v1/responses"))
```

只设置一个动作：

```text
Request Body Buffering = Full
```

不要在同一规则中设置 Response Body Buffering，也不要附加 cache、Origin、route 或无关 timeout 变更

Cloudflare 文档将 `Full` 定义为完整缓冲请求体后再向 Origin 发送，这可以把慢客户端上传与 Cloudflare 到 Nginx 的请求阶段分开；Nginx
在下一跳仍可使用 `proxy_request_buffering off`，两项配置不冲突

`Full` 不会提高 Cloudflare 的 125 秒 Proxy Read Timeout 或 30 秒 Proxy Write Timeout，也不保证所有 524 都会消失；`None`
会在不检查正文的情况下直接向 Origin 传输，可能降低 WAF 或其他依赖正文检查功能的效果；完整缓冲也不代表安全产品会检查每个字节，检查范围仍由套餐和产品限制决定

规则使用不含 query string 的 URI path；上述表达式有意排除 `/v1/messages/`、子路径、其他 host、GET 请求和端点别名

## 验证

### 配置

1. 每次 reload 前执行 `nginx -t`
2. graceful reload 后确认旧 worker 已退出
3. 确认两个精确路径都能通过公网 host 到达 CLIProxyAPI
4. 确认管理路由和其他 host 没有继承流式 location
5. 在 Cloudflare 中确认规则只匹配目标 proxied host 和两个 POST 路径，唯一动作是 Request Body Buffering `Full`

### 慢请求体

只使用不含凭证和用户内容的生成数据；以下文件略大于 1 MiB，以 8 KiB/s 上传时每条请求超过两分钟：

```bash
python3 - <<'PY'
import json

padding = "x" * (1024 * 1024)
with open("/tmp/cpa-messages.json", "w", encoding="utf-8") as output:
    json.dump({
        "model": "test-model",
        "stream": True,
        "max_tokens": 1,
        "messages": [{"role": "user", "content": padding}],
    }, output)
with open("/tmp/cpa-responses.json", "w", encoding="utf-8") as output:
    json.dump({
        "model": "test-model",
        "stream": True,
        "input": padding,
    }, output)
PY

for endpoint in messages responses; do
    curl --http1.1 --limit-rate 8k --max-time 240 \
        -H 'Content-Type: application/json' \
        --data-binary "@/tmp/cpa-${endpoint}.json" \
        -o /dev/null \
        -w "endpoint=${endpoint} status=%{http_code} total=%{time_total}s upload=%{size_upload}\n" \
        "https://api.example.com/v1/${endpoint}"
done
```

不提供 API Key 时，请求体接收完成后返回 401 可以作为 Origin 结果；Cloudflare 在请求到达 Origin 前阻断时，该检查不能证明
Origin 行为；使用响应中的 `CF-Ray: <CF-RAY>` 关联 timing 日志，对比客户端上传时间、Nginx `request_time`、upstream header
time 和响应字节数

一条合成请求成功只证明对应路由和测试条件，不代表所有真实客户端、请求大小、协议或提供商路径都已正常

还要使用生产客户端的协议和调用栈重复测试；上述命令固定为 HTTP/1.1，只提供一个可重复基线

### 流式响应

使用本地延迟 stub 或专用测试提供商，不要为了制造延迟调用付费生产请求；针对每个支持的 release 和客户端验证：

- Origin 和公网 host 返回的响应状态、`Content-Type: text/event-stream`、`Cache-Control: no-cache, no-transform` 和
  `X-Accel-Buffering: no`
- HTTP/2 响应中没有 `Connection` header
- 首字节和首个语义事件时间
- 上游 setup 完成前、首个语义事件前和普通响应空档期间的 heartbeat 到达时间
- 多个响应事件和唯一的终止事件
- 终止事件后及时结束连接，包括上游 transport 没有正常关闭的情况
- 非流式 JSON 响应
- 客户端取消和对应 Nginx 状态

## 故障排查

| 状态 | 在该拓扑中的含义                                                 | 检查内容                                                                                  |
|------|------------------------------------------------------------------|-------------------------------------------------------------------------------------------|
| 408  | 某一层停止等待请求头或正文数据                                   | 先确定生成状态码的层，再比较客户端上传空档、Nginx `client_body_timeout` 和已部署应用版本  |
| 499  | Nginx 观察到客户端关闭连接                                       | 先检查客户端 timeout 或 retry，499 通常是此前延迟的结果                                   |
| 504  | 网关等待上游时超时                                               | 对比 Nginx proxy timing、CLIProxyAPI 和提供商耗时                                         |
| 524  | Cloudflare 已连接 Origin，但 Origin 读写阶段超过 Cloudflare 限制 | 使用 Cloudflare ray 关联 Nginx timing；Full 缓冲只影响一种请求体路径，不覆盖所有 524 原因 |

客户端发生 retry 时，使用脱敏 request identifier 归并各次请求，对比请求大小、开始时间、状态、响应字节和 retry
count，不要直接把连续请求视为不同用户操作

## 安全与脱敏

- 保持 API 和管理认证启用
- 只信任已知代理地址提供的转发头
- 按允许的 timeout 窗口配置连接和速率限制
- 保持有限的请求体上限，并为代理缓冲准备足够临时空间
- 不记录请求体、响应体、凭证、Cookie、query token 或完整 header map
- 发布诊断信息前删除公网 host、Origin IP、rule ID、Cloudflare ray、PID、本地路径、hash、时间戳、流量数量和会话标识
- 在 issue 和文档中使用 `api.example.com`、`<CF-RAY>` 和 `<REDACTED>` 等占位符

## 限制

- 模板只覆盖 `/v1/messages` 和 `/v1/responses`
- 其他端点别名需要单独添加精确规则
- 客户端、操作系统、企业代理、提供商和应用自身的 timeout 仍可结束请求
- Cloudflare 限制和功能可用性取决于当前套餐与产品文档
- 缓冲会改变请求 timing 和取消行为，需要分别测试流式与非流式客户端
- 本文不主张所有 408、499、504、524 或客户端 retry 具有同一个原因

## 回滚

1. 禁用单条 Cloudflare Configuration Rule，确认生效的请求体模式恢复到修改前状态
2. 恢复上一份 Nginx 配置，执行 `nginx -t` 后 graceful reload
3. 每次只回滚一层，再重复相同请求和 timing 检查
4. 不要把 loopback 上的 CLIProxyAPI listener 临时开放到公网绕过代理

## 参考资料

- [NGINX proxy module](https://nginx.org/en/docs/http/ngx_http_proxy_module.html)
- [NGINX core HTTP module](https://nginx.org/en/docs/http/ngx_http_core_module.html)
- [Cloudflare Configuration Rules settings](https://developers.cloudflare.com/rules/configuration-rules/settings/)
- [Cloudflare connection limits](https://developers.cloudflare.com/fundamentals/reference/connection-limits/)
- [Cloudflare Error 524](https://developers.cloudflare.com/support/troubleshooting/http-status-codes/cloudflare-5xx-errors/error-524/)
