# Codex Live 设计

## 目标

该模块将 Codex Live bootstrap、sideband 和可选 WebRTC media relay 接入 CPA 现有认证、动态配置与可观测性体系

设计约束：

1. Bootstrap 只选择 Codex OAuth credential
2. Bootstrap、sideband 和 media 使用同一 credential 与 Home 执行范围
3. 配置热更新只影响新 media session，不中断现有 PeerConnection
4. 所有 media factory generation 共享同一并发计数
5. replacement、过期、失败和 shutdown 不得误删新 generation 或重复释放资源
6. 协议 header、TURN secret、SDP 和 candidate 不进入非必要日志或 Management 响应
7. OpenAI-facing proxy 模式只连接经过 candidate 审查的固定 public `IP:443`，任何 proxy 错误都不回退到 direct

## 调用关系

```text
POST /v1/live 或 /v1/realtime/calls
        │
        ▼
读取并归一化请求，限制为 16 MiB
        │
        ▼
选择 Codex OAuth
  ├─ 普通模式：记录 auth ID
  └─ Home 模式：保留 HomeDispatchSelection
        │
        ├─ media relay 关闭：保持原始 SDP bootstrap
        │
        └─ media relay 开启
              │
              ├─ downstream PeerConnection 接受 Desktop offer
              └─ upstream PeerConnection 生成 OpenAI offer
        │
        ▼
调用 Codex realtime calls
        │
        ├─ media relay 关闭：透传成功响应
        │
        └─ media relay 开启
              │
              ├─ direct 模式：upstream PeerConnection 接受 OpenAI answer
              ├─ proxy 模式：审查并改写 OpenAI TCP candidate，创建 authenticated ICE-TCP tunnel
              └─ downstream PeerConnection 生成 Desktop answer
        │
        ▼
从 Location 提取 call_id，写入 generation-safe session store
        │
        ▼
GET sideband route
        │
        ▼
claim session，恢复 pinned credential 或 retained Home selection
        │
        ▼
连接上游 WebSocket，再升级下游
        │
        ▼
双向转发 sideband 数据帧和控制帧
```

## 动态配置

`Handler` 保留 `ConfigProvider`，每次 HTTP 和 sideband 请求读取 server 当前配置；media factory 由 `UpdateConfig` 管理

`UpdateConfig` 比较 `Codex.LiveMediaRelay`：

- 配置未变化时保留原 factory
- 配置变化时创建新 factory，并原子替换当前指针和错误状态
- 所有 factory 使用 handler 生命周期内唯一的 `mediaSessionLimiter`
- 关闭 relay 时当前 factory 设为 nil，现有 session继续运行
- 降低 `max-sessions` 时不结束 active session，新 session 在 active 数量低于新上限前被拒绝

Media factory 创建错误写内部日志，后续新请求返回固定 unavailable 错误

## SDP 转换

Multipart 请求先转换为 JSON；media relay 开启后，`callRequestSDP` 从 JSON、`application/sdp` 或 `text/plain` 提取 Desktop
offer

`replaceCallRequestSDP` 只替换 `sdp`，保留 JSON 中 `session`、model 和其他字段；原始 SDP body 会包装成 Codex backend 接受的
JSON

成功响应先从 `Location` 提取合法 `call_id` 并写入 media session，再通过 `callResponseSDP` 读取 SDP 或 JSON answer；这样
direct PeerConnection connected 或 TCP tunnel forwarding 日志在首次触发时已经包含 call ID；OpenAI answer 应用到 upstream
PeerConnection 后，Desktop answer 以 `application/sdp` 返回

Media session 只有在合法 `call_id` 写入 store 后才 retained；此前任何错误都使用 `request_not_retained` 关闭

## WebRTC media

### PeerConnection

Downstream leg 面向 Codex Desktop，upstream leg 面向 OpenAI；两端分别注册 Opus payload type 111，48 kHz、2 channels、
`minptime=10;useinbandfec=1`

`disable-private-remote-ips=true` 时，只为 downstream API 安装 remote IP filter；upstream API 不应用该策略

Direct 模式下，STUN/TURN、NAT 1:1 public IP 和固定 UDP range由两个 API generation 共享配置

Proxy 模式下，downstream API 保持 direct 配置；upstream API 只生成 loopback TCP candidate，不使用 ICE servers、NAT public IP
或固定 UDP range

### ICE-TCP proxy

Bootstrap 完成 OAuth 或 Home credential 选择后，media 和 sideband 都使用 credential proxy 优先于当前全局 proxy
的规则；credential `direct` 或 `none` 可覆盖全局 proxy；media 的空配置不读取环境代理

HTTP (S)、SOCKS5 或 SOCKS5H proxy 会令 OpenAI-facing leg 使用单独的 upstream API；local offer 保存在 session 中，OpenAI
answer 返回后再执行以下处理：

1. 解析 local offer 与 remote answer 的 bundled ICE ufrag 和 password
2. 检查最多 64 个 remote candidate
3. 只保留 public literal IP、RTP component 1、TCP host、passive、端口 443 的 candidate
4. 最多为 16 个目标创建同地址族的 loopback listener
5. 将 answer 中的 public target 改写为 loopback listener，并在 `SetRemoteDescription` 前安装到 session

