package auth

const liveCodexResponsesCreatedUpstream = `{"type":"response.created","response":{"id":"resp_live","object":"response","created_at":1782272843,"status":"in_progress","model":"gpt-5.4","output":[],"parallel_tool_calls":true}}`

const liveCodexResponsesCompletedUpstream = `{"type":"response.completed","response":{"id":"resp_live","object":"response","created_at":1782272843,"status":"completed","model":"gpt-5.4","output":[{"type":"message","content":[{"type":"output_text","text":"Hi!"}]}]}}`

const liveAntigravityMessagesStartUpstream = `{"type":"message_start","message":{"id":"msg_live","type":"message","role":"assistant","content":[],"model":"gemini-3-flash","stop_reason":null,"stop_sequence":null,"usage":{"input_tokens":2,"output_tokens":1}}}`

const liveKimiChatChunkUpstream = `{"id":"chatcmpl-live","object":"chat.completion.chunk","created":1782272842,"model":"kimi-k2.5","choices":[{"index":0,"delta":{"role":"assistant","content":""},"finish_reason":null}]}`

const liveKimiMessagesStartUpstream = `{"type":"message_start","message":{"id":"msg_live","type":"message","role":"assistant","content":[],"model":"kimi-k2.5","stop_reason":null,"stop_sequence":null,"usage":{"input_tokens":1263,"output_tokens":0}}}`

const liveCodexResponsesNonStreamUpstream = `{"model":"gpt-5.4","output":[{"type":"message","content":[{"type":"output_text","text":"Hi!"}]}]}`

const liveKimiMessagesNonStreamUpstream = `{"type":"message","role":"assistant","model":"kimi-k2.5","content":[{"type":"text","text":"hi"}]}`
