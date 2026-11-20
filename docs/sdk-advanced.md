# SDK executors and translators

This guide covers custom provider executors, protocol translators, and model registration for an embedded CLIProxyAPI
service. The SDK uses Go 1.27 and the `/v6` module path.

`examples/custom-provider/main.go` is the buildable reference implementation. The excerpts below focus on individual
contracts and omit surrounding package and error-handling code.

Existing SDK callers can keep `openai.OpenAIResponsesAPIHandler` and `openai.NewOpenAIResponsesAPIHandler`.
They use the current `ResponsesAPIHandler` and `NewResponsesAPIHandler` implementation. Responses streams deliver
complete
SSE events; an unfinished event at stream closure returns an error.

Image endpoints return HTTP 400 for unsupported models even with a zero-value `APIHandler`.
Use `NewAPIHandler(base)` to execute supported image models; a missing handler configuration returns HTTP 500.

`disable-image-generation` accepts `off`, `all`, `chat`, and `passthrough`, plus legacy boolean and `on`/`yes`/`no`
inputs.
JSON encodes `off` as `false` and `all` as `true`; the extended modes remain strings.
`all` closes image endpoints, while `chat` removes image tools and their forced choice outside image endpoints.
`passthrough` preserves supplied image tools and skips Codex's automatic image-tool insertion.
Executors filter tools before applying payload rules, so explicit payload overrides determine the final request tools.

## Provider executor contract

Register an implementation of `sdk/cliproxy/auth.ProviderExecutor` for each custom provider key. The interface currently
contains these methods:

```text
type Executor struct{}

func (Executor) Identifier() string
func (Executor) Execute(
    context.Context,
    *coreauth.Auth,
    clipexec.Request,
    clipexec.Options,
) (clipexec.Response, error)
func (Executor) ExecuteStream(
    context.Context,
    *coreauth.Auth,
    clipexec.Request,
    clipexec.Options,
) (*clipexec.StreamResult, error)
func (Executor) Refresh(context.Context, *coreauth.Auth) (*coreauth.Auth, error)
func (Executor) CountTokens(
    context.Context,
    *coreauth.Auth,
    clipexec.Request,
    clipexec.Options,
) (clipexec.Response, error)
func (Executor) HttpRequest(
    context.Context,
    *coreauth.Auth,
    *http.Request,
) (*http.Response, error)
```

`Identifier` returns the provider key used by auth records. `Execute` returns the complete provider payload.
`ExecuteStream` returns response headers and a channel of `StreamChunk` values. A stream producer reports terminal
errors
through `StreamChunk.Err`, marks an explicit completion boundary with `StreamChunk.Terminal`, and closes the channel.

`HttpRequest` executes raw provider requests. Its caller owns any non-nil response body and closes it. `CountTokens`
should
return a request-scoped error when the provider has no token-count endpoint, so the auth manager does not treat that
capability gap as a credential failure.

When `store=false` leaves a historical item unavailable, the caller receives the original 404 and body. The credential
and
model remain available across ordinary, count and streaming requests. Failure statistics still record each actual
attempt.

A credential's `request_retry` applies to eligible requests. Legacy cooldown records containing 400, 401, 402 or 404
errors
retain their retry restrictions; explicit retry authority follows the recorded phase and outcome. Unknown 500 or 502
outcomes end the current call with the original error. A later independent request checks current availability again.
Credential preparation uses the same retry authority: a permitted failure before dispatch can select the next
credential,
while `no_retry` ends the current request. A Windows socket-read failure or unexpected EOF after executor dispatch has
an
unknown outcome and stops replay; a later independent request can still use the available credential.

A request-scoped error retains the executor's declared processing outcome. When Codex reports an incomplete response
after
HTTP 200, the call keeps the original timeout error with `unknown` and `no_retry`. Received partial output remains
available.

The global retry default is captured when a request starts; a changed default applies to later requests. Each
credential's
`request_retry` limits its extra eligible rounds, while `max-retry-credentials` limits credentials tried within each
round.
Credentials skipped by that cap still age with the current round. A temporary Home cooldown before any credential is
selected in the current round preserves that round.
With cooling enabled, an explicit HTTP 429 `Retry-After` below 10 s is raised to 10 s. A credential already tried in
that
round also waits at least 10 s before retrying, even if its deadline expired while other credentials ran. Disabling
cooling
preserves immediate retry eligibility for expired credentials. An untried credential with an expired cooldown is ready
immediately.

Non-streaming Chat and Completions requests send blank lines at the interval in `nonstream-keepalive-interval` while
waiting for execution; zero disables them. The handler stops the heartbeat writer before writing the final success or
error body, including after cancellation.

Chat and Completions streams send `[DONE]` after every observed choice has a non-empty `finish_reason`.
A role-only choice still needs its own finish reason. Null or empty values keep a choice unfinished; closing the stream
with an unfinished choice returns a truncation error. Usage-only chunks after completion remain in the response.
Chat, Completions, Responses, and Claude return a JSON error with the upstream HTTP status and safe `Retry-After`
when an error arrives before any response bytes are sent. After a heartbeat or payload is sent, errors use that
protocol's
streaming error format. Closing the payload channel preserves a pending error and its response headers.
The Claude error object retains additional upstream fields, including reset times, nested data, and integer values.

Chunk consumers call `StreamChunk.Acknowledge(nil)` after writing and flushing the corresponding chunk successfully,
or pass the actual delivery error. Successful terminal acknowledgement permits the producer to save replay state and
publish successful usage.

Caller cancellation releases in-flight quota while the manager waits for upstream chunks. Streams with per-chunk
acknowledgements record a request-scoped delivery failure and close output. Legacy streams wait up to 1 s for the source
to close or return its terminal error, then close output. The source error retains its execution-result callback;
cancellation alone publishes no execution result. Request cancellation leaves credential availability unchanged.

Byte-stream consumers can retain the whole-stream API in `sdk/cliproxy/usage`: `WithStreamDelivery` returns a delivery
context and a finish function. Call finish on every exit; its first result wins. Producers close their output before
`WaitStreamDelivery`, which keeps waiting for the receipt after request cancellation. `SupportStreamDelivery` registers
one
active producer and returns an idempotent release function; `StreamDeliverySupported` stays true until the last producer
releases it. Internal model and protocol executions isolate the caller's delivery ownership while retaining
cancellation.

An accepted plugin-stream emission keeps its successful result if its call is canceled or the stream closes afterward.
Emissions waiting to be received remain cancellable. Stream cancellation drops undelivered chunks; normal closure drains
accepted chunks in order. Client delivery continues to use the acknowledgement after write and flush.

An executor may also implement `coreauth.RequestPreparer`:

```text
func (Executor) PrepareRequest(request *http.Request, auth *coreauth.Auth) error
```

