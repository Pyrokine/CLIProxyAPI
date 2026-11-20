# Default model price sources

`default_prices.json` is a reference-price manifest. Its keys do not create model inventory entries. Model inventory
comes from the embedded provider catalogs and runtime discovery.

All numeric token rates use USD per one million tokens. Usage totals use these rates to estimate the reference amount
for each recorded token bucket.

- `prompt`: uncached input
- `completion`: total output, including reasoning tokens already contained in output usage
- `cache`: legacy cached-input rate
- `cache_read`: cache-read input
- `cache_write`: cache-write input when the TTL is not represented separately
- `cache_write_5m`: cache-write input with a 5-minute TTL
- `cache_write_1h`: cache-write input with a 1-hour TTL
- `complete`: all request token buckets needed by the model have fixed rates
- `partial`: a rate, tier, storage charge, or effective condition is not fully identifiable from request usage
- `unpriced`: no numeric token reference rate is recorded
- `auto_apply`: legacy metadata describing whether known parts of a `partial` price used to be applied automatically; it
  does not gate reference amount calculation

`partial` and `unpriced` are compatibility status values. A status value does not discard numeric reference rates. For
request-time receipts, a known 5-minute or 1-hour write uses its matching rate, then `cache_write`, then the prompt
rate.
A write with unknown TTL uses `cache_write`; if that rate is absent, it uses the higher configured TTL rate, then the
prompt rate. Historical records without a receipt retain their original `cache_write` or prompt base and
cache-accounting
semantics. An explicit zero rate remains zero. Models without any numeric token rate do not receive a fabricated amount.

`effective_from` and `effective_until` restrict a reference rate to the event timestamp. A date-only `effective_until`
includes the entire stated day. The usage store captures the catalog revision, effective range, provider, requested and
canonical model, currency, token coverage, and line items before accepting a live event. Historical repricing updates
only the current-catalog derived amount; it does not change this request-time receipt.

Usage aggregates are denominated in USD. New price records with another currency are rejected. A legacy non-USD record
can still be read for audit evidence, but its request amount is marked `unpriced` and excluded from USD aggregation.

Media output priced per image, video duration, or another non-token unit is marked `unpriced` in this manifest.

## 2026-11-19 speed-based reference amounts

Model prices contain `speed_rates` rules for an observed speed, provider, and effective interval. A rule's `multiplier`
applies to the base token rates. Optional `multipliers` values select separate factors for `prompt`, `completion`,
`cache_read`, `cache_write`, `cache_write_5m`, and `cache_write_1h`. The default factor is a finite positive JSON
number;
bucket factors are finite nonnegative JSON numbers. An explicit numeric zero bucket factor remains zero; `null` is
rejected.
The resolver selects the token bucket's base rate, including its existing cache fallback, before applying its factor
once.

Rules use `standard`, `fast`, `ultrafast`, `flex`, or `batch`. A rule with an empty `provider` applies to all providers;
a matching exact provider takes precedence. Rules for the same speed and provider have non-overlapping effective
intervals. Date-only end values include that entire UTC day; RFC3339 end timestamps are exclusive.

Default speed rules use non-promotional standard multipliers. `openai` uses the published API token-price factors;
`codex` uses the published Included usage factors as CPA's local reference-cost policy. The Codex rule source points to
its own pricing table, while the model's base reference prices retain their API sources. Token counts remain measured
usage. Temporary API or Credits/PAYG promotions are excluded from the defaults.

The defaults contain 76 rules across 28 exact model IDs: 60 `openai`, 11 `codex`, and five `claude` rules. The current
Codex table publishes seven Fast and four Ultrafast rules. Unlisted Codex rates retain the existing `partial` handling
when that speed is observed; the model-price editor can maintain independently verified channel rules.