原 public `IP:443` 只保存在 tunnel 内部；private、CGNAT、loopback、link-local、multicast、文档、benchmark、保留、translation 和
mapped address 都不能成为 proxy target

每个 tunnel 最多并发验证 4 个未认证连接；第一帧使用 ICE-TCP 两字节长度，STUN payload 上限为 4096 bytes，并且必须是 Binding
Request；`USERNAME` 必须匹配 `remoteUfrag:localUfrag`，short-term `MESSAGE-INTEGRITY` 使用 remote ICE password，
`FINGERPRINT` 必须有效

第一帧 read deadline 为 10 秒；认证成功后清除 deadline；认证失败或 timeout 会关闭 connection 并释放 validation slot；认证成功后
tunnel 只允许 claim 一次，随后关闭 listener

Tunnel 通过 `proxy.ContextDialer` 连接固定 target，先转发已认证的首帧，再执行双向 `io.Copy`；HTTP CONNECT 的建连、HTTPS
handshake 和 response wait 都受同一 context 控制；proxy 失败时关闭 connection，不使用 direct fallback

### RTP 与 RTCP

Desktop 与 OpenAI 的 Opus RTP 双向转发；写入目标 track 前清除 `Extension`、`ExtensionProfile` 和 `Extensions`

每个 `RTPSender` 有独立 RTCP reader；PeerConnection 关闭后 reader结束，关闭错误只记录非预期状态

### DataChannel

只接受 label 为 `oai-events` 的 DataChannel；upstream channel由 relay主动创建；其他 downstream channel 关闭

每方向有一个 `dataChannelPipe`：

- queue size 64
- 单消息上限 256 KiB
- buffered amount 上限 1 MiB
- low threshold 512 KiB
- 入队前复制 payload，保留 text/binary 类型

DataChannel error、意外 close 或发送失败会触发 media session failure

## Session store

Session entry 包含：

- `call_id`
- auth ID
- model
- 可选 retained `HomeDispatchSelection`
- media session
- sideband WebSocket resources
- generation token、claimed 状态和 expiry timer

状态转换：

```text
put -> claim -> complete
          │
          └-> release -> claim 或 expire
```

每次 `put` 分配新 token；旧 timer、旧 claimant、media close callback 的 `complete` 只有 token 匹配时才生效

`liveSessionResources` 收集 upstream/downstream WebSocket closer；资源已经关闭后再加入的 closer 会立即执行

Store 不在 mutex 内关闭 WebSocket、PeerConnection 或 Home selection

## Sideband

Sideband 在 claim 前校验 WebSocket Upgrade 和 `call_id`；普通 session 使用 pinned auth metadata，Home session直接复用
retained selection

连接顺序：

1. 准备 OAuth header
2. 按当前配置建立上游 WebSocket
3. 绑定上游连接到 Home selection
4. 使用协商结果升级下游 WebSocket
5. 绑定下游连接并加入 session resources
6. 双向转发 text、binary、Ping、Pong 和 close

转发前错误 release session，允许重试；转发开始后 complete session

## 关闭顺序

`endLiveSession` 按以下顺序执行：

1. 关闭 sideband WebSocket resources
2. 关闭 media DataChannel bridge
3. 取消 ICE-TCP dial context，并关闭 candidate listeners 和 tunnel connections
4. 关闭两个 PeerConnection
5. 释放 shared limiter slot
6. 结束 Home selection

Media failure callback、sideband complete、expiry、replacement 和 server stop 都使用同一入口；各资源内部使用一次性关闭语义

`Handler.Close` 调用 `sessionStore.closeAll("server_stopped")`；store 先删除 entry 和停止 timer，再在锁外关闭资源，并将
WebSocket 与 media 关闭错误返回给 `Server.Stop` 汇总

## 安全与可观测性

Bootstrap 继续记录现有 HTTP request/response timeline；sideband 继续记录 WebSocket request、handshake、dial error 和 relay
error；响应设置 `X-CPA-TRACE-ID`

日志不写 token、attestation 原值、SDP、candidate、TURN username/credential、proxy URL、proxy authentication 或 candidate
target IP；media 配置日志只写 enabled、有效上限、private policy、public IP、UDP range 和 ICE server 数量

Remote media forwarding 日志通过 session 内的 `sync.Once` 保证最多一条：

- direct 模式在 OpenAI-facing PeerConnection 进入 connected 后触发
- proxy 模式在 authenticated ICE frame 成功写入固定 target 后触发，proxy dial 或首帧写入失败不触发
- connection 只记录 `direct` 或 `via <scheme> proxy`
- credential 只记录 label、文件 basename 或 opaque auth index，不记录 auth ID 和文件路径
- `LogFormatter` 对 credential、connection、transport、media/call ID、peer、state 和 reason 使用 quoted string，换行不会形成伪造日志行

Management JSON 排除 TURN username/credential；YAML placeholder 仅将 `ice-servers` 项内的 username 和 credential 视为
secret；watcher diff 只记录 ICE server 条目数