Use it to inject provider credentials into a raw request. Do not mutate shared transports or auth records from this
method.

## Authentication errors

`coreauth.Error` has four public fields in order: `Code`, `Message`, `Retryable`, and `HTTPStatus`.
Existing positional literals remain valid:

```go
legacy := coreauth.Error{"legacy", "original SDK message", true, 429}
```

Use `WithExecutionDisposition(disposition, generation)` on an `*Error` to attach dispatch phase, outcome, retry
authority,
and effect scope. `ExplicitExecutionDisposition()` reads those explicit values; `ExecutionDisposition()` also applies
the
HTTP-status classification when no outcome was supplied. `ExecutionStateGeneration()` reads the attached generation.
`ErrorCodeTransientTransport` identifies a transport failure; dispatch phase and outcome determine retry eligibility.

| Execution value   | Read                                                | Write through `WithExecutionDisposition` |
|-------------------|-----------------------------------------------------|------------------------------------------|
| `DispatchPhase`   | `err.ExplicitExecutionDisposition().DispatchPhase`  | `disposition.DispatchPhase`              |
| `Outcome`         | `err.ExplicitExecutionDisposition().Outcome`        | `disposition.Outcome`                    |
| `RetryDirective`  | `err.ExplicitExecutionDisposition().RetryDirective` | `disposition.RetryDirective`             |
| `EffectScope`     | `err.ExplicitExecutionDisposition().EffectScope`    | `disposition.EffectScope`                |
| `EffectKey`       | `err.ExplicitExecutionDisposition().EffectKey`      | `disposition.EffectKey`                  |
| `StateGeneration` | `err.ExecutionStateGeneration()`                    | The `generation` argument                |

For a single-field update, read the explicit disposition, edit that field, and pass it back with the current generation.
The setter replaces all execution fields with the supplied values, including zero values.

Use `err.Clone()` to copy all public fields, execution state, and the runtime cause. A plain value copy such as
`copied := *err` copies the four public fields. Pass an `*Error` to `json.Marshal` to include execution fields;
marshaling an `Error` value writes the four-field form. Decoding a partial object updates only supplied fields, and
JSON `null` preserves the existing error. Runtime causes stay in the error chain and are omitted from JSON.

Unavailable-auth errors expose `Headers()` and `WithAuthError(base)` through their returned error value.
`Headers()` returns an independent header map with the locally calculated `Retry-After`, rounded up to whole seconds.
`WithAuthError` retains that wait and the original cause while replacing the auth error details. Wrappers sharing the
same base retain their `errors.Is` identity. Use `IsTerminalAuthError` to detect a credential that needs authentication
again.

## Request metadata

`Options.EnsureMetadata()` initializes a nil `Metadata` map and returns it. Existing entries and explicit zero values
stay
unchanged. Set request-local metadata before passing the options to an executor:

```go
opts := clipexec.Options{}
opts.EnsureMetadata()["custom_route"] = "primary"
```

The helper returns the existing map without copying it. Use a separate options/map for requests that need independent
metadata

After-auth callbacks receive independent request containers. When a callback returns `Handled=true`, the manager copies
its selected request and options again before execution:

| Fields                                       | Copy behavior                     | Ownership after migration                                                                        |
|----------------------------------------------|-----------------------------------|--------------------------------------------------------------------------------------------------|
| `Request.Payload`, `Options.OriginalRequest` | Independent byte buffers          | Callback edits preserve the caller's input; later callback edits preserve the selected execution |
| `Options.Headers`, `Options.Query`           | Independent maps and value slices | Each request owns its header and query edits                                                     |
| `Request.Metadata`, `Options.Metadata`       | Independent top-level maps        | Nested extension values retain their existing ownership                                          |

`$CPA-SESSION-ID` uses the current request's session value. An explicit `util.WithSessionID` value takes precedence; an
explicit empty value omits the dynamic header. Legacy calls supplying only logging metadata retain its `SessionID`.
Direct header-tool callers can initialize `util.SessionIDResolver` before handling requests; the callback receives the
current context and caller headers.

To clear an inferred session after authentication, remove its header and session metadata. An explicit context value
remains authoritative. Ordinary, token-count and streaming calls share these rules with session affinity enabled or
disabled. The selected credential continues to supply protected headers such as `Authorization`.

`ExtractSessionID(headers, body, metadata)` returns the public v6 session value with its original source priority.
Use `CanonicalSessionID` for internal reporting and routing that need a source-qualified identity. Keep the two values
separate:

| Input or field                                                        | `ExtractSessionID` result                          | Canonical consumer                                                         |
|-----------------------------------------------------------------------|----------------------------------------------------|----------------------------------------------------------------------------|
| `Session-Id: s` or `Session_id: s`                                    | `s`                                                | `codex:s`                                                                  |
| `X-Session-ID: s`                                                     | `s`                                                | `header:s`                                                                 |
| A body with both `prompt_cache_key: cache` and `session_id: session`  | `cache`                                            | `session:session`                                                          |
| `session_id` in Claude JSON `metadata.user_id`                        | Original `claude:` representation                  | Separate session and agent identities                                      |
| `CanonicalSessionIDMetadataKey` and `LCPAffinitySessionIDMetadataKey` | Original public sources and content-derived result | Read these fields before content fallback when no explicit identity exists |
| Session and agent fields in a nested `request`                        | Original public source rules                       | Read the nested identity                                                   |

The public value for `ExecutionSessionMetadataKey` retains `execution:`. The `user:`, `derived:` and content-derived
representations also stay unchanged. Literal prefixes inside an opaque session value remain intact. A valid 256-byte
raw session value remains complete; the canonical identity is independently bounded to 256 bytes while preserving
long-value uniqueness.

Codex derives prompt-cache and replay identities after the final payload is ready. Replay records are isolated by
service,
credential, caller, session and final model. A token refresh for the same credential retains that identity; a different
credential or caller reads separate records. Built-in executors pass the request context to Required reads and deletes,
then append a turn after successful terminal delivery. Native Responses requests remain independent. The cache retains
its sliding one-hour lifetime and independent byte copies; read and cleanup errors remain visible.

`Result.AuthGeneration`, `Result.StateGeneration` and `Result.ModelQuotaEpoch` bind an execution result to the
credential
and state observed by the manager. Update managed credentials through typed mutation methods and their expected
generation;
the manager rejects state updates from replaced credentials or older state. Request proxy settings apply to execution
while
credential refresh keeps its own transport policy

Responses WebSocket identifies a downstream session using `X-Client-Request-Id`, the `session_id` in
`X-Codex-Turn-Metadata`, `Session-Id`, then `Session_id`, in that order. Requests with `previous_response_id` retain
complete
`function_call` and `custom_tool_call` items. Tool caches commit after successful response delivery; failed requests
retain
the previous turn's cache. The last connection for that session clears the caches when it ends. Merged history and
cached
values use independent byte buffers.

