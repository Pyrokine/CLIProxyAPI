# Model catalog sources

Checked on 2026-09-23

## Catalog boundaries

The model catalog keeps these layers separate:

- Embedded provider inventory and Go fallback metadata
- Remote provider catalog and Codex client manifest
- User `models.json`, aliases, enabled state, custom prices, and tombstones
- Runtime credentials, exclusions, suspension, quota cooldown, API key policy, and account entitlement
- Lookup-only historical IDs and price-only historical records

A model in the embedded or management catalog is not requestable until an enabled runtime credential publishes that route. Price records do not create inventory entries. Unknown capabilities are not inferred from model names.

## Claude

- Models: <https://platform.claude.com/docs/en/about-claude/models/overview>
- Pricing: <https://platform.claude.com/docs/en/about-claude/pricing>
- Sonnet 5.5 migration: <https://platform.claude.com/docs/en/models/sonnet-5-5/whats-new-sonnet-5-5>
- Default additions: `claude-opus-5-5`, `claude-sonnet-5-5`
- Current adaptive models: `claude-fable-5-1`, `claude-opus-5-5`, `claude-sonnet-5-5`

Fable 5.1 and Opus 5.5 use always-on adaptive thinking. Sonnet 5.5 accepts adaptive thinking or `between_tools`, but rejects `thinking.type=disabled` and forced tool choices. For Sonnet 5.5, catalog `zero_allowed` preserves the `none` request intent; the Claude executor sends `between_tools` instead of `disabled`. Restricted models are not added to the general default inventory.

## OpenAI and Codex

- Public API models: <https://developers.openai.com/api/docs/models/all>
- Deprecations: <https://developers.openai.com/api/docs/deprecations>
- GPT-5.3-Codex: <https://developers.openai.com/api/docs/models/gpt-5.3-codex>
- Codex provider catalog: <https://raw.githubusercontent.com/router-for-me/models/refs/heads/main/models.json>
- Codex client manifest: <https://raw.githubusercontent.com/router-for-me/models/refs/heads/main/codex_client_models.json>
- Fallback catalog endpoints: <https://models.router-for.me/models.json> and <https://models.router-for.me/codex_client_models.json>

OpenAI public API availability does not establish Codex OAuth or CLI plan entitlement. The official `gpt-6.1-sol` API ID has a reference-price record but is not added to the embedded Codex plan inventory or client manifest without a reviewed Codex client template; a remote catalog may publish it for an eligible credential. `gpt-5.3-codex` remains available for public API compatibility but is not reintroduced into the Codex plan inventory. `gpt-5.3-codex-spark` is removed from the default Codex inventory and client manifest, with exact-ID historical lookup retained.

Current Codex plan placement is:

| Plan | Models |
|---|---|
| Free | `gpt-6-luna` |
| Team, Business, Go | `gpt-6-sol`, `gpt-6-luna` |
| Plus | `gpt-6-sol`, `gpt-6-luna` |
| Pro | `gpt-6-sol`, `gpt-6-luna` |

The Codex client manifest keeps its own request contract and version gates. Public API context limits are not copied into the Codex requestable route without client-manifest evidence.

## GPT Image 2.5

- Flare and Flare pricing: <https://developers.openai.com/api/docs/models/gpt-image-2.5-flare>
- Sunburst and Sunburst pricing: <https://developers.openai.com/api/docs/models/gpt-image-2.5-sunburst>
- Image generation guide: <https://developers.openai.com/api/docs/guides/image-generation>

The supported official IDs are:

- `gpt-image-2.5-flare`
- `gpt-image-2.5-flare-2026-09-08`
- `gpt-image-2.5-sunburst`
- `gpt-image-2.5-sunburst-2026-09-08`

CPA does not create a generic `gpt-image-2.5` alias. The four IDs are covered by Images generations and edits, multipart edits, Responses image generation, streaming events, usage extraction, and quality normalization.

All four IDs have `partial` reference-price records. The manifest stores the published text-input, cached-text-input, and image-output token rates. Image input has a separate rate that the current token schema cannot select by modality, so the catalog exposes the limitation instead of displaying a complete or zero price.

Existing `gpt-image-1.5` and `gpt-image-2` defaults now have the same explicit `partial` treatment. Their image-input and per-image conditions remain visible as limitations. GPT Image 1.5 stays available during its published deprecation window and is scheduled for API shutdown on 2026-12-01.

## Gemini API, AI Studio, and Vertex

- Gemini 3.1 Flash Image: <https://ai.google.dev/gemini-api/docs/models/gemini-3.1-flash-image>
- Gemini 3.1 Flash-Lite Image: <https://ai.google.dev/gemini-api/docs/models/gemini-3.1-flash-lite-image>
- Image generation: <https://ai.google.dev/gemini-api/docs/image-generation>
- Pricing: <https://ai.google.dev/gemini-api/docs/pricing>

Current stable image defaults for Gemini API, AI Studio, and Vertex are `gemini-3.1-flash-image` and `gemini-3.1-flash-lite-image`. Closed preview IDs are removed from default exposure. Gemini CLI OAuth inventory is maintained independently and is not expanded from Gemini API documentation alone.

`gemini-3.8-flash` and its thinking-level `high` alias now have explicit `partial` reference prices through 2026-12-31. Batch, context-cache storage, the 2027 rate change, and the absence of a separately published alias rate remain visible as limitations.

