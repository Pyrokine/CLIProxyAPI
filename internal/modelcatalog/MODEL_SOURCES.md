# Model catalog sources

Checked on 2026-11-18

## Catalog boundaries

The embedded inventory supplies default model names and capabilities. Remote catalogs, the Codex client manifest,
user `models.json`, and runtime credential discovery have separate lifecycles. A requestable route requires a
registered credential with the appropriate account entitlement, enabled model, exclusions, quota, and API key policy.

Default changes use the existing preview and per-model confirmation flow. User aliases, disabled models, custom
prices, and deletion decisions keep their existing meaning. Price-only records do not create inventory entries.
The embedded `legacy` sections preserve exact historical metadata for lookup without advertising those IDs as current
defaults. Provider-specific lookup keeps its channel boundary.

Explicit cross-provider models keep their configured IDs and aliases. Capability lookup uses the channel's current
or historical metadata first, then the global model reference.

## Claude

- [Current models](https://platform.claude.com/docs/en/about-claude/models/overview)
- [Model lifecycle](https://platform.claude.com/docs/en/about-claude/model-deprecations)
- [Thinking](https://platform.claude.com/docs/en/build-with-claude/thinking)
- [Fable 5.2 migration](https://platform.claude.com/docs/en/models/fable-5-2/migration-guide)
- [Haiku 5.5 migration](https://platform.claude.com/docs/en/models/haiku-5-5/migration-guide)

Current additions are `claude-fable-5-2` and `claude-haiku-5-5`, each with a 1,000,000-token context and a
128,000-token output limit. Both use adaptive thinking and five effort levels. Fable 5.2 keeps thinking enabled and
rejects manual thinking budgets and forced tool choices. Haiku 5.5 permits disabled thinking through high effort and
accepts forced tool choices. Both omit temperature, top_p, and top_k.

Fable 5.1 and Fable 5.2 share the existing progress-update path. Haiku 5.5 has per-message effort support but stays
outside that progress classification. Sonnet 5.5 retains the `none` to `between_tools` conversion. New requests and
history must follow the model's thinking-block, prefill, toolset, and account-binding rules in the migration guides.

Retired Opus 4, Opus 4.1, Sonnet 4, Sonnet 3.7, and Haiku 3.5 snapshots remain lookup-only. Fable and Mythos restricted
access requirements continue to apply to each account; restricted models are not added from a family name alone.

## OpenAI and Codex

- [Public API models](https://developers.openai.com/api/docs/models/all)
- [Codex models and retirement](https://learn.chatgpt.com/docs/models)
- [Codex plan pricing](https://learn.chatgpt.com/docs/pricing)
- [Official client manifest](https://raw.githubusercontent.com/openai/codex/main/codex-rs/models-manager/models.json)
- [Runtime provider catalog](https://raw.githubusercontent.com/router-for-me/models/refs/heads/main/models.json)
- [Runtime client mirror](https://raw.githubusercontent.com/router-for-me/models/refs/heads/main/codex_client_models.json)

Paid Codex sections add `gpt-6.2-astra`, `gpt-6.2-sol`, and `gpt-6.1-terra`. Their default client context limits are
272,000, 500,000, and 500,000 tokens. GPT-6.1 Sol also uses 500,000. GPT-5.6 client routes retain their current
272,000-token defaults. The client manifest's maximum context and minimum client version remain separate fields.
The new dedicated client rows use the official templates and retain `base_instructions` for CPA's existing client
contract.

The new models preserve Responses reasoning configuration updates. GPT-6.2 rejects effort `none`; Codex Ultra uses
client-side task delegation. Chat Completions support does not include GPT-6.2 tool calling, which requires Responses.

Free retains its documented text models and excludes image generation and the new paid models. GPT-5.5 is lookup-only
for Codex plan retirement while its separately documented public API reference remains available. GPT-5.3 Codex Spark
also keeps historical exact-ID lookup.

### Speed selection

Model management stores each model's speed in `models.json`: `inherit`, `standard`, `fast`, or `ultrafast`.
The server supplies `speed_options` for the exact model and channel. A selection applies to that channel; aliases use
the client model's setting before the forwarded model's setting. Default adoption preserves the saved speed.

- [Fast processing](https://developers.openai.com/api/docs/guides/fast-mode)
- [Fast model pricing](https://developers.openai.com/api/docs/pricing)
- [Ultrafast processing](https://developers.openai.com/api/docs/guides/ultrafast-mode)
- [Standard service tier](https://developers.openai.com/api/reference/resources/responses/methods/create)
- [Claude Fast mode](https://platform.claude.com/docs/en/build-with-claude/fast-mode)

OpenAI and Codex Fast sends `service_tier=priority` for exact models with published Fast prices. Ultrafast sends
`service_tier=ultrafast` for GPT-6.2 Astra, GPT-6.2 Sol, GPT-6 Astra, and GPT-6.1 Sol through Responses, including
Codex Responses. Standard explicitly sends `service_tier=default`; omitting the field would inherit the upstream
project setting. Account and plan access are checked by the upstream service.

Claude Fast sends `speed=fast` for Fable 5.2, Mythos 5.2, Opus 5.5, Opus 5, and Opus 4.8. Standard sends
`speed=standard`. Opus 5 and Opus 4.8 retain the required Fast beta header; Opus 4.8 Fast retires on 2026-11-30.
Foundry's separate `-fast` deployment IDs keep their own routing contract.

Explicit payload overrides and filters retain priority over the model selection. Token counting and Responses compact
keep their existing payload rules. Request details show the requested and returned tier and speed separately;
missing upstream confirmation is unknown. Reference prices remain the documented Standard rates, so accelerated
charges require the published tier conditions.

### GPT Image

- [Image generation API](https://developers.openai.com/api/docs/guides/image-generation)
- [GPT Image 2.6 Flare](https://developers.openai.com/api/docs/models/gpt-image-2.6-flare)
- [GPT Image 2.6 Sunburst](https://developers.openai.com/api/docs/models/gpt-image-2.6-sunburst)

GPT Image 1.5, Image 2, the four exact Image 2.5 Flare/Sunburst IDs, and Image 2.6 Flare/Sunburst use the existing
Images
generation, edit, and Responses image-tool routes. The 2.6 models require OpenAI API-key credentials. Explicit 2.6 tools
filter credential selection before execution, including mixed API-key/OAuth pools and Home routes. Codex OAuth plan
inventories retain their own model sets.

The 2.6 path preserves complexity, markup, safety identifiers, image continuation, SVG, GIF, and animation parameters.
SVG downloads require the requesting principal and use attachment delivery with sandbox CSP and nosniff. The generation
guide documents 2.6 features; some method pages still describe 2.5. Account entitlement and generated-media quality
remain
unverified. Modality and per-image billing conditions are recorded in the price sources.

## Gemini API, AI Studio, Vertex, CLI, and Antigravity

- [Gemini models](https://aistudio.google.com/docs/models)
- [Gemini lifecycle](https://aistudio.google.com/docs/deprecations)
- [GenerateContent thinking](https://aistudio.google.com/docs/thinking?generate-content=true)
- [Long decode](https://aistudio.google.com/docs/long-decode?generate-content=true)
- [Vertex lifecycle](https://docs.cloud.google.com/gemini-enterprise-agent-platform/models/model-versions)
- [Nano Banana 2.1](https://docs.cloud.google.com/gemini-enterprise-agent-platform/models/gemini/nano-banana-2-1)
- [Antigravity models](https://antigravity.google/docs/models)

Gemini API, AI Studio, and Vertex add the documented GenerateContent models `gemini-4-carbon`, `gemini-4-argon`,
`gemini-4-hydrogen`, and `gemini-nano-banana-2.1`. Carbon and Argon use low/medium/high effort; Hydrogen also supports
xhigh. Their catalog thinking metadata is level-only, so the existing applier sends `thinkingLevel` rather than a
numeric thinking budget.

Carbon and Argon have a 1,048,576-token combined window. Ordinary non-streaming REST calls allow 32,768 output tokens;
streaming allows 65,536. Million-token output requires continuation, which is not advertised as a single CPA response.
Hydrogen uses the 65,536 ordinary output limit. Nano Banana 2.1 uses the image-capable GenerateContent path; its Vertex
API rejects seed, topK, logprobs, temperature, and topP. The shared Google request and response paths retain native
image configuration, inline image data, MIME types, and thought signatures. Responses image outputs and Chat image
history support replay; stream image indices remain distinct across frames. Gemini API, Vertex, CLI, Antigravity, and
AI Studio relay preparation have offline contract coverage when the exact model is routed through those channels.
Actual channel entitlement and the AI Studio relay round trip remain unverified.

Vertex 2.5 Pro, Flash, and Flash-Lite retired on 2026-10-20. The five retained Imagen exact endpoints retired on
2026-06-30. They are lookup-only in those channels. Vertex 3.6 Flash remains listed on the check date and is scheduled
for retirement on 2026-11-19. Developer API lifecycle dates remain independent of Vertex dates.

Closed previews leave their affected default sections and retain historical lookup. Existing API routing aliases and
CLI compatibility entries keep their channel identity. Public API IDs are not copied into Gemini CLI or Antigravity
without exact channel evidence. The new Antigravity CLI slugs have no verified Cloud Code wire mapping and remain
outside automatic expansion. Live, TTS, Omni, embeddings, and transcription keep their dedicated endpoint contracts.

## DeepSeek

- [Current pricing and models](https://api-docs.deepseek.com/quick_start/pricing/)
- [Thinking modes](https://api-docs.deepseek.com/guides/thinking_mode/)
- [Anthropic-compatible API](https://api-docs.deepseek.com/guides/anthropic_api/)
- [Change history](https://api-docs.deepseek.com/updates/)

Defaults use `deepseek-flash` and `deepseek-v4-pro`. Flash currently serves DeepSeek V4.1 Flash 1017 and accepts text
and
images. Pro accepts text. Both keep the existing 1,048,576 context value and the documented 393,216 output maximum.
The source labels the context as 1M without expanding its integer value. Anthropic effort aliases remain distinct from
the narrower OpenAI-compatible effort list; manual thinking budgets are ignored by the DeepSeek endpoint.

`deepseek-v4-flash` and `deepseek-v4-flash-vision-exp` are temporary compatibility redirects to Flash. They retain
lookup and price-only records. Their final alias removal date is unknown. Runtime discovery uses the existing
account-authenticated `/models` path when the user invokes it.

## Kimi

- [Kimi API models](https://platform.kimi.ai/docs/models)
- [Kimi API parameters](https://platform.kimi.ai/docs/api/models-overview)
- [Kimi Coding models](https://www.kimi.com/code/docs/kimi-code/models.html)
- [Coding release history](https://www.kimi.com/code/docs/en/kimi-code/whats-new.html)

Coding adds `kimi-k3.1-pro` and `kimi-k3.1-pro-highspeed`; the existing normalizer sends `k3.1-pro` and
`k3.1-pro-highspeed`. These routes have a 1,048,576-token context and low/high/max effort. The Coding documentation does
not publish a separate output maximum, so the new records omit it.

The fixed `kimi-k2.8`, `kimi-k2.8-code`, and `kimi-k3-256k` entries remain. The two K2.8 names and the regular K2.7 Code
compatibility name select `kimi-for-coding`, currently K2.8 Preview. Disabling thinking selects the documented
non-thinking route. K2.7 Code Highspeed retains its separate target. Old K2-family Coding IDs remain compatibility
entries with unverified current response identity. Moonshot API prices and output maxima remain separate from Coding
membership and quota consumption.

## Qwen

- [Qwen Code OAuth constants](https://github.com/QwenLM/qwen-code/blob/main/packages/core/src/models/constants.ts)
- [Qwen4 Max](https://www.alibabacloud.com/help/en/model-studio/qwen4-max)
- [Model Studio pricing](https://www.alibabacloud.com/help/en/model-studio/model-pricing)
- [Legacy model retirement](https://www.alibabacloud.com/en/notice/model_studio_notice_on_the_postponed_retirement_of_selected_legacy_models_7d0?_p_lc=1)

Qwen OAuth currently lists `coder-model`, described as Qwen 3.7 Max with vision. Its current context and output maxima
are unspecified by those constants. The previous coder-plus, coder-flash, and vision OAuth entries remain lookup-only.

Model Studio compatibility defaults add Qwen4 Max, current Qwen 3.8 variants, Coder Flash, and VL Plus. Qwen4 has a
1,000,000-token context, a 131,072 non-thinking output maximum, and a 262,144 thinking output maximum. These API model
names require a configured compatible endpoint. Coder Next and the affected legacy hosted Qwen3 models retired on
2026-10-10. Their historical prices do not restore OAuth or hosted-API availability.

## GLM and MiniMax

- [GLM 5.3](https://docs.z.ai/guides/llm/glm-5.3)
- [GLM 5.3 Flash and FlashX](https://docs.z.ai/guides/vlm/glm-5.3-flash)
- [Z.AI pricing](https://docs.z.ai/guides/overview/pricing)
- [MiniMax current and retired models](https://platform.minimax.io/docs/faq/models)
- [MiniMax OpenAI-compatible API](https://platform.minimax.io/docs/api-reference/text-chat-openai)

GLM compatibility defaults include `glm-5.3-flashx` and `glm-5.3-flashx-legal`. API availability, Coding Plan access,
and the mainland CNY and international USD price tables remain separate facts.

MiniMax compatibility defaults use `MiniMax-M3` and `MiniMax-M3.1-Flash`. M3.1 Flash has mandatory adaptive thinking,
five effort levels, and no Priority tier. M2.7 and M2.5 pay-as-you-go endpoints retired on 2026-11-18. M Plan and Token
Plan aliases have forwarded their old names to M3.1 Flash since 2026-10-19. Existing user routes and historical IDs
remain independently configured.

## xAI and Meta

- [xAI models](https://docs.x.ai/developers/models)
- [xAI pricing](https://docs.x.ai/developers/pricing)
- [Meta models](https://dev.meta.ai/docs/models)
- [Meta image generation](https://dev.meta.ai/docs/image-generation)
- [Meta file transcription](https://dev.meta.ai/docs/speech-to-text)
- [Meta pricing](https://dev.meta.ai/docs/pricing-rate-limits)

Grok metadata includes the current context, image-input and reasoning-level distinctions. Grok Imagine Video 1.5 Lite
is included through the existing video path. Video 1.5 accepts reference audio and native 1080p; its Lite variant has
480p, 720p, and upscaled 1080p output. The legacy image-quality name redirects to Image 2.0 low quality from 2026-11-02.
The xAI model and price values were available through official-site search excerpts; the full pages could not be
retrieved during this check.

Meta defaults contain seven text-output models: five Spark variants, `muse-wonder-1.0`, and `muse-ember-1.0`, plus
`muse-image-1.0` and `muse-voice-transcribe-1.0` for dedicated media requests. Contributor is a separate exact ID with
its
own data-use conditions. Image generation and edits use the existing Images routes; Muse Image also keeps its native
Responses tool contract. File transcription uses `POST /v1/asr/transcribe`, preserving the native request JSON and audio
multipart parts through the normal public authentication and execution policy. JSON, text, and SSE responses are
supported. Realtime voice WebSocket support remains unimplemented, and real Meta account access is unverified.

## iFlow, Devin, Home, and plugins

- [iFlow shutdown announcement](https://vibex.iflow.cn/t/topic/4819)
- [Devin public catalog mirror](https://raw.githubusercontent.com/router-for-me/models/refs/heads/main/devin_models.json)

iFlow API, model library, and CLI service closed on 2026-04-17. Its default inventory is empty and its exact historical
metadata remains queryable. First-party model fees are not assigned to the retired subscription channel.

Devin embeds the current 52-model mirror plus the four existing public compatibility aliases. Runtime loading also
retains the existing slow SWE builtin and `devin/` namespace. The mirror is a channel inventory source, not an official
vendor price list; Devin subscription token rates remain unknown.

Home keeps provider-bound same-ID routes and aggregates capabilities across requestable sources. Plugin and generic
compatibility routes use their configured or discovered inventory. Actual account access and provider billing were not
measured in this local update.