Error frames preserve JSON number values and notation, including large integers, decimals and exponents, in the error
node and its nested extension fields.

Responses streams rebuild a missing or empty final `response.output` from accepted `response.output_item.done` events.
A non-empty upstream output remains authoritative. After streaming begins, internal failures produce a generic
`Bad Gateway` event for standard and Codex clients; actionable request errors retain their sanitized details.
WebSocket failures use the same policy before the connection closes.

## Home dispatch and subscriptions

`RPopAuth` and `RPopAuthWithPolicy` preserve the supplied `count` and omit `retry_round`. `RPopAuthWithConstraints`
requests
one credential and sends an explicit retry round starting at zero. Nil `ExcludedAuthIDs` omits the exclusion field;
an explicit empty slice sends `excluded_auth_ids: []`.

A config subscription becomes ready after a valid ACK and a fresh command connection probe. ACK validation checks the
kind, channel and cumulative count for each subscribed channel. Context cancellation closes pending subscription reads
and returns `context.Canceled`.

Service shutdown withdraws Home dispatch, cancels active executions, waits for Home to acknowledge concurrency releases,
and closes the client. Ordinary, token-count and streaming executions release their owned resources once on completion
or cancellation.

After an upstream stream error, the manager drains the remaining source events until the source closes, the request is
canceled, or one second has elapsed. It then finishes the streaming call; downstream receives the first error and the
preceding content. An executor that returns no stream source causes `Manager.ExecuteStream` to return an error directly.

Home selects the credential and upstream model. If a post-auth interceptor changes the target, the manager resolves the
final model's capabilities again, including its `[1m]` support. Home retries use the error's processing outcome and
explicit
retry directive. An unknown outcome ends the call, preserving the original error. A rejected request can select another
credential when its retry directive permits it; redispatch waits for Home to acknowledge the previous release.

For a pinned credential, its own `request_retry` and Home's aggregate budget both bound retry rounds; the lower value
applies.
An already-tried pinned credential ends the current round locally. A new round starts with fresh round-local exclusions.
Legacy `RPopAuth` dispatchers can retry an explicit safe rejection within the configured budget. A dispatcher that
declares
constraint support and returns an excluded credential ends the request.
Reaching `max-retry-credentials` ends that round and immediately requests the next one when retry authority and budget
permit it.

When a Home round ends, its error retains the earliest eligible wait reported by attempted credentials. A later Home
cooldown
supplies the authoritative wait instead. Ordinary, count and streaming errors expose that wait through
`SafeResponseHeaders`,
rounded up to whole seconds; stream bootstrap errors retain it in the response headers as well.

For SDK requests with an `AuthSelectionModelMetadataKey` value different from `Request.Model`, Home receives the
selection
model while the executor keeps `Request.Model`. When Home enables force mapping, `RequestedModelMetadataKey` supplies
the
client-visible response model, including its prefix and reasoning suffix. An absent requested-model value uses
`Request.Model`.
Without force mapping, the provider's response model remains unchanged.

An upstream 401 ends the current call with the original error. `RefreshHomeSelectionAfterUnauthorized` is a read-only
snapshot query: its boolean result reports whether the same enabled credential already has a different non-empty token
installed in the selection. Home errors `auth_not_found`, `auth_unavailable`, `refresh_temporarily_unavailable` and
`home_unavailable` return HTTP 503 with their error code and message preserved.

An error after the stream starts keeps the original failure and ends its Home selection when the source closes,
the request is canceled or the existing drain deadline expires. A successful WebSocket stream keeps its retained
selection
available to the same session.

Query credentials from `key` or `auth_token` reach Home as `X-Goog-Api-Key` when the request has no credential header.
Existing `Authorization`, `X-Goog-Api-Key` and `X-Api-Key` headers keep their values. Query-source authentication
metadata
follows the same rule. A `user_api_key` returned with a Home selection updates that request's API-key identity for
subsequent handling.

Publish a Home client and its execution registry together with `PublishHomeDispatch(client, registry, generation)`.
Keep the returned bundle as the lifetime handle. `HomeDispatchBundle()` reads the active bundle, and
`ClearHomeDispatchBundle(bundle)` removes it only while that bundle remains active.

Read a Home session's credential with `GetExecutionSessionAuthByID(sessionID, authID)`. The active selection owns this
snapshot and removes it when it ends. WebSocket calls for the same session are serialized, and the lock entry is
reclaimed
after the last caller finishes. `CloseExecutionSession(sessionID)` ends that session's selections while other sessions
remain active; `CloseExecutionSession(CloseAllExecutionSessionsID)` ends all sessions. Repeated closure releases each
resource and concurrency admission once.

Home infers the LCP provider from `SourceFormat` without regard to letter case, so canonical and mixed-case names keep
the
same session binding. A shared parent namespace must be non-empty. Valid session IDs that begin with `:` retain their
identity and separate execution-session credentials. Prompt-cache and conversation aliases share the configured affinity
TTL; changing that TTL clears the alias cache. The cache has a soft limit of 4096 alias entries and evicts whole groups.

`HomeDispatchSelection.CloneAuth()` returns an independent credential copy. `ReplaceAuth(auth)` updates a standalone or
manager-owned selection and retains these routing attributes:

| `Auth.Attributes` key | Meaning                                    | Replacement rule           |
|-----------------------|--------------------------------------------|----------------------------|
| `home_upstream_model` | Home-selected upstream model               | Keep the selection's value |
| `home_force_mapping`  | Whether the response uses the client alias | Keep the selection's value |
| `home_original_alias` | Original client-visible model name         | Keep the selection's value |

A successful manager-owned runtime refresh updates the active selection and its session snapshot together. A stale
owner,
changed executor or rejected final commit leaves both credentials unchanged.

Codex and xAI retain the optional `UpstreamDisconnectChan` subscription for the session's first disconnect. An idle
upstream
disconnect ends the old Home selection and releases its concurrency. The downstream connection stays available, and the
next
request selects a credential through Home again.

## Request-scoped Home KV

Inside an executor, call `coreauth.HomeKVClientFromContext(ctx)` with the context supplied by the manager. The returned
`HomeKVClient` provides GET, SET, DEL, EXPIRE, TTL, INCRBY, MGET and MSET. Implementations use the public
`coreauth.HomeKVSetOptions` type for SET options.

An explicit local scope returns `homeMode=false`. An enabled Home that is disconnected or lacks the common operations
returns an error. Preserve that error in request preparation. Contexts without an explicit scope retain the
current-client
compatibility path.

`KVTTL` returns duration, found and error; a missing key returns `found=false`, and a persistent key returns zero
duration
with `found=true`. `KVMGet` preserves key order and distinguishes a missing value from an empty value through its found
flags. `KVMSet` writes the supplied pairs in one command; INCRBY preserves signed 64-bit integers.