| Provider | Models                                                                                                                        | Fast factor                                        | Official source                                                                             |
|----------|-------------------------------------------------------------------------------------------------------------------------------|----------------------------------------------------|---------------------------------------------------------------------------------------------|
| openai   | GPT-6.2 Astra/Sol, GPT-6 Astra/Luna/Sol, GPT-6.1 Sol/Terra, GPT-5.6 Sol/Terra/Luna, GPT-5.4/5.4-mini/5.2/5.1/5, GPT-5.3 Codex | 2x                                                 | [OpenAI pricing](https://developers.openai.com/api/docs/pricing)                            |
| openai   | GPT-5.5                                                                                                                       | 2.5x                                               | [OpenAI pricing](https://developers.openai.com/api/docs/pricing)                            |
| openai   | GPT-5-mini                                                                                                                    | 1.8x                                               | [OpenAI pricing](https://developers.openai.com/api/docs/pricing)                            |
| openai   | GPT-4.1, GPT-4.1-mini, o3                                                                                                     | 1.75x                                              | [OpenAI pricing](https://developers.openai.com/api/docs/pricing)                            |
| openai   | GPT-4o                                                                                                                        | 1.7x                                               | [OpenAI pricing](https://developers.openai.com/api/docs/pricing)                            |
| openai   | GPT-4o-mini                                                                                                                   | 5/3x                                               | [OpenAI pricing](https://developers.openai.com/api/docs/pricing)                            |
| codex    | GPT-6.2 Astra/Sol                                                                                                             | 2x                                                 | [Codex pricing](https://learn.chatgpt.com/docs/pricing)                                     |
| codex    | GPT-6 Astra/Luna/Sol, GPT-6.1 Sol/Terra                                                                                       | 2.5x                                               | [Codex pricing](https://learn.chatgpt.com/docs/pricing)                                     |
| claude   | Claude Fable 5.2, Mythos 5.2, Opus 5.5/5/4.8                                                                                  | 2x, with cache modifiers applied to the Fast rates | [Claude Fast mode](https://platform.claude.com/docs/en/build-with-claude/fast-mode#pricing) |

| Provider | Models                                      | Ultrafast factor | Official source                                                  |
|----------|---------------------------------------------|------------------|------------------------------------------------------------------|
| openai   | GPT-6 Astra, GPT-6.1 Sol, GPT-6.2 Astra/Sol | 6x               | [OpenAI pricing](https://developers.openai.com/api/docs/pricing) |
| codex    | GPT-6 Astra, GPT-6.1 Sol                    | 8x               | [Codex pricing](https://learn.chatgpt.com/docs/pricing)          |
| codex    | GPT-6.2 Astra/Sol                           | 6x               | [Codex pricing](https://learn.chatgpt.com/docs/pricing)          |

These standard Ultrafast rules apply on both sides of the advertised March 2027 promotion boundary. Local default
speed rules start at `2026-11-18T00:00:00Z`; the Codex factors were checked on 2026-11-19. The Opus 4.8 Fast reference
ends at the start of its announced removal date, `2026-11-30T00:00:00Z`; the provider's exact shutdown timezone remains
unspecified.

OpenAI Flex and Batch use their published non-promotional tier prices where input, output, and cached-input rates are
all supplied. GPT-5.4 uses 0.5x for input/output and 0.52x for cache read. A missing cached-input cell retains the
unknown
condition. Codex does not inherit these API-only default rules.

The six price-only records for GPT-5-mini, GPT-4.1, GPT-4.1-mini, GPT-4o, GPT-4o-mini, and o3 provide the Standard bases
for their published API Fast rates. These records retain `partial` status because independent cache-write prices are
unspecified. Existing base prices, source dates, region conditions, and media `unpriced` records retain their values.
Kimi Highspeed IDs keep their separately published base prices. Default updates use the existing preview and adoption
flow, retaining customer overrides and historical request-time receipts.

Usage selects speed from the upstream response: Claude uses `response_speed`, while other service-tier requests map
`priority` to `fast` and `default` to `standard`. When usable base rates exist, new receipts with an unconfirmed speed,
or a confirmed nonstandard speed without a matching rule, retain the base reference estimate with `partial` coverage
and a reason. Missing or unusable base rates keep their `unpriced` status and its reason. Historical records without a
receipt keep their original base-price status when no speed rules apply. Failed or interrupted requests retain any
response speed and valid usage that was observed.

Each request-time receipt records the observed speed and its source, selected factor, rule source and interval, base
and final line-item rates, and resulting amount. API key monetary limits consume that same amount; token limits count
the actual tokens. Price updates recalculate only current-catalog estimates, preserving the original receipts and
settled reservations. The existing default-update preview and confirmation flow selects changes to these rules.

## 2026-11-18 base reference prices

The manifest contains 214 exact-ID records: 24 complete, 94 partial, and 96 unpriced. Each record carries its source,
check date, and applicable conditions. All check dates are 2026-11-18. The defaults revision remains 2026-10-09.1 during
this local, unpublished update.

The following values are USD per one million tokens. They use the named API's Standard reference tier; subscription
access and quota consumption have separate contracts.

| Exact model ID                           | Input / cache read / output      | Cache write                     | Official source                                                                                                                                                        |
|------------------------------------------|----------------------------------|---------------------------------|------------------------------------------------------------------------------------------------------------------------------------------------------------------------|
| `gpt-6.2-astra`                          | 8 / 0.40 / 40                    | 10 for 30m, 20 for 12h          | [OpenAI](https://developers.openai.com/api/docs/pricing)                                                                                                               |
| `gpt-6.2-sol`                            | 2 / 0.10 / 10                    | 2.50 for 30m, 5 for 12h         | [OpenAI](https://developers.openai.com/api/docs/pricing)                                                                                                               |
| `gpt-6.1-terra`                          | 1 / 0.05 / 5                     | 1.25 for 30m, 2.50 for 12h      | [OpenAI](https://developers.openai.com/api/docs/pricing)                                                                                                               |
| `claude-fable-5-2`                       | 8 / 0.20 / 40                    | 10 for 5m, 16 for 1h            | [Anthropic](https://platform.claude.com/docs/en/about-claude/pricing)                                                                                                  |
| `claude-haiku-5-5`                       | 0.10 / 0.01 / 0.50               | 0.125 for 5m, 0.20 for 1h       | [Anthropic](https://platform.claude.com/docs/en/about-claude/pricing)                                                                                                  |
| `gemini-4-carbon`, `gemini-4-argon`      | 2 / 0.10 / 10 through 2026-11-30 | Implicit caching only           | [Gemini](https://aistudio.google.com/docs/pricing)                                                                                                                     |
| `gemini-4-hydrogen`                      | 1.50 / 0.075 / 7.50              | Implicit caching only           | [Gemini](https://aistudio.google.com/docs/pricing)                                                                                                                     |
| `kimi-k3.1-pro`                          | 2 / 0.20 / 10                    | 2 for 5m, 4 for 1h              | [Kimi API](https://platform.kimi.ai/docs/pricing/chat)                                                                                                                 |
| `kimi-k3.1-pro-highspeed`                | 4 / 0.20 / 20                    | 4 for 5m, 8 for 1h              | [Kimi API](https://platform.kimi.ai/docs/pricing/chat)                                                                                                                 |
| `qwen4-max`                              | 1.50 / 0.075 / 4.50, Singapore   | 1.875 for 5m, 3 for 1h          | [Model Studio](https://www.alibabacloud.com/help/en/model-studio/model-pricing), [Qwen4 cache](https://www.alibabacloud.com/help/en/model-studio/explicit-cache-qwen4) |
| `glm-5.3-flashx`, `glm-5.3-flashx-legal` | 0.37 / 0.075 / 1.25              | Separate write rate unspecified | [Z.AI](https://docs.z.ai/guides/overview/pricing)                                                                                                                      |
| `MiniMax-M3.1-Flash`                     | 0.15 / 0.015 / 0.60              | Separate write rate unspecified | [MiniMax](https://platform.minimax.io/docs/guides/pricing-paygo)                                                                                                       |
| `muse-wonder-1.0`                        | 4 / 0.20 / 20                    | Separate write rate unspecified | [Meta](https://dev.meta.ai/docs/pricing-rate-limits)                                                                                                                   |
| `muse-ember-1.0`                         | 0.10 / 0.004 / 0.50              | Separate write rate unspecified | [Meta](https://dev.meta.ai/docs/pricing-rate-limits)                                                                                                                   |

### Length, tier, region, and promotion conditions

OpenAI's table uses at most 272,000 input tokens. Above that threshold, the three models use 2x input and cache rates
and 1.5x output rates for the entire request. Batch is 0.5x Standard. Fast is 2x Standard. GPT-6.1 Terra also has a 0.5x
Flex rate. GPT-6.2 has no published Flex row. Its API Ultrafast defaults use the non-promotional 6x Standard rate.
The manifest records 30-minute writes; the separate 12-hour rate stays in these conditions.

Haiku 5.5's short tier includes all prompt tokens through 100,000, including cache reads and writes. Above 100,000,
input/read/output become 0.50/0.05/2.50, with 0.625 5-minute writes and 1.00 1-hour writes. Anthropic's global rates,
US-only inference 1.1x, Batch, and Fast remain separate choices. Sonnet 5.5 cache reads remain 0.10.

Gemini Carbon and Argon double to 4/0.20/20 on 2026-12-01. Their current records expire after 2026-11-30. Gemini 3.6 and
3.8 Flash retain their separately dated promotion through 2026-12-31. Developer API and Vertex have independent region,
cache, tier, and lifecycle conditions. The manifest uses the Developer API reference for shared IDs. Vertex non-global
and Priority/Flex/Batch rows remain separate published alternatives. Routing aliases such as `gemini-3.7-flash` and
Antigravity effort suffixes have explicit unpriced records where the exact billing identity is unknown.

Qwen numeric references select an explicitly named official USD region and input-length band. Other region and CNY
rows retain their original currencies. Qwen4's Singapore cache rates require at least 1,024 cached tokens for explicit
caching and use the requested 5-minute or 1-hour TTL. CNY-only, retired, subscription, and Realtime modality records
retain explicit unpriced status where a single USD token reference does not apply.

Z.AI USD and domestic BigModel CNY remain separate tables. Free Flash rows retain their published zeros. A limited-time
free cache-storage offer supplies no cache-write operation price or permanent free period. FlashX API availability
does not imply Coding Plan entitlement.

MiniMax M3 Standard uses 0.30/0.06/1.20 through 512K input and 0.60/0.12/2.40 above it. Priority uses
0.45/0.09/1.80 and 0.90/0.18/3.60. M3.1 Flash supports Standard only. M2-series pay-as-you-go endpoints are retired;
subscription aliases redirect under their own M Plan or Token Plan terms.

Kimi K3 now records 5-minute and 1-hour writes at 3 and 6. K3.1 Pro Highspeed keeps the same 0.20 cache-read price as
standard K3.1 Pro. K2 API access is limited to eligible existing accounts, with retirement planned for late December

2026. Coding aliases and their membership quota usage remain separate from these API reference prices.

DeepSeek's existing weekday UTC peak bands are retained. Chinese public holidays use off-peak rates; the recurring
weekly selector has no holiday calendar, so these references remain partial. The four old iFlow DeepSeek IDs have no
current channel fee and are unpriced.

Grok 4.7 and 4.6 use 2/0.50/6; 4.5 uses 2/0.30/6; 4.3 and the documented 4.20 exact IDs use 1.25/0.20/2.50. At a
prompt length of 200,000 or more, including cached input, the corresponding rates double. US endpoints add 10%.
Grok Build uses 1/0.20/2 and its own long-input row. Exact current prices for `grok-3-mini`, `grok-3-mini-fast`, and
`grok-composer-2.5-fast` remain unknown, so their previous unsourced equal rates were removed. The xAI numeric evidence
comes from official-site search excerpts because full-page retrieval was unavailable.

Meta Spark Standard uses 1.25/0.15/4.25; Contributor uses 0.10/0.002/0.20 and permits submitted content to be used for
training. Ember's
rates triple above 200,000 input tokens. Search grounding costs 2.50 USD per 1,000 queries and remains outside token
reference amounts.

### Media and retired endpoints

GPT Image 2.6 Flare/Sunburst, Nano Banana 2.1, Grok image/video, Meta image/transcription, and retained Imagen records
keep published modality or media units in their source conditions. The combined token buckets cannot select all those
rates, so these records stay unpriced.

Nano Banana 2.1 Standard has 1.50 input, 7.50 text/thinking output, and 30 image output per million tokens. Its Standard
per-image equivalents are 0.0336 for 1K, 0.0504 for 2K, and 0.113 for 4K. Batch uses 0.75 input, 3.75 text/thinking,
15 image output, and the separately published per-image values. Vertex cache rates remain channel-specific.

Grok Video 1.5 Lite charges 0.01 USD per input image, 0.02 per output second at 480p, 0.03 at 720p, and 0.14 for
upscaled
1080p. Full Video 1.5 also bills video input; its unverified input-video amount remains unspecified. Meta Image costs
0.01 USD per successful image, and Meta Voice Transcribe costs 0.18 USD per hour. The Meta inventory exposes both media
models through their image and file-transcription routes. These per-image and per-hour amounts remain source conditions;
the token-price manifest keeps both records unpriced. The same separation applies to GPT Image 2.6, Nano Banana 2.1,
and Grok Video 1.5 Lite even though their local execution paths are implemented.

The five Imagen endpoints retired on 2026-06-30 and iFlow's official service closed on 2026-04-17. Historical exact-ID
metadata, prices, and request-time receipts retain their own dates. Default updates still require the existing user
confirmation flow. Actual provider invoices and account entitlement were not measured by this local update.

## 2026-11-06 reference-price verification (historical)

Canonical Anthropic, OpenAI, Gemini, DeepSeek, Kimi K3 and MiniMax M3 entries use the retrieved official USD reference
rates and identify the Standard tier. Qwen 3.8 remains unpriced because no deployment region is selected. Dated
snapshots and local aliases require exact billing identity evidence before their checked dates or amounts change.

OpenAI's 30-minute and 12-hour cache writes have different prices. The manifest records the 30-minute reference and
identifies the unrepresented 12-hour rate in limitations. GPT-5.6 Cyber's exact model page publishes USD 15.625 per
million cache-write tokens; the generic pricing page lists USD 15. The exact-model rate is retained with the source
discrepancy recorded.

DeepSeek weekday peak bands exclude Chinese public holidays. These records are partial because the recurring UTC
selector has no holiday calendar. Kimi's current chat page omitted numeric tables, Zhipu's page exposed no prices, and
the xAI pricing and model pages failed to fetch. Those exact rates remain unverified and retain their previous amounts
and checked dates.

Exact retired Codex pages publish historical input/cache-read/output rates for `gpt-5.2-codex` (1.75/0.175/14),
`gpt-5.1-codex`, `gpt-5.1-codex-max` and `gpt-5-codex` (1.25/0.125/10), and `gpt-5.1-codex-mini` (0.25/0.025/2), in USD
per million tokens. These price records do not restore retired models to inventory. Their pages publish no separate
cache-write rate.

Anthropic's lifecycle and model-versioning documentation identify seven dated Opus, Sonnet and Haiku snapshots. Their
reference rates use the named-model rows in the official pricing table; retirement conditions remain attached to each
price record.

### Z.AI and Chinese BigModel

Fifteen exact GLM entries use [Z.AI's USD API reference rates](https://docs.z.ai/guides/overview/pricing). They are
partial references: the source specifies neither region nor service tier, and its `Cached Input Storage` column does not
define a cache-write operation or storage period. The three Flash entries explicitly labeled Free retain intentional
zero input, cache-read and output rates. These references apply to Z.AI, while Chinese BigModel has separate CNY
billing.

Nine other GLM entries remain unpriced using the exact conditions
in [BigModel's CNY table](https://docs.bigmodel.cn/cn/guide/start/pricing.md).
[AutoGLM-Phone](https://docs.bigmodel.cn/cn/guide/models/vlm/autoglm-phone)
advertises a limited-time free offer without currency, billing units, expiry or account conditions. No permanent zero
USD token rate is assigned to that offer.

### Historical DeepSeek references

[DeepSeek V3](https://api-docs.deepseek.com/news/news1226) publishes USD 0.27 uncached input, 0.07 cache-hit input and
1.10 output per million tokens after its introductory
period. [DeepSeek V3.1](https://api-docs.deepseek.com/news/news250821) publishes 0.56, 0.07 and 1.68, effective
September 5, 2025, at 16:00 UTC. These are historical native-API references with no separately published cache-write
rate. The V3.2-Exp pricing image does not establish stable V3.2 price parity, so the stable record remains unverified.

### Image pricing

The six GPT Image entries below and `gemini-3-pro-image`, `gemini-2.5-flash-image`, `gemini-3.1-flash-image` and
`gemini-3.1-flash-lite-image` are `unpriced` and contain no numeric token rates. Their billing distinguishes token
modalities or uses per-image units. Source URLs and conditions remain available for manual pricing. The usage-consumer
regression checks these ten and the five Imagen entries below through the legacy lookup, request logger, persisted
amount and request-time receipt.

[Google Cloud's Imagen table](https://cloud.google.com/model-garden/pricing) lists USD 0.04 per generated image for
Imagen 4, USD 0.06 for Imagen 4 Ultra, USD 0.02 for Imagen 4 Fast, USD 0.04 for Imagen 3 and USD 0.02 for Imagen 3 Fast.
The [Imagen 4 model reference](https://docs.cloud.google.com/vertex-ai/generative-ai/docs/models/imagen/4-0-generate)
identifies `imagen-4.0-generate-001`, `imagen-4.0-ultra-generate-001` and `imagen-4.0-fast-generate-001`;
the [Imagen 3 reference](https://docs.cloud.google.com/vertex-ai/generative-ai/docs/models/imagen/3-0-generate)
identifies `imagen-3.0-generate-002` and `imagen-3.0-fast-generate-001`. In the token manifest, these Vertex AI entries
keep the `unpriced` status. The separate Imagen 4 upscaling rate is USD 0.06 per image.

All five exact Imagen endpoints were discontinued on June 30, 2026. Their model references retain this lifecycle
condition. The pricing table states no region, service tier or rate-effective period; the manifest leaves those
conditions unspecified and records discontinuation separately from price-effective dates.

| Models                                                        | Official source                                                                  | Unrepresented conditions                                                                                                         |
|---------------------------------------------------------------|----------------------------------------------------------------------------------|----------------------------------------------------------------------------------------------------------------------------------|
| `gpt-image-2.5-flare`, `gpt-image-2.5-flare-2026-09-08`       | [Flare](https://developers.openai.com/api/docs/models/gpt-image-2.5-flare)       | USD per one million tokens: text input 5, cached text 1.25, image input 8, cached image 2; image output 30 Standard or 15 Batch. |
| `gpt-image-2.5-sunburst`, `gpt-image-2.5-sunburst-2026-09-08` | [Sunburst](https://developers.openai.com/api/docs/models/gpt-image-2.5-sunburst) | The same modality-specific USD rates and Standard/Batch output rates.                                                            |
| `gpt-image-1.5`                                               | [GPT Image 1.5](https://developers.openai.com/api/docs/models/gpt-image-1.5)     | USD per one million tokens: text input 5, cached text 1.25, text output 10, image input 8, cached image 2, image output 32.      |
| `gpt-image-2`                                                 | [GPT Image 2](https://developers.openai.com/api/docs/models/gpt-image-2)         | The retrieved official model page did not expose numeric token or processing-tier prices.                                        |

## 2026-10-09 model update

| Models              | Source                                                                                 | Manifest treatment                                                                                                                                                                                                                                                                                                |
|---------------------|----------------------------------------------------------------------------------------|-------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|
| `claude-sonnet-5-5` | [Anthropic Sonnet 5.5](https://platform.claude.com/docs/en/models/sonnet-5-5/overview) | Complete global token rates: USD 2 input, USD 0.10 cache read, USD 2.50 5-minute cache write, USD 4 1-hour cache write, and USD 10 output per one million tokens.                                                                                                                                                 |
| `gpt-6.1-sol`       | [OpenAI GPT-6.1 Sol](https://developers.openai.com/api/docs/models/gpt-6.1-sol)        | Public API reference price only. Base USD 2 input, USD 0.10 cache read, USD 2.50 cache write, and USD 10 output per one million tokens are `partial`; requests over 272K input tokens use higher rates that the current manifest cannot select automatically. This price record does not grant Codex plan access. |

## 2026-09-23 model update

| Models                                                                                                                 | Source                                                                                                                                                       | Manifest treatment                                                                                                                                                                                                                       |
|------------------------------------------------------------------------------------------------------------------------|--------------------------------------------------------------------------------------------------------------------------------------------------------------|------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|
| `claude-fable-5-1`                                                                                                     | [Anthropic pricing](https://platform.claude.com/docs/en/about-claude/pricing)                                                                                | Complete global token rates: USD 10 input, USD 0.25 cache read, USD 12.50 5-minute cache write, USD 20 1-hour cache write, and USD 50 output per one million tokens.                                                                     |
| `claude-opus-5-5`                                                                                                      | [Anthropic pricing](https://platform.claude.com/docs/en/about-claude/pricing)                                                                                | Complete global token rates: USD 4 input, USD 0.20 cache read, USD 5 5-minute cache write, USD 8 1-hour cache write, and USD 20 output per one million tokens.                                                                           |
| `gpt-6-sol`, `gpt-6-luna`                                                                                              | [GPT-6 Sol](https://developers.openai.com/api/docs/models/gpt-6-sol), [GPT-6 Luna](https://developers.openai.com/api/docs/models/gpt-6-luna)                 | Base input, cache-read, cache-write, and output rates are recorded as `partial`. Requests above 272K input tokens use a higher tier that the current manifest cannot select automatically.                                               |
| `gpt-image-1.5`, `gpt-image-2`                                                                                         | [GPT Image 1.5](https://developers.openai.com/api/docs/models/gpt-image-1.5), [GPT Image 2](https://developers.openai.com/api/docs/models/gpt-image-2)       | Unpriced; see the 2026-11-06 modality and source conditions above.                                                                                                                                                                       |
| `gpt-image-2.5-flare`, `gpt-image-2.5-flare-2026-09-08`, `gpt-image-2.5-sunburst`, `gpt-image-2.5-sunburst-2026-09-08` | [Flare](https://developers.openai.com/api/docs/models/gpt-image-2.5-flare), [Sunburst](https://developers.openai.com/api/docs/models/gpt-image-2.5-sunburst) | Unpriced; modality-specific inputs and Standard/Batch image output cannot share the manifest token rates.                                                                                                                                |
| `gemini-3.1-flash-image`, `gemini-3.1-flash-lite-image`                                                                | [Gemini pricing](https://aistudio.google.com/docs/pricing)                                                                                                   | Unpriced; text/thinking and image output have different rates, and the combined usage buckets cannot select them.                                                                                                                        |
| `gemini-3.8-flash`, `gemini-3.8-flash-high`                                                                            | [Gemini pricing](https://ai.google.dev/gemini-api/docs/pricing)                                                                                              | Promotional standard rates through 2026-12-31 are recorded as `partial`. Batch and context-cache storage use separate rates, 2027 standard rates are higher, and Google publishes no separate price for the thinking-level `high` alias. |
| `grok-4.7`                                                                                                             | [xAI Grok 4.7](https://docs.x.ai/developers/models/grok-4.7)                                                                                                 | Below-200K input, cached-input, and output rates are recorded as `partial`; the current manifest cannot select the doubled above-200K tier automatically.                                                                                |
| `grok-imagine-image`, `grok-imagine-image-quality`, `grok-imagine-video`, `grok-imagine-video-1.5`                     | [xAI pricing](https://docs.x.ai/developers/pricing)                                                                                                          | Marked `unpriced`. Image models use per-image input and output units; video models use per-image input and per-second, resolution-dependent output units.                                                                                |
| `grok-imagine-image-2.0`                                                                                               | [xAI Grok Imagine Image 2.0](https://docs.x.ai/developers/models/grok-imagine-image-2.0)                                                                     | Marked `unpriced`. Output starts at USD 0.04 per 1K low-quality image, and input/output charges vary by image quality and resolution rather than token buckets represented by this manifest.                                             |
| `qwen3.8-max`, `qwen3.8-max-0902`, `qwen3.8-flash`, `qwen3-coder-next`                                                 | [Alibaba Cloud Model Studio pricing](https://www.alibabacloud.com/help/en/model-studio/model-pricing)                                                        | Marked `unpriced`. Region, context tier, request length, thinking mode, cache, and Batch pricing vary, and no default Model Studio region is selected.                                                                                   |
| `MiniMax-M3`, `MiniMax-M2.7`, `MiniMax-M2.7-highspeed`                                                                 | [MiniMax pay-as-you-go pricing](https://platform.minimax.io/docs/guides/pricing-paygo)                                                                       | Existing standard-tier numeric rates remain `partial`; service tier and long-context multipliers are not selected automatically. Rates were rechecked on 2026-09-23.                                                                     |
| `kimi-k2.7-code`, `kimi-k2.7-code-highspeed`                                                                           | [Kimi chat pricing](https://platform.kimi.ai/docs/pricing/chat)                                                                                              | Complete cache-hit, cache-miss input, and output rates are recorded. The high-speed variant uses its separately published rates.                                                                                                         |
| `glm-5.3-flash`, GLM vision, voice, and AutoGLM defaults                                                               | [Zhipu pricing](https://open.bigmodel.cn/pricing)                                                                                                            | Marked `unpriced` because the client-rendered official page did not expose independently verifiable exact current USD rates and billing units for these IDs.                                                                             |

Kimi K2.8 compatibility entries have explicit `unpriced` records with no numeric rates. Their versioned inventory source
and executor routes are documented in [Model catalog sources](MODEL_SOURCES.md#kimi). `gpt-5.3-codex` retains public API
pricing and compatibility metadata but is not restored to Codex OAuth plan inventory. `gpt-5.3-codex-spark` retains
historical exact-ID lookup and applicable historical pricing records. `kimi-k3-256k` retains its existing price record
independently of its compatibility route.

## 2026-09-11 DeepSeek update

| Models                                              | Source                                                                 | Manifest treatment                                                                                                                                                                                                   |
|-----------------------------------------------------|------------------------------------------------------------------------|----------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|
| `deepseek-flash`                                    | [DeepSeek pricing](https://api-docs.deepseek.com/quick_start/pricing/) | Off-peak rates are USD 0.15 uncached input, USD 0.003 cache-read input, and USD 0.60 output per one million tokens. Weekday peak rates are USD 0.30, USD 0.006, and USD 1.20 during 01:00-04:00 and 06:00-10:00 UTC. |
| `deepseek-v4-pro`                                   | [DeepSeek pricing](https://api-docs.deepseek.com/quick_start/pricing/) | Off-peak rates remain USD 0.66 uncached input, USD 0.022 cache-read input, and USD 1.98 output. Weekday peak rates are USD 1.32, USD 0.044, and USD 3.96 during the same recurring UTC bands.                        |
| `deepseek-v4-flash`, `deepseek-v4-flash-vision-exp` | [DeepSeek models](https://api-docs.deepseek.com/api/list-models/)      | These retired IDs are temporarily forwarded to `deepseek-flash`. They remain price-only entries using V4.1 Flash rates, so historical usage can still be priced without restoring them to the default inventory.     |

The resolver selects the recurring UTC band from the usage event timestamp. Band start times are inclusive, end times
are exclusive, and weekends use off-peak rates

## 2026-09-04 OpenAI update

| Models          | Source                                                                                    | Manifest treatment                                                                                                                                                                                                                                                                                                                                                                                                   |
|-----------------|-------------------------------------------------------------------------------------------|----------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|
| `gpt-6-astra`   | [OpenAI model documentation](https://developers.openai.com/api/docs/models/gpt-6-astra)   | Standard requests use USD 10 uncached input, USD 1 cache-read input, USD 12.50 cache-write input, and USD 50 output per one million tokens. Requests above 272K input tokens double input and cache rates and use 1.5x output rates; the current resolver cannot select that tier automatically. Access is rolling out through the Trusted Access Program.                                                           |
| `gpt-5.6-cyber` | [OpenAI model documentation](https://developers.openai.com/api/docs/models/gpt-5.6-cyber) | Standard requests use USD 12.50 uncached input, USD 1.25 cache-read input, USD 15.625 cache-write input, and USD 75 output per one million tokens. Requests above 272K input tokens double input rates and use 1.5x output rates; the source does not publish long-context cache rates, and the current resolver cannot select that tier automatically. Access requires separate Daybreak approval and provisioning. |

These entries provide reference prices only. They do not add model inventory, routing, OAuth aliases, or access
entitlement.

## 2026-08-30 baseline

| Models                                                                            | Source                                                                                                                        | Manifest treatment                                                                                                                                                                                                                                                                                                                                                                                                                                                |
|-----------------------------------------------------------------------------------|-------------------------------------------------------------------------------------------------------------------------------|-------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|
| `gpt-5.6-sol`, `gpt-5.6-terra`, `gpt-5.6-luna`                                    | [OpenAI model documentation](https://developers.openai.com/api/docs/models/gpt-5.6-sol)                                       | Base input, cache-read, cache-write, and output rates are recorded as reference rates. Requests above 272K input tokens use 2x input and 1.5x output rates, which the current resolver cannot select automatically. The public API documentation states a 1,050,000-token context window and 128,000-token output limit. The Codex runtime manifest currently advertises a 372,000-token route limit, so the Codex registry retains that lower requestable limit. |
| `claude-opus-5`, `claude-sonnet-5`, `claude-fable-5`, `claude-haiku-4-5-20251001` | [Anthropic pricing](https://platform.claude.com/docs/en/about-claude/pricing)                                                 | Base input, cache-read, 5-minute cache-write, 1-hour cache-write, and output rates are recorded. Anthropic usage exposes the two cache-creation TTL buckets; when a response provides only aggregate cache creation, the receipt records an unknown-TTL line item and uses the higher configured TTL rate.                                                                                                                                                        |
| `gemini-3.6-flash`, `gemini-3.7-flash`, provider aliases                          | [Gemini API pricing](https://ai.google.dev/gemini-api/docs/pricing)                                                           | Promotional rates through 2026-12-31 are used as reference rates. Context-cache storage is billed separately and is excluded from request token cost.                                                                                                                                                                                                                                                                                                             |
| `gemini-3.5-flash-extra-low`                                                      | [Gemini API pricing](https://ai.google.dev/gemini-api/docs/pricing)                                                           | Antigravity exposes a thinking-level alias. Google publishes no separate alias price, so the Gemini 3.5 Flash family rate is used. Cache storage is billed separately.                                                                                                                                                                                                                                                                                            |
| DeepSeek V4 models                                                                | [DeepSeek pricing](https://api-docs.deepseek.com/quick_start/pricing/)                                                        | Historical baseline superseded by the 2026-09-11 DeepSeek update above.                                                                                                                                                                                                                                                                                                                                                                                           |
| `grok-4.5`, `grok-4.6`                                                            | [xAI pricing](https://docs.x.ai/developers/pricing)                                                                           | Below-200K rates are used as reference rates. Input, cached-input, and output prices double at 200K input tokens, which the current resolver cannot select from usage metadata.                                                                                                                                                                                                                                                                                   |
| `kimi-k3`                                                                         | [Kimi K3 announcement](https://www.kimi.com/en/blog/kimi-k3)                                                                  | Input, cache-read, and output rates are used as reference rates. No cache-write rate is published, so the prompt rate estimates that bucket.                                                                                                                                                                                                                                                                                                                      |
| `kimi-k2.5`                                                                       | [Kimi API pricing](https://platform.kimi.ai/docs/pricing/chat), [iFlow changelog](https://platform.iflow.cn/en/cli/changelog) | iFlow publishes the runtime model, but the current Kimi pricing page does not publish a verified direct USD token rate for this ID. No reference amount is generated.                                                                                                                                                                                                                                                                                             |
| `MiniMax-M3`, `MiniMax-M2.7`, `MiniMax-M2.7-highspeed`, `minimax-m2.5`            | [MiniMax pay-as-you-go pricing](https://platform.minimax.io/docs/guides/pricing-paygo)                                        | M2.5 has fixed legacy rates. M2.7 variants use standard-tier reference rates because service-tier selection is not represented. M3 uses standard reference rates because prices vary by service tier and above-512K context.                                                                                                                                                                                                                                      |
| GLM 4 and GLM 5 families                                                          | [Zhipu pricing](https://open.bigmodel.cn/pricing)                                                                             | Current exact USD rates were not independently recoverable from the client-rendered official page. No reference amount is generated.                                                                                                                                                                                                                                                                                                                              |
| Qwen 3 families                                                                   | [Alibaba Cloud Model Studio pricing](https://help.aliyun.com/zh/model-studio/model-pricing)                                   | Region, context tier, thinking mode, cache, and batch pricing vary. No single fixed reference rate is recorded.                                                                                                                                                                                                                                                                                                                                                   |
| Imagen and Grok video models                                                      | [Gemini API pricing](https://ai.google.dev/gemini-api/docs/pricing), [xAI pricing](https://docs.x.ai/developers/pricing)      | Output uses non-token units, so no reference amount is generated.                                                                                                                                                                                                                                                                                                                                                                                                 |

Every model in the default runtime inventory must have either numeric token reference rates or an explicit manifest
entry explaining why no reference amount is generated. This invariant is checked by `default_prices_test.go`.