Specialized Live, TTS, Omni, and transcription endpoints are not represented as ordinary chat routes unless the executor implements the corresponding endpoint contract.

## DeepSeek

- Models: <https://api-docs.deepseek.com/api/list-models/>
- V4.1 Flash release: <https://www.deepseek.com/en/news/deepseek-v4-1-flash/>
- Anthropic-compatible API: <https://api-docs.deepseek.com/guides/anthropic_api/>
- Pricing: <https://api-docs.deepseek.com/quick_start/pricing/>
- Discovery: authenticated `GET https://api.deepseek.com/models`

Default inventory contains `deepseek-flash` and `deepseek-v4-pro`. `deepseek-v4-flash` and `deepseek-v4-flash-vision-exp` remain exact-ID legacy lookups and price-only historical records.

The Anthropic-compatible route is `https://api.deepseek.com/anthropic`. CPA uses the documented `x-api-key` credential header and omits `anthropic-version`, which DeepSeek ignores. Image content blocks, tools, `tool_use`, `tool_result`, and thinking enabled or disabled pass through the existing Claude-compatible executor. DeepSeek ignores `thinking.budget_tokens`, so the catalog does not declare a numeric thinking budget or invented effort range.

## xAI

- Grok 4.7: <https://docs.x.ai/developers/models/grok-4.7>
- Grok Imagine Image 2.0: <https://docs.x.ai/developers/models/grok-imagine-image-2.0>
- Imagine capability: <https://docs.x.ai/developers/model-capabilities/imagine>
- Cost tracking: <https://docs.x.ai/developers/cost-tracking>

Default inventory adds `grok-4.7` and `grok-imagine-image-2.0`. Image 2.0 preserves its canonical ID through generations and edits, accepts JSON and multipart edits, and remains subject to the same credential and API key policy checks as other xAI routes. Per-image pricing remains `unpriced` in the token-rate manifest.

Existing Grok Imagine image and video defaults also have explicit `unpriced` records. Their official billing units are images or video seconds with quality and resolution conditions, so the token manifest does not convert them into synthetic token rates.

## Kimi

- Kimi API models: <https://platform.kimi.ai/docs/models>
- Kimi Code subscription models: <https://www.kimi.com/code/docs/kimi-code/models.html>

The regular Kimi API and Kimi Code subscription use different exact IDs. Default Kimi API inventory contains `kimi-k3`, `kimi-k2.7-code`, `kimi-k2.7-code-highspeed`, and `kimi-k2.6`. The K2.7 Code variants use their separately published cache-hit, cache-miss input, and output prices. The Kimi Code selector `k3-256k` is not rewritten into a regular API canonical ID; `kimi-k3-256k` remains exact-ID historical lookup only. Unverified K2.8 API IDs and aliases are not included in the default inventory.

## Qwen

- Pricing and model list: <https://www.alibabacloud.com/help/en/model-studio/model-pricing>
- Qwen 3.8 Max: <https://www.alibabacloud.com/help/en/model-studio/qwen3-8-max>
- Qwen3 Coder Next: <https://www.alibabacloud.com/help/en/model-studio/qwen3-coder-next>
- Release list: <https://www.alibabacloud.com/help/en/model-studio/newly-released-models>

Documented OpenAI-compatible inventory contains `qwen3.8-max`, `qwen3.8-max-0902`, `qwen3.8-flash`, and `qwen3-coder-next`. These entries remain non-requestable until a compatible base URL and credential publish the route. Qwen OAuth portal IDs such as `coder-model` and `vision-model` remain channel-specific and are not rewritten as Model Studio canonical IDs.

## GLM

- Conversation models: <https://docs.bigmodel.cn/api-reference/模型-api/对话补全>
- Release history: <https://docs.bigmodel.cn/cn/update/new-releases>
- Pricing: <https://open.bigmodel.cn/pricing>

Current documented general defaults include `glm-5.3` and `glm-5.3-flash`. No official exact model ID for `glm-5.3-flashx` was found on 2026-09-23, so CPA does not add it. Existing vision, voice, and AutoGLM defaults have explicit `unpriced` records because exact current USD rates and billing units were not independently recoverable from the client-rendered official pricing page.

## MiniMax

- Models: <https://platform.minimax.io/docs/guides/models-intro>
- OpenAI-compatible model list: <https://platform.minimax.io/docs/api-reference/models/openai/list-models>
- Anthropic-compatible model list: <https://platform.minimax.io/docs/api-reference/models/anthropic/list-models>
- Pricing: <https://platform.minimax.io/docs/guides/pricing-paygo>

Documented compatibility inventory contains `MiniMax-M3`, `MiniMax-M2.7`, and `MiniMax-M2.7-highspeed`. Static endpoint examples can be narrower than the models overview, and an authenticated account response remains authoritative for that credential. Without a configured endpoint and credential, these catalog entries remain non-requestable.

## iFlow, Devin, Antigravity, Home, and plugins

- iFlow follows its own account inventory and changelog; first-party retirement dates do not remove an iFlow slug without iFlow evidence
- Devin keeps the `devin/` namespace and its independent dynamic catalog
- Antigravity uses the account-discovered Cloud Code catalog; Gemini Interactions agent IDs are not inserted into that channel
- Home preserves provider-bound same-ID routes and aggregates capabilities conservatively across requestable sources
- Plugin and generic compatibility models are merged from their runtime provider; CPA does not claim a static complete inventory for non-enumerable providers