Built-in thinking-signature caches use the same request scope. Home read or expiry failures return a request-scoped
HTTP 503 before provider dispatch; the credential stays available. Completed responses log cache-write failures without
changing the delivered response. Local signature caches stay separate for each service.

When credits are enabled, Antigravity warm-up queries read the current Home's hints and existing balances first.
SET NX coordinates a 10-minute query interval, and results expire after 30 minutes.

Credits queries have a five-second limit. Warm-up queries use an independent budget; post-token-refresh queries follow
caller cancellation and can retry immediately after cancellation. A new token or newer credential generation cancels the
previous query. Older generations and canceled results cannot publish observations.

Read credits observations with `GetAntigravityCreditsHintRequired(ctx, authID)`, which returns hint, found and error.
A local scope reads the process hint; a Home scope reads the current Home observation. Known available, known
unavailable
and unknown states stay distinct. Read failures return the original error; an empty identity returns an empty hint and
`found=false`.

Custom executors publish current credential observations with
`auth.PublishAntigravityCreditsHintWithContext(ctx, hint)`.
Home publication uses CAS to keep newer observations; unsupported CAS or failed writes are logged and return false.
The original `PublishAntigravityCreditsHint(hint)` keeps its local publication behavior.

Antigravity generation and streaming requests read Home short cooldowns, return HTTP 429 with the remaining wait while
active, and delete expired keys. KV read, write or delete failures return a request-scoped HTTP 503 with the original
cause.
Token counts keep their independent endpoint. Disabled cooling and explicit credits requests bypass generation
cooldowns.

With retries available, each same-credential wait is limited to `500 ms`. Terminal rate-limit responses with a longer
delay
below five minutes write a Home short cooldown. The manager handles the remaining scheduling.

## Register the executor

Create the manager from the SDK token store, register the executor, and pass the manager to the builder:

```text
tokenStore := sdkauth.GetTokenStore()
if setter, ok := tokenStore.(interface{ SetBaseDir(string) }); ok {
    setter.SetBaseDir(cfg.AuthDir)
}
core := coreauth.NewManager(tokenStore, nil, nil)
core.RegisterExecutor(Executor{})

service, err := cliproxy.NewBuilder().
    WithConfig(cfg).
    WithConfigPath(configPath).
    WithCoreAuthManager(core).
    Build()
```

Auth records whose effective provider key matches `Identifier()` route to this executor. `RegisterExecutor` replaces the
current executor for that key. Generation-aware registration methods are available when a plugin lifecycle must reject a
stale replacement.

For a record with `compat_name`, the direct SDK uses a non-empty `provider_key` as the executor key after trimming and
lowercasing it. An empty key uses `compat_name`. Request execution, request refresh and automatic refresh use that same
key.

| Field                        | Direct SDK registration                                                        | Service configuration publication                                                                |
|------------------------------|--------------------------------------------------------------------------------|--------------------------------------------------------------------------------------------------|
| `Auth.Provider`              | Keep the logical provider, including a plugin provider                         | Configuration credentials use the namespaced key; plugin credentials keep their logical provider |
| `Attributes["provider_key"]` | Match the registered `Identifier()`, including an explicit raw or prefixed key | Normalize through `OpenAICompatibleProviderKey`, such as `custom` to `openai-compatible-custom`  |
| `Attributes["compat_name"]`  | Supply the key when `provider_key` is empty                                    | Keep the original channel name                                                                   |

When adapting an upstream record with `provider_key=custom` and an executor named `openai-compatible-custom`, use
Service publication or set the SDK record's key to `openai-compatible-custom`. Direct registration also supports the raw
pair `custom` / `custom`. Credential IDs, base URLs and token metadata retain their meanings.

Service publication maps native `kimi.com` credentials to `kimi` and native `kimi.ai` credentials to `kimi-ai`. Each
site keeps its own executor and credentials. A configured compatibility channel with either site name uses its own
`openai-compatible-` key.

`Builder.WithWatcherFactory` accepts the published `WatcherFactory` callback, `reload func(*config.Config)`.
`Build` retains the factory selected at that call. The built-in watcher still uses acceptance results and file-version
acknowledgement; a rejected update retains the previous configuration and file acknowledgement.

## Credential identity and routing

Use `routing.session-affinity` for session affinity. Reading a configuration with `routing.claude-code-session-affinity`
migrates its value when the new key is absent. An explicit new value, including `false`, takes precedence. Reading
leaves the file unchanged; the next successful save keeps only the new key in the effective routing configuration and
preserves other fields and comments.

Programmatic configuration uses `cfg.Routing.SessionAffinity`; `ClaudeCodeSessionAffinity` is retired. File saves,
management YAML saves, and the visual editor use this migration rule. The migration log asks the operator to save the
configuration.

Management lookups accept legacy API-key index references for Gemini, Claude, Codex and OpenAI-compatible credentials.
Responses retain the current source-aware index. Use that current index when several credentials share a legacy
reference;
auth-file requests can also specify the credential's exact ID in `name`. An ambiguous legacy-only mutation returns an
error.
`Auth.MatchesIndex` checks current and supported legacy references without assigning or replacing an index. Explicit
indexes
and seeds keep their selected identity. Concurrent index queries and recent-request snapshots preserve identity and
counters;
clones retain independent request histories.

External callers can assign `Auth.Index` directly or use `AssignIndex`. `EnsureIndex`, registration, cloning and
management
lookups retain that index. Usage export and restore preserve the original `auth_index` reference.

`Auth.AuthKind()` checks `Attributes["auth_kind"]`, then `Metadata["auth_kind"]`, then legacy field-shape inference. An
OAuth
credential can retain a historical `api_key`: Codex uses the current `access_token`, OAuth account headers and OAuth
model
rules. Model routing, request rules and automatic refresh use that credential kind. Email remains display information
for that explicitly classified credential.

API-key credentials keep their existing key fields and model configuration. If an explicit OAuth credential has no
`access_token`, its authorization header stays empty. Callers use the existing refresh or error handling path to recover
the credential; the historical `api_key` stays in the record.

Kimi's Claude-compatible entry points normalize upstream model names such as `kimi-k3` to `k3`. Ordinary and streaming
responses retain the caller's requested model name. The usage record's `ResponseModel` keeps the model returned by the
upstream.

When Claude removes incompatible historical signatures, debug logs include the request ID, target model, removal or
replacement counts, and the first decision reason.

With `claude-header-defaults.stabilize-device-profile` false or unset, supplied OS/architecture headers retain their
values;
missing values use the proxy's runtime platform. This applies to caller-owned, CLI and OAuth requests.
Setting it to true pins the platform for a selected CLI or OAuth profile to the configured baseline.
Caller-owned User-Agent and authentication stay unchanged; `fingerprint-profile` continues to select the CLI identity.

