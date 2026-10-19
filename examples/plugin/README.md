# Plugin examples

- [Host model callbacks](host-model-callback/README.md) call the host executor from a plugin resource
- [Scheduler](scheduler/README.md) selects credentials or delegates to the built-in scheduler
- [Codex service tier](codex-service-tier/README.md) normalizes outgoing requests
- [JS handler](jshandler/README.md) applies script-based request and response interceptors

## Host HTTP callback cancellation

To cancel `host.http.do` or `host.http.do_stream` before a response or `stream_id` is returned:

1. Call `host.http.operation_open` with the live `host_callback_id` that the HTTP request will use, if present; it must belong to the calling plugin instance.
2. Pass the returned `operation_id` and the same `host_callback_id` to `host.http.do` or `host.http.do_stream`.
3. Call `host.http.cancel` with that `operation_id` from the same plugin instance; cancellation also removes an opened host HTTP stream.

Each operation can be claimed once, and HTTP streams belong to their creating plugin instance. Cancel unused operations and close completed streams with `host.http.stream_close`. Opening an operation does not set a new per-call deadline; a plugin-selected deadline requires a cancellation call. The host's existing HTTP timeout and parent-context deadline still apply.

## Executor error status

A shared-library executor must serialize an upstream failure in the RPC error envelope for both `executor.execute` and `executor.execute_stream`. Set `error.http_status` to the upstream HTTP status when one exists. Go plugins can create the envelope with `pluginabi.NewErrorEnvelope`:

```go
raw, err := pluginabi.NewErrorEnvelope("rate_limit", "upstream rate limit", http.StatusTooManyRequests)
```

The host decodes this field into an error with `StatusCode()`. If the status is absent, the HTTP error handler uses 500 when the failure reaches it. Routing may retry with another credential before sending a response. For streaming requests, an error returned before the stream opens can set the HTTP status; once the response is committed, a later stream error cannot change that status. A Go `error` value alone does not carry fields across the shared-library ABI; encode the status in the JSON envelope.
