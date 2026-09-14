# Codex Live

`live` 包提供 Codex Live HTTP bootstrap、sideband WebSocket 和可选 WebRTC media relay

## 接口

| 方法   | 路径                          | 用途                       |
|--------|-------------------------------|----------------------------|
| `POST` | `/v1/live`                    | 创建 Live 会话             |
| `GET`  | `/v1/live/:call_id`           | 连接 Live sideband         |
| `POST` | `/v1/realtime/calls`          | 创建兼容 Realtime 会话     |
| `GET`  | `/v1/realtime/calls/:call_id` | 连接兼容 Realtime sideband |
| `GET`  | `/v1/realtime?call_id=...`    | 通过查询参数连接 sideband  |

这些接口注册在现有 `/v1` 认证组中，继承 TLS 认证策略、API key 校验、限流和 `Cache-Control: no-store`

## Bootstrap

Bootstrap 接受 JSON、`multipart/form-data` 和 SDP content type；multipart 请求必须包含 `sdp`，可选 `session` 必须是合法
JSON；请求体和上游响应体上限均为 16 MiB

凭据选择仅接受 Codex OAuth；普通模式记录选中的 auth ID；Home 模式保留 `HomeDispatchSelection`，sideband 和 media
生命周期复用同一执行范围

下游只向上游转发以下协议 header：

- `OpenAI-Alpha`
- `X-Session-Id`
- `Session-Id`
- `Thread-Id`
- `Originator`
- `X-Oai-Attestation`

`Authorization`、Cookie、Host 和其他非协议 header 不会转发；`X-Oai-Attestation` 的日志副本固定为 `[REDACTED]`；下游响应只复制
`Content-Type` 和 `Location`

## Sideband

Sideband 先连接上游 WebSocket，再升级下游连接；文本帧、二进制帧、Ping、Pong 和关闭状态在两端之间转发

Sideband 的显式代理优先级为 credential proxy、当前全局 proxy；credential `direct` 或 `none` 可覆盖全局
proxy；两者都为空时继续读取环境代理，支持 HTTP (S)、SOCKS5 和 SOCKS5H

## WebRTC media relay

Media relay 默认关闭，可通过以下配置开启：

```yaml
codex:
  live-media-relay:
    enabled: true
    max-sessions: 32
    disable-private-remote-ips: false
    public-ip: ""
    udp-port-min: 0
    udp-port-max: 0
    ice-servers: []
```

开启后，CPA 创建两个 `PeerConnection`：

```text
Codex Desktop
    │
    ▼
downstream PeerConnection
    │
    ├─ Opus RTP ──► upstream PeerConnection ──► OpenAI
    └─ oai-events DataChannel ◄──────────────► OpenAI
```

Media relay 固定使用 Opus payload type 111，48 kHz、2 channels；RTP 转发前清除 header extension；`oai-events` 每方向使用 64
条有界队列，单消息上限 256 KiB，发送缓冲上限 1 MiB

`max-sessions` 在全部热更新 generation 之间共享；降低上限或关闭 relay 不会中断现有 session，只影响新 session

`disable-private-remote-ips` 只过滤 Desktop-facing remote candidate；默认 `false`，适用于本机或局域网 Codex
Desktop；互联网部署可显式设为 `true`

WebRTC media 使用同一套 credential proxy 优先级，但空配置不会读取环境代理；空配置、`direct` 或 `none` 保持 direct
ICE/STUN/TURN；NAT 部署可配置 `public-ip` 和固定 UDP port range；固定范围至少需要每个并发 session 两个端口

配置 HTTP (S)、SOCKS5 或 SOCKS5H proxy 后，仅 OpenAI-facing leg 改用 authenticated ICE-TCP；Desktop-facing leg 保持
direct，配置的 ICE servers 仍只用于 direct PeerConnection

Proxy 模式会检查 OpenAI answer 中最多 64 个 candidate，只接受 public literal IP、RTP component 1、TCP host、passive、端口
443，并最多创建 16 个 loopback tunnel；UDP、relay、active 和不适用 candidate 会从改写后的 answer 中移除

每个 tunnel 最多同时验证 4 个连接；首个 ICE-TCP frame 必须在 10 秒内完成 STUN Binding Request 的 `USERNAME`、short-term
`MESSAGE-INTEGRITY` 和 `FINGERPRINT` 校验；认证成功后清除 read deadline，并且只允许一个连接 claim tunnel

Tunnel 只通过所选 proxy 连接 answer 中经过校验的固定 public `IP:443`；proxy 建连、TLS handshake、CONNECT 等待和 session
shutdown 都受 context 取消控制；proxy 失败时关闭 session，不使用 direct fallback

Remote media forwarding 首次开始时只记录一条结构化日志；direct 模式在 OpenAI-facing PeerConnection 进入 connected
后记录，proxy 模式在已认证 ICE frame 成功写入固定 target 后记录

日志字段包含安全的 credential 显示名、`auth_index`、`media_session_id`、`call_id`、connection、transport、peer 和
state；credential 优先使用 label，其次使用文件 basename，最后使用 opaque auth index；proxy URL、用户名、密码、target IP、SDP 和
ICE credential 不进入日志

旧字段 `allow-private-remote-ips` 仍可读取，并按反值迁移；新旧字段同时存在时配置加载失败

TURN `username` 和 `credential` 不进入 Management JSON、YAML 响应或 watcher diff 日志

## 生命周期

Session store 使用 generation token 隔离 replacement、旧 timer 和旧 claimant；replacement、expiry、media failure、sideband
close、Home drain 和 server stop 统一关闭 WebSocket、media 和 Home selection

Server shutdown 会主动结束 hijacked sideband WebSocket 和 Pion 资源；资源关闭在 store mutex 外执行，关闭错误由
`Server.Stop` 汇总返回

## 文件

- `live.go`：bootstrap、OAuth 选择、SDP 转换和 media factory 热更新
- `media.go`：Pion PeerConnection、Opus RTP、DataChannel、proxy mode 和 shared limiter
- `tcp_proxy.go`：candidate 审查、SDP 改写、STUN 认证和 ICE-TCP tunnel 生命周期
- `sideband.go`：session store、sideband WebSocket、代理和资源生命周期
- `live_test.go`：bootstrap、Home、media 接入和错误生命周期测试
- `media_test.go`：本地 PeerConnection 的 RTP、DataChannel、proxy mode、限流和日志测试
- `tcp_proxy_test.go`：candidate 安全边界、STUN 认证、timeout、single claim 和 tunnel 测试
- `sideband_test.go`：sideband、session、代理和关闭行为测试
- `DESIGN.md`：状态转换、网络边界和关闭顺序