Antigravity restores missing Claude thinking signatures from the request's cache before validating them for the target
provider. Generation, streaming and token-count entry points use the same order. When incompatible thinking blocks are
removed, debug logs include the request ID, target model, cleanup stage and reason, and the counts before and after
cleanup.
The summary omits payload text and raw signatures.

Credentials carrying `config_index` bind to that entry. An invalid index or a credential identity mismatch leaves the
model configuration unbound. Without an index, native API-key credentials match by key and base URL, preferring an entry
whose prefix and proxy match `Auth.Prefix` and `Auth.ProxyURL`. Selection then uses the first credential match, followed
by the existing key-only match. OpenAI-compatible routes without an index match by provider name; Home uses the selected
credential's options.

Request rules read credential metadata before provider configuration. Explicit OAuth credentials use their provider's
OAuth rules. API-key credentials can bind rules with `config_index` alone; a supplied key or base URL still validates
that identity. Legacy `openai-compatible-custom` and `openai-compatibility:custom` identities resolve the `custom`
channel's rules.

`stop-and-cooldown` and `continue-and-cooldown` retain the resolved model or credential scope. For transient errors, an
explicit cooling action preserves a positive `Retry-After` even when `transient-error-cooldown-seconds` is negative.
Without a hint, it uses the configured positive interval or the 60 s default. Ordinary transient cooling stays disabled
for a negative setting, and an existing later deadline is preserved.

A TPM rate limit without a valid `Retry-After` retains the one-minute delay. Valid retry headers, including those on
HTTP 503, take precedence. Errors retain their upstream status and body.

Model cooldown errors keep the requested model. The upstream cause remains in the error chain; model causes take
priority over credential causes, and the newest record wins within the same level. For a positive waiting deadline,
`SafeResponseHeaders(err)` supplies locally calculated `Retry-After`. Error text uses safe diagnostics.

Setting a model's `force-mapping` to `true` rewrites ordinary and streaming response model fields to its configured
`alias`.
The upstream `name` retains the request's thinking suffix and `[1m]` decoration. With `false`, response model fields
retain
their original values.

Several upstream names on one API-key credential can share an alias. Local requests rotate across available targets and
skip blocked targets before execution. Explicit model or quota rejections can select another target under the existing
retry authority; unknown outcomes and empty streams end the call. Each attempt uses its actual target's response
mapping,
capabilities and state, including a target changed after authentication. Successful counts preserve availability;
model rejections update the actual target's state. Home uses the remotely selected target.

OAuth aliases resolve to the selected credential's actual target. Selection, capabilities, cooling and execution results
use that target; the executor's context retains the requested alias. For direct result reporting, set `Result.Model` to
the
actual target and `Result.AuthGeneration` to the selected snapshot's `Generation`.

When the manager uses `SessionAffinitySelector`, explicit sessions and LCP validate their bound credential against the
actual target. Target recovery keeps the current healthy binding; new sessions follow configured priority. If every
target is cooling, selection returns the waiting error without calling an executor. Direct
`SessionAffinitySelector.Pick` calls retain the highest-available-priority rule.

After re-registering a model catalog, call `RefreshSchedulerEntry(authID)` to publish current model state. Projections
from
an old registration epoch or publication generation are rejected. Removing or repointing an alias uses the new catalog
for
selection while retaining the original model's independent cooldown and reset authority. A canceled `ResetQuota` or
failed
auth-store save returns the original error and retains published state.

`coreauth.QuotaCooldownDisabledForAuthWithConfig(auth, cfg)` reads the shared cooling policy. An auth override,
including
an explicit `false`, takes precedence over provider and global settings. Antigravity uses this policy for automatic
credits refresh and credits-disable hints as well as credential scheduling.

Explicit credits execution is enabled separately by `QuotaExceeded.AntigravityCredits` and remains available when
cooling
is disabled. Before ordinary or streaming credits execution, the manager prepares the selected credential and preserves
the original request body and metadata. Preparation failures that allow another credential can continue before anything
is sent; cancellation, unknown outcomes and `no_retry` stop the call with the original error. Home mode uses its own
selection authority. An unavailable Home returns HTTP 503 before local execution.

`Manager.SetAuthAffinity(key, authID)` sets a local credential binding, `AuthAffinity(key)` reads it, and
`ClearAuthAffinity(key)` removes it. The request option `Metadata["auth_affinity_key"]` accepts a string or byte slice;
explicit pinned-credential metadata takes priority. Successful local execution or stream setup remembers the current
credential. Credential removal or replacement clears its bindings. Home dispatch retains selection authority.

Passing zero or a negative TTL to `coreauth.NewSessionCache` or `coreauth.NewSessionCacheWithCapacity` selects 30
minutes.
Positive TTLs retain their value. The built-in session-affinity selector keeps its one-hour default.

## Saving credentials and publishing fields

Use `Register` or `Update` for ordinary credential changes. Use `MergePersistentUpdate(ctx, expected, next)` for
prepared persistent fields. A successful save publishes the saved fields and the manager-assigned generation to the
return value, manager, scheduler and hooks. If the expected persistent state has changed, `MergePersistentUpdate`
returns a nil record; read the latest snapshot before deciding whether to submit another change. If `Register` or
`Update` fails to save, it returns the storage error and keeps the published credential.

`Register`, `Update`, `Load` and `RestoreCooldownStates` fold thinking-suffix model states into the base model name.
Higher state generations take precedence; within one generation, the later update wins. Quotas retain their independent
generation, deadline and reset authority. Restored states publish their merged model catalog and scheduler state. Model
entries containing only historical backoff counts remain without creating a cooldown; disabled credentials stay
unavailable for selection.

Use `PrepareRequestAuth(ctx, executor, selected)` to run the executor's `RequestAuthPreparer` before sending a request.
The manager merges preparation results with the current record and preserves its runtime restrictions. A concurrent
credential change makes preparation retry against the latest record. `ForceRefreshAuth` runs the provider's refresh
exchange and reports its outcome through the safe summary described below. Cancellation while waiting returns the
original
context error and releases the preparation, refresh and mutation locks.

Home prepares the selected ephemeral credential for ordinary, token-counting and streaming requests. Its metadata and
execution results stay separate from a local credential with the same ID, including the local scheduler, model catalog
and persistent record. Local preparation saves missing metadata once and reuses it on later requests. A preparation
failure with HTTP 403 applies the credential cooldown before the executor sends the request.

For an external file or database mutation, call `MutatePersistentAuth` with a context from
`WithExpectedAuthGeneration(ctx, expected.Generation)`. Its callback returns the credential state it has persisted. A
generation mismatch returns `ErrAuthGenerationConflict` before the callback runs. A callback error is returned while the
published credential fields remain unchanged.

