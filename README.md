# CLI Proxy API

English | [中文](README_CN.md)

CLIProxyAPI exposes OpenAI, Gemini, Claude, and Codex-compatible APIs for CLI tools and SDKs

It supports OAuth and API-key providers, multiple accounts, streaming responses, tool calls, text and image input,
configurable upstream routing, and a built-in model catalog with current DeepSeek V4.1 model identifiers

## Sponsor

[![z.ai](https://assets.router-for.me/english-5.png)](https://z.ai/subscribe?ic=8JVLJQFSKB)

This project is sponsored by Z.ai, supporting us with their GLM CODING PLAN.

GLM CODING PLAN is a subscription service designed for AI coding, starting at just $10/month. It provides access to
their flagship GLM-4.7 & （GLM-5 Only Available for Pro Users）model across 10+ popular AI coding tools (Claude Code,
Cline, Roo Code, etc.), offering developers top-tier, fast, and stable coding experiences.

Get 10% OFF GLM CODING PLAN：https://z.ai/subscribe?ic=8JVLJQFSKB

---

<table>
<tbody>
<tr>
<td style="width:180px"><a href="https://www.packyapi.com/register?aff=cliproxyapi"><img src="./assets/packycode.png" alt="PackyCode" style="width:150px"></a></td>
<td>Thanks to PackyCode for sponsoring this project! PackyCode is a reliable and efficient API relay service provider, offering relay services for Claude Code, Codex, Gemini, and more. PackyCode provides special discounts for our software users: register using <a href="https://www.packyapi.com/register?aff=cliproxyapi">this link</a> and enter the "cliproxyapi" promo code during recharge to get 10% off.</td>
</tr>
<tr>
<td style="width:180px"><a href="https://www.aicodemirror.com/register?invitecode=TJNAIF"><img src="./assets/aicodemirror.png" alt="AICodeMirror" style="width:150px"></a></td>
<td>Thanks to AICodeMirror for sponsoring this project! AICodeMirror provides official high-stability relay services for Claude Code / Codex / Gemini CLI, with enterprise-grade concurrency, fast invoicing, and 24/7 dedicated technical support. Claude Code / Codex / Gemini official channels at 38% / 2% / 9% of original price, with extra discounts on top-ups! AICodeMirror offers special benefits for CLIProxyAPI users: register via <a href="https://www.aicodemirror.com/register?invitecode=TJNAIF">this link</a> to enjoy 20% off your first top-up, and enterprise customers can get up to 25% off!</td>
</tr>
</tbody>
</table>

## Highlights

- OpenAI, Gemini, Claude, and Codex-compatible API endpoints
- OAuth and API-key access for supported providers
- Multiple-account routing, round-robin selection, and configurable provider prefixes
- Streaming and non-streaming responses, function calls, tools, and text and image input
- Dynamic OpenAI model metadata and compatible Responses endpoints
- Current DeepSeek V4.1 catalog entries: `deepseek-flash` and `deepseek-v4-pro`
- Configurable OpenAI-compatible upstream providers
- Management API and bundled control panel support
- Reusable Go SDK for embedding the proxy

## Quick start

The full user guide is available at [help.router-for.me](https://help.router-for.me/)

### Docker Compose

Create a dedicated configuration directory before the first start:

```bash
mkdir -p config
cp config.example.yaml config/config.yaml
docker compose up -d
```

Compose mounts `./config` at `/CLIProxyAPI/config`, which allows management updates to replace `config.yaml` atomically
and retain `config.yaml.last-good`

Existing root-level `config.yaml` deployments can move the file to `config/config.yaml` or temporarily set
`CLI_PROXY_CONFIG_DIR=.`. The Compose file defines its default image; set `CLI_PROXY_IMAGE` when using another
compatible image

Before exposing the service outside localhost, read the [production deployment guide](docs/production-deployment.md)

## Documentation

- [User guide](https://help.router-for.me/)
- [Production deployment: Nginx, Cloudflare, timeouts, and diagnostics](docs/production-deployment.md)
- [Management API](https://help.router-for.me/management/api)
- [Usage SQLite capacity, retention, backup, and migration](docs/usage-sqlite-capacity.md)
- [Model price sources and request-time pricing receipts](internal/modelcatalog/PRICE_SOURCES.md)
- [Go SDK usage](docs/sdk-usage.md)
- [Go SDK advanced executors and translators](docs/sdk-advanced.md)
- [Go SDK access](docs/sdk-access.md)
- [Go SDK watcher](docs/sdk-watcher.md)
- Custom provider example: [`examples/custom-provider`](examples/custom-provider)

## Development

The root module targets Go 1.27.0. Install dependencies from `go.mod` and run the baseline checks before opening a pull
request:

```bash
go mod verify
go mod tidy -diff
go test -count=1 ./...
go vet ./...
go build ./cmd/server
```

Release-candidate validation also runs the race detector, Staticcheck v0.8.1, govulncheck v1.7.0, and `CGO_ENABLED=0`
builds for Linux AMD64 and ARM64

## Version and release behavior

Local source and Docker builds resolve the displayed version through `git describe --tags --always --dirty`. Release builds
embed the release version, short commit SHA, and UTC build date through linker flags. Without release metadata, the server
reports `dev`, `none`, and `unknown`

Local augmented releases use tags in the form `v<upstream>-aug.N`; the first local release based on a new upstream version
uses `aug.1`. Tags trigger the GoReleaser workflow, while `v*` tags also trigger the multi-architecture Docker workflow. A
matching entry in `CHANGELOG.md` is required before publishing release assets

## Contributing

Contributions are welcome. Keep changes focused, run the relevant checks, and open a pull request with the behavior and
verification described clearly

## License

CLIProxyAPI is licensed under the [MIT License](LICENSE)
