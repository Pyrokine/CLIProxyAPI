# Default model price sources

`default_prices.json` stores USD prices per one million tokens

- `prompt`: uncached input
- `completion`: output
- `cache`: cached input or cache hit

Media APIs priced per image or per second are intentionally omitted from the token price table

## 2026-07-31 baseline

| Models | Source | Notes |
|---|---|---|
| `gpt-5.6-sol`, `gpt-5.6-terra`, `gpt-5.6-luna` | [OpenAI GPT-5.6](https://openai.com/index/gpt-5-6/) | Standard context rates; the current schema cannot represent long-context multipliers |
| `claude-sonnet-5`, `claude-fable-5` | [Anthropic Claude pricing](https://platform.claude.com/docs/en/docs/about-claude/pricing) | Sonnet 5 introductory pricing ends on 2026-08-31; standard pricing starts on 2026-09-01 |
| `gemini-3.5-flash` | [Google Gemini API pricing](https://ai.google.dev/gemini-api/docs/pricing) | Standard paid-tier rates |
| `deepseek-v4-pro` | [DeepSeek Models & Pricing](https://api-docs.deepseek.com/quick_start/pricing) | `prompt` is cache-miss input; `cache` is cache-hit input |
| `grok-4.5` | [xAI Grok 4.5](https://docs.x.ai/developers/models/grok-4.5) | Standard context rates; the current schema cannot represent long-context multipliers |
| `kimi-k2.7-code` | [Kimi K2.7 Code](https://www.kimi.com/resources/kimi-k2-7-code) | `prompt` is uncached input; `cache` is cached input |

A model added to the default runtime catalog must have a token price or an explicit exemption in `default_prices_test.go`