For login, import, or a direct Store `Save`/`SaveWithBaseDir` call, the caller owns the unpublished Storage. Existing
`SetMetadata`/`SaveTokenToFile` implementations remain usable, including provider-generated fields and original errors.

Manager persistence requires `CloneForPersistence` for published Storage. The implementation owns the copy of all
private state changed by its setter or saver, including mutable state captured by closures. Missing copy support fails
before the Store is called. A typed-nil copy or a pointer-based copy that reuses the original mutable instance fails
before its setter is called.

Persistence preparation copies ordinary metadata containers and retains numeric types and the ownership of opaque
extension values. A save failure preserves published Storage and metadata. Already-issued replacement tokens follow the
durable-state rule below. Credential content merges keep new tokens authoritative, preserve concurrent user settings,
and use an explicit empty metadata map to delete unchanged base keys while retaining newly added keys.

The built-in file, Git, object, and PostgreSQL Stores call `RecordPersistedAuthVersion(ctx, path)` while holding their
save lock. Manager uses that actual file version for conditional deletion. Custom Stores retain the original `Save`
contract; file-backed implementations can also report their saved version with this function. A permissions or
directory-sync failure after live-file replacement returns `ErrAuthDurableStateUncertain` with the original cause. A
failure confined to a provider's staging file retains its original error.

If a provider has already issued refreshed tokens, a save failure returns `ErrAuthDurableStateUncertain` with the
storage
error. The manager retains the issued material and retries its persistence. `Load` reports that uncertainty while the
current refresh still has an active unsaved result.

### Manual refresh results

`Manager.ForceRefreshAuth(ctx, id)` returns `(ForceRefreshResult, error)`. The result contains the five public fields
below.
A failed refresh returns the available identity summary with the original error. A nil context uses
`context.Background()`.
Read complete credential metadata and model state through `GetByID` or `List`; edits to those independent snapshots
leave
the manager's stored state unchanged.

The provider refresh exchange runs outside the credential mutation lock. Saving and publishing the result serialize with
`Update`. If an update replaces the credential during the exchange, the refresh returns a safe summary and a stale
result
error. Concurrent edits to user settings merge with a valid refresh while keeping the user's fields.

Within the same credential identity, refreshing a rejected access token keeps it unavailable when only expiry or
refresh-token metadata changes. A replacement access token clears authentication failures; active quota restrictions
retain their deadlines and reset authority.

Recovery uses the recorded error classification and authority. A legacy `StatusError` or plain-text error without an
authentication classification retains its recorded error state after token refresh. Identified authentication failures
can
recover, while provider failures and model quotas keep their own deadlines and recovery rules. Model-scoped success
keeps
credential restrictions; credential-scoped success follows the observed state generation and reset authority.

Single and batch management HTTP responses use the same five-field summary on success and failure.

| HTTP field     | SDK source                                | Projection rule                                        |
|----------------|-------------------------------------------|--------------------------------------------------------|
| `filename`     | `Auth.FileName`, then `Auth.ID`           | Return the base filename                               |
| `provider`     | `Auth.Provider`                           | Trim surrounding whitespace                            |
| `auth_index`   | `Auth.Index`                              | Derive a missing index on an independent copy          |
| `status`       | `Auth.Status` and current rejection state | Keep disabled status; report a rejected token as error |
| `refreshed_at` | `Auth.LastRefreshedAt`                    | Keep the refresh completion time                       |

Batch refresh uses `POST /v0/management/auth-files/refresh` with `{"all":true}`. It selects enabled credentials with
refresh material or supported runtime capabilities and returns each credential's summary and result.
`auth-auto-refresh-workers` sets the worker limit; an empty, zero or negative value uses 16 workers.

Cancellation marks queued items as `credential_refresh_cancelled` and leaves their credentials unchanged. An exchange
that has already returned new credential material completes persistence with an independent bounded context, retains the
request's context values, and publishes its successful result. Independent quota restrictions retain their authority.

### Automatic refresh lifecycle

`Manager.StopAutoRefresh(ctx)` keeps the published `func(context.Context) error` signature. Pass nil for a default 5 s
wait, or pass a context to choose cancellation and deadline. A provider still running at the deadline returns the
context error together with `ErrAuthDurableStateUncertain`.

Restart waits up to 5 s for the old lifecycle, reports any provider still running, and creates independent workers for
the new lifecycle. Old results remain isolated from replacement credentials. Disabled credentials stay outside automatic
token refresh. Canceling queued work releases its pending marker and preserves provider cooldowns.

## Reading cooldown state and changing stores

Credential and model restrictions keep independent deadlines and backoff levels. A later failure preserves a longer
active deadline in the same scope. A model quota remains local when it arrives before or after a credential-wide
authorization failure. Scheduling and client model projections use the same effective restriction.

`CooldownSnapshotForAuth` lists active local waits with a deadline, rounds remaining seconds up and returns safe
classifications while preserving the credential. An authoritative restriction without a deadline still blocks selection
until its generation and reset authority permit recovery. An ordinary HTTP 500 with an unknown outcome ends the current
request and preserves the credential's current state.

New quota cooldowns use the same deadline in `NextRetryAfter` and `Quota.NextRecoverAt`, with Go's monotonic clock
reading
removed. Independent restrictions retain their own deadlines.

A rejected access token stays ineligible during refresh, after refresh failure, and after a refresh that returns the
same
token. Metadata-only updates retain that rejection. A different access token restores authorization while independent
model quotas keep their own deadlines. Canceling a refresh releases its pending job without creating a Provider
cooldown.
The manager keeps the rejected-token fingerprint private; rejection publication updates `UpdatedAt`, while `Generation`
continues to identify the credential version. `GetByID` and `List` expose `Unavailable=true` for a rejected token and
report
error status while preserving disabled status. The refresh summary reports that status. Metadata updates retain the
rejection view; a replacement token removes it.

### Credential versions and late results

Read the selected credential snapshot from `Register`, `GetByID`, `List` or `Update`. `Load` assigns a nonzero runtime
`Generation` to credentials read from storage. Keep that value with the request instead of assuming version 1 or 2.

| Upstream field                                         | Current public input                          | Current meaning                                                                                                                    |
|--------------------------------------------------------|-----------------------------------------------|------------------------------------------------------------------------------------------------------------------------------------|
| `Auth.CredentialVersion`                               | `Auth.Generation`                             | The manager advances the published snapshot version, including label-only updates                                                  |
| `Auth.RegistrationEpoch`                               | The newly registered `Auth.Generation`        | Keep a fresh snapshot after removal and re-registration; earlier registration results stay isolated                                |
| `Result.CredentialVersion`, `Result.RegistrationEpoch` | `Result.AuthGeneration = selected.Generation` | Direct `MarkResult` calls bind to the selected published snapshot; also retain the observed state generation and model quota epoch |

