# Model catalog sources

Checked on 2026-09-11

## DeepSeek

- Models: <https://api-docs.deepseek.com/api/list-models/>
- V4.1 Flash release: <https://www.deepseek.com/en/news/deepseek-v4-1-flash/>
- Anthropic-compatible API: <https://api-docs.deepseek.com/guides/anthropic_api/>
- Discovery: use authenticated `GET https://api.deepseek.com/models` when editing a DeepSeek provider
- Static defaults: `deepseek-flash`, `deepseek-v4-pro`

`deepseek-flash` is the canonical V4.1 Flash API ID. `deepseek-v4-flash` and
`deepseek-v4-flash-vision-exp` are retired IDs that DeepSeek temporarily forwards to V4.1 Flash. CPA retains static
capability and price metadata for those two IDs but does not include them in the default model inventory

The authenticated response remains the source for the models available to a specific API key and can change without a
CPA release. Static defaults keep the management catalog usable before provider discovery runs

## GLM

- Source: <https://docs.bigmodel.cn/api-reference/模型-api/对话补全>
- Release history: <https://docs.bigmodel.cn/cn/update/new-releases>
- Discovery: no verified account-scoped model-list endpoint is used by CPA
- Static defaults: the text, vision, phone-agent, and voice model IDs listed by the conversation API documentation

The latest documented general model is `glm-5.3`. The latest documented Flash model is `glm-5.3-flash`

## Catalog rules

Price records do not create model inventory entries. Provider discovery adds only selected model IDs to that provider
configuration and preserves metadata already saved for existing entries
