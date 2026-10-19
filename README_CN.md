# CLI 代理 API

[English](README.md) | 中文

CLIProxyAPI 为 CLI 工具和 SDK 提供 OpenAI、Gemini、Claude 与 Codex 兼容 API

支持 OAuth 与 API Key 提供商、多账户路由、流式响应、工具调用、多模态输入、可配置的上游转发，以及包含当前 DeepSeek V4.1 模型标识的内置模型目录

## 赞助商

[![bigmodel.cn](https://assets.router-for.me/chinese-5.png)](https://www.bigmodel.cn/claude-code?ic=RRVJPB5SII)

本项目由 Z智谱 提供赞助, 他们通过 GLM CODING PLAN 对本项目提供技术支持。

GLM CODING PLAN 是专为AI编码打造的订阅套餐，每月最低仅需20元，即可在十余款主流AI编码工具如 Claude Code、Cline、Roo Code
中畅享智谱旗舰模型GLM-4.7（受限于算力，目前仅限Pro用户开放），为开发者提供顶尖的编码体验。

智谱AI为本产品提供了特别优惠，使用以下链接购买可以享受九折优惠：https://www.bigmodel.cn/claude-code?ic=RRVJPB5SII

---

<table>
<tbody>
<tr>
<td style="width:180px"><a href="https://www.packyapi.com/register?aff=cliproxyapi"><img src="./assets/packycode.png" alt="PackyCode" style="width:150px"></a></td>
<td>感谢 PackyCode 对本项目的赞助！PackyCode 是一家可靠高效的 API 中转服务商，提供 Claude Code、Codex、Gemini 等多种服务的中转。PackyCode 为本软件用户提供了特别优惠：使用<a href="https://www.packyapi.com/register?aff=cliproxyapi">此链接</a>注册，并在充值时输入 "cliproxyapi" 优惠码即可享受九折优惠。</td>
</tr>
<tr>
<td style="width:180px"><a href="https://www.aicodemirror.com/register?invitecode=TJNAIF"><img src="./assets/aicodemirror.png" alt="AICodeMirror" style="width:150px"></a></td>
<td>感谢 AICodeMirror 赞助了本项目！AICodeMirror 提供 Claude Code / Codex / Gemini CLI 官方高稳定中转服务，支持企业级高并发、极速开票、7×24 专属技术支持。 Claude Code / Codex / Gemini 官方渠道低至 3.8 / 0.2 / 0.9 折，充值更有折上折！AICodeMirror 为 CLIProxyAPI 的用户提供了特别福利，通过<a href="https://www.aicodemirror.com/register?invitecode=TJNAIF">此链接</a>注册的用户，可享受首充8折，企业客户最高可享 7.5 折！</td>
</tr>
</tbody>
</table>

## 核心能力

- 提供 OpenAI、Gemini、Claude 与 Codex 兼容 API
- 支持已接入提供商的 OAuth 与 API Key 认证，包括 Meta Muse 账户和 Meta Model API Key
- 支持 Kimi.com、Kimi.ai 设备授权，以及通过 `--devin-login` 登录 Devin 账户
- 支持多账户路由、轮询选择和提供商前缀
- 支持流式与非流式响应、函数调用、工具和多模态输入
- 返回动态 OpenAI 模型元数据，并提供兼容的 Responses 端点
- 内置当前 DeepSeek V4.1 模型目录项：`deepseek-flash` 与 `deepseek-v4-pro`
- 通过配置接入 OpenAI 兼容上游
- 提供管理 API 和内置控制面板
- 提供可嵌入代理的 Go SDK

## 快速开始

完整用户手册位于 [help.router-for.me](https://help.router-for.me/cn/)

### Docker Compose

首次启动前创建专用配置目录：

```bash
mkdir -p config
cp config.example.yaml config/config.yaml
docker compose up -d
```

Compose 会把 `./config` 挂载到 `/CLIProxyAPI/config`，管理接口可以原子替换 `config.yaml`，并保留 `config.yaml.last-good`

已有根目录 `config.yaml` 的部署可以把文件迁移到 `config/config.yaml`，也可以暂时设置 `CLI_PROXY_CONFIG_DIR=.`；Compose
文件定义默认镜像，使用其他兼容镜像时设置 `CLI_PROXY_IMAGE`

将服务开放到本机之外前，先阅读[生产部署指南](docs/production-deployment_CN.md)

## 文档

- [用户手册](https://help.router-for.me/cn/)
- [生产部署：Nginx、Cloudflare、超时与排查](docs/production-deployment_CN.md)
- [管理 API](https://help.router-for.me/cn/management/api)
- [使用量 SQLite 容量、保留、备份与迁移](docs/usage-sqlite-capacity_CN.md)
- [Go SDK 使用](docs/sdk-usage_CN.md)
- [Go SDK 高级执行器与翻译器](docs/sdk-advanced_CN.md)
- [Go SDK 认证](docs/sdk-access_CN.md)
- [Go SDK 凭据加载与更新](docs/sdk-watcher_CN.md)
- 自定义 Provider 示例：[`examples/custom-provider`](examples/custom-provider)

## 开发检查

根 module 要求 Go 1.27.2；按 `go.mod` 安装依赖，并在提交 Pull Request 前执行基础检查：

```bash
go mod verify
go mod tidy -diff
go test -count=1 ./...
go vet ./...
go build ./cmd/server
```

发布候选验证还会执行 race detector、Staticcheck v0.8.2、govulncheck v1.7.0，以及 Linux AMD64 和 ARM64 的 `CGO_ENABLED=0` 构建

## 版本与发布行为

本地源码构建和 Docker 构建通过 `git describe --tags --always --dirty` 确定显示版本。正式构建通过 linker flags
写入发布版本、短 commit SHA 和 UTC 构建时间；没有发布元数据时，服务分别显示 `dev`、`none` 和 `unknown`

本地增强版本使用 `v<upstream>-aug.N` tag；基于新上游版本的首个本地版本从 `aug.1` 开始。所有 tag 都会触发
GoReleaser，`v*` tag 还会触发多架构 Docker 构建。发布资产前，`CHANGELOG.md` 必须存在与 tag 匹配的条目

## 贡献

欢迎贡献，请保持变更范围清楚，执行相关检查，并在 Pull Request 中说明行为变化与验证结果

## 许可证

CLIProxyAPI 使用 [MIT License](LICENSE)

## 社区

QQ 群：188637136

Telegram 群：https://t.me/CLIProxyAPI