Manager executions keep their per-attempt credential binding. A label-only update retains that binding, so a late 429
can
still restrict the unchanged credential. Token or API-key replacement isolates old failures. Credential replacement
clears
earlier session bindings; a request using the current credential can establish a new binding, which late results
preserve.
An in-flight request keeps its original HTTP error even when that result leaves the replacement credential unchanged.

For a credential-wide quota result, replace the former `CredentialScope` flag with the following disposition fields:

| Field                         | Value                              | Meaning                                               |
|-------------------------------|------------------------------------|-------------------------------------------------------|
| `DispatchPhase`               | `response_started`                 | An upstream response has started                      |
| `Outcome`                     | `rejected_before_processing`       | The upstream rejected processing                      |
| `RetryDirective`              | `retry_after`                      | A retry follows the supplied wait and existing budget |
| `EffectScope` and `EffectKey` | `credential` and the credential ID | The restriction affects all models on that credential |

`AuthGeneration` identifies the selected credential version. `StateGeneration` identifies the observed state in the
result's scope; model results also carry the observed `ModelQuotaEpoch`. Later credential or state changes isolate older
results. Credential quota uses `Quota.Reason="quota"`; `StateEffectScope` identifies its credential-wide scope. Ordinary
401 failures use credential scope, 429 failures use quota-family scope, and unknown 500 outcomes keep Provider scope
with `no_retry`. A default exact-model 404 with an empty target leaves credential availability unchanged.
`invalid_grant` can replace a finite wait with a deadline-free restriction that requires reset authority.

Publish passive quota headers from a custom executor with `coreauth.RecordUpstreamResponseHeaders(ctx, headers)`, using
the context supplied for that attempt. Each attempt owns its header snapshot. Supported quota headers are limited to 64
entries and 512 bytes per value. Quota observations remain active when request logging is disabled. Count requests
retain the existing `Signals` and `ObservedAt`. Successful counts and missing independent count endpoints preserve
availability; other failures retain their existing state-effect classification. CDS stores filtered `Signals` and
`ObservedAt`; restoration retains a runtime observation with an equal or later timestamp.

`QuotaCooldownDisabledForAuth(auth)` reads the credential override and process setting. Calls with configuration use
`QuotaCooldownDisabledForAuthWithConfig(auth, cfg)`. Credential overrides take precedence over provider and global
configuration.

`HasUnauthorizedAuthFailure(auth)` reads a 401 or unauthorized error when status is StatusError, Unavailable is true,
and both waiting times are zero. `HasDisabledInvalidGrantFailure(auth)` reads an invalid_grant body or code on a
disabled credential. Both preserve the supplied record. An authoritative unavailable state with no deadline continues to
block selection until its reset authority permits recovery.

`Manager.SetConfigSnapshot(cfg)` publishes an independent configuration copy and updates runtime state. It returns true
when it clears cooldowns that need a later `PersistCooldownStates(ctx)` call. A copy failure is logged and preserves the
current configuration. The snapshot method completes in-memory publication before returning; persistence is a separate
call. `SetConfig` and combined configuration updates also publish independent copies, so later caller edits preserve the
published state.

`Manager.SwapCooldownStateStore(ctx, store, persistOld)` saves pending old state before publishing the replacement
store. Save errors and cancellation return false while retaining the current store and pending work. A later call with
an active context saves that pending state. With no pending state, `persistOld=false` replaces the store directly. A nil
context uses `context.Background()`.

Use `ApplyConfigWithCooldownStateStore(ctx, cfg, store)` to change configuration and its cooldown store together. A
failed old-store save preserves the current configuration. Cancellation after the old snapshot has been saved completes
the corresponding configuration and store publication.

## Batch credential alias updates

Keep existing batch calls by deriving a context with `coreauth.WithDeferredAPIKeyModelAliasRebuild(ctx)` and calling
`Manager.RefreshAPIKeyModelAlias()` once after all work completes:

```go
batchCtx := coreauth.WithDeferredAPIKeyModelAliasRebuild(ctx)
defer core.RefreshAPIKeyModelAlias()
for _, next := range auths {
    if _, err := core.Update(batchCtx, next); err != nil {
        return err
    }
}
```

| Input or entry point                                                  | Retained meaning                                                                                                           | Publication behavior                                                                   |
|-----------------------------------------------------------------------|----------------------------------------------------------------------------------------------------------------------------|----------------------------------------------------------------------------------------|
| `ctx`                                                                 | Cancellation, deadline and existing context values; a nil parent uses `context.Background()`                               | Credential mutations using the derived context defer alias-table rebuilds              |
| `Auth.ID`, `Attributes`, `Metadata`                                   | Existing credential identity, config selection and field meanings                                                          | Successful mutations still commit credential snapshots and scheduler state             |
| `Generation`                                                          | The generation returned in the snapshot; typed mutations and conditional deletion still validate their expected generation | Stale mutations remain rejected                                                        |
| `Register`, `Update`, `Remove`                                        | Existing entry points, results and error propagation                                                                       | Support deferred alias publication                                                     |
| `MutatePersistentAuth`, `MergePersistentUpdate`, `RemoveIfGeneration` | Existing persistence and conditional-commit contracts                                                                      | Honor the same context flag                                                            |
| `RefreshAPIKeyModelAlias()`                                           | Current committed credentials and latest config                                                                            | Atomically replaces the complete alias table; existing read snapshots stay independent |

Concurrent batches wait for every worker before the final rebuild. Failure or cancellation still publishes mutations
that have already committed and returns or logs the original errors. A batch does not become an all-or-nothing
transaction.

Persistence policy remains separate. Ordinary updates still save their state; watcher updates use `WithSkipPersist`
under
the existing already-persisted-data contract. Mutations with an ordinary context and `SetConfig` still rebuild
immediately.

## Read legacy provider key lists

Existing upstream key lists load through the current YAML decoder and config-file entry points:

| Legacy key                                                                                               | Decoded value                                                | Save behavior                                                                       |
|----------------------------------------------------------------------------------------------------------|--------------------------------------------------------------|-------------------------------------------------------------------------------------|
| `generative-language-api-key`                                                                            | String entries become `api-key` fields in `gemini-api-key`   | An explicit save writes the current field and removes the old key                   |
| `openai-compatibility[].api-keys`                                                                        | String entries become `api-key-entries` on the same provider | Keeps that provider's URL, models and headers; an explicit save removes the old key |
| `amp-upstream-url`, `amp-upstream-api-key`, `amp-restrict-management-to-localhost`, `amp-model-mappings` | Returns a migration diagnostic naming the retired field      | Remove the retired fields before saving                                             |

A present current field takes precedence, including explicit `[]` and `null`. Legacy lists trim surrounding whitespace
and remove empty and duplicate entries. Valid YAML aliases and merges still apply.

Compatibility decoding builds the current provider fields; an explicit save replaces the old keys. Invalid legacy types
produce field-specific errors without key values. Management validation and config-file loading use the same rules.

## Keep credential rotation when no plugin scheduler is active

After `Manager.SetPluginScheduler` installs a scheduler, the manager reads its optional `HasScheduler()` method for both
single-provider and mixed-provider selection. A false result uses the built-in selector and keeps the existing
credential rotation.

| Entry point or field                    | When it applies                | Behavior                                                                                                                                                              |
|-----------------------------------------|--------------------------------|-----------------------------------------------------------------------------------------------------------------------------------------------------------------------|
| `PluginScheduler.PickAuth(ctx, req)`    | Required scheduler entry point | Keeps `SchedulerPickRequest`, `SchedulerPickResponse` and error returns; `Provider`, `Providers`, `Model`, `Stream`, `Options` and `Candidates` retain their meanings |
| `HasScheduler() bool`                   | Optional scheduler method      | False skips `PickAuth`; true continues candidate filtering and scheduling. Existing schedulers that implement only `PickAuth` still participate                       |
| `SchedulerWantsAcrossPriorities() bool` | Optional scheduler method      | True allows selection across priority tiers; the default uses the highest available tier. Cooldown and disabled-state checks still apply                              |

Host capability queries use the current plugin snapshot. Reconfiguration, disabling or unloading prevents an old
snapshot from starting another scheduler call. After caller cancellation, an active native call keeps its shared-library
lease until it returns; unloading then completes.

## Per-auth transports

`Manager.SetRoundTripperProvider` installs a provider that returns an `http.RoundTripper` for each auth record:

```text
type roundTripperProvider struct{}

func (roundTripperProvider) RoundTripperFor(auth *coreauth.Auth) http.RoundTripper {
    return transportFor(auth.ProxyURL)
}
```

Executors should use the provided transport policy instead of modifying `http.DefaultTransport`. For raw request flows,
`Manager.InjectCredentials(request, authID)` invokes the selected executor's optional `RequestPreparer`.

Antigravity uses short-lived connections by default. Set `antigravity.connection-pool.enabled` to `true` to reuse
connections. Enabled pools default to a 30-second idle timeout and two idle connections per host; the respective limits
are 210 seconds and 100 idle connections. Pools stay separate by credential and proxy. A pool configuration change
removes cached pools and closes their idle connections; subsequent requests can create new pools.

## Translator registration

`sdk/translator.Register` stores a request transform from the inbound format to the provider format and response
transforms
for the reverse direction. Request transforms return `[]byte`; streaming response transforms return `[][]byte`;
non-stream
response transforms return `[]byte`.

```text
var (
    formatOpenAI = sdktr.Format("openai.chat")
    formatMyProv = sdktr.Format("myprov.chat")
)

sdktr.Register(
    formatOpenAI,
    formatMyProv,
    func(model string, raw []byte, stream bool) []byte {
        return convertOpenAIToMyProv(model, raw, stream)
    },
    sdktr.ResponseTransform{
        Stream: func(
            ctx context.Context,
            model string,
            originalRequest, translatedRequest, raw []byte,
            param *any,
        ) [][]byte {
            return convertStreamMyProvToOpenAI(
                model, originalRequest, translatedRequest, raw,
            )
        },
        NonStream: func(
            ctx context.Context,
            model string,
            originalRequest, translatedRequest, raw []byte,
            param *any,
        ) []byte {
            return convertMyProvToOpenAI(
                model, originalRequest, translatedRequest, raw,
            )
        },
    },
)
```

Registration direction matters. Request lookup uses `from → to`; response lookup uses the reverse provider-to-client
path. When no matching transform exists, the registry preserves the input payload and still applies registered plugin
hooks.

The v6 capability helpers `HasRequestTransformerByFormatName`, `HasResponseTransformerByFormatName`,
`HasStreamResponseTransformerByFormatName`, and `HasNonStreamResponseTransformerByFormatName` query the registered
transforms in the same registration direction. A token-count-only registration provides response capability without
providing streaming or non-streaming conversion.

A plugin response translator takes precedence over a native translator. If the plugin declines the response, the native
translator runs; its empty result consumes the input without forwarding the original payload. The registry releases its
lookup lock before invoking a converter, so a converter may register another transform.

Use `TranslateRequestChecked` or inspect `RequestEnvelope.Err` to handle rejected input. Chat-to-Claude conversion
matches
tool results to the preceding assistant call group by the original call ID, before ID normalization. Unmatched or
missing
IDs return a request-scoped HTTP 400 with an empty body. Adjacent assistant continuations and an intervening user
reminder
preserve valid pairing; the byte-only entry also returns an empty body for rejected input.

Responses-to-Gemini conversion keeps unsigned text and reasoning without adding `thoughtSignature`; empty unsigned parts
are omitted. Valid provider-compatible signatures retain their value, and function calls keep their separate signature
compatibility rules. Media data and explicit MIME types survive top-level input, message content and tool outputs;
unsupported file IDs and invalid or empty attachments return a checked error. Tool output strings that encode an array
or object containing supported media blocks deliver that media through `functionResponse.parts` with its tool result.
Other strings, including JSON text, `"null"` and the empty string, keep their original string value.

Devin native Interactions responses preserve custom-tool arguments as raw text in ordinary and streaming responses.
Responses requests retain the declared custom tool and its input through the existing function-style bridge; converted
streams emit custom input once the complete argument is available. A converted custom call without a matching
declaration
returns an error and preserves observed failure usage.

Devin encodes numeric `top_p` values from 0 through 1 in its Connect request, preserving explicit zero.
An explicit `presence_penalty`, `frequency_penalty` or `stop_sequences` returns a request-scoped HTTP 400 before
upstream dispatch.

Vertex requests sourced from Responses omit `functionCall.id` and `functionResponse.id` from the final payload,
including IDs introduced by payload rules. IDs inside argument and response objects retain their original values.

## Model registration

Register custom models under each auth ID so they appear in `/v1/models` and model lookup:

```text
models := []*cliproxy.ModelInfo{
    {
        ID:          "myprov-pro-1",
        Object:      "model",
        Type:        "myprov",
        DisplayName: "MyProv Pro 1",
    },
}
cliproxy.GlobalModelRegistry().RegisterClient(authID, "myprov", models)
```

The service registers built-in providers automatically. Custom executors should publish models after their auth records
are
available and replace that registration when the provider's inventory changes.

## Verification

Keep executor tests local by using an `httptest.Server` or a fake `http.RoundTripper`. Cover non-stream responses,
stream
terminal behavior, context cancellation, response-body ownership, credential injection, token-count errors, translator
direction, and model replacement. The repository's `examples/custom-provider` package is compiled by the release checks.
