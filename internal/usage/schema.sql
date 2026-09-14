-- Usage DB schema v9
-- All *_ns timestamps are UTC Unix nanoseconds.
-- cost_micro = USD * 1e6 (micro-dollars).
-- IF NOT EXISTS lets the same script run idempotently on every startup.
--
-- v2 adds the provider dimension so credentials shared across providers
-- (Claude / Codex / Gemini) no longer collapse onto a single bucket. Old
-- rows have provider_id NULL and surface as "(unknown)" — backfill from
-- v1/v2 JSON is impossible because the legacy payloads never carried the
-- provider name in the first place.

CREATE TABLE IF NOT EXISTS meta
(
    key
    TEXT
    PRIMARY
    KEY,
    value
    TEXT
    NOT
    NULL
) WITHOUT ROWID;

INSERT
OR IGNORE INTO meta(key, value) VALUES ('schema_version', '9');
INSERT
OR IGNORE INTO meta(key, value) VALUES ('api_key_reservation_clock_ns', '0');

CREATE TABLE IF NOT EXISTS dim_model
(
    id
    INTEGER
    PRIMARY
    KEY,
    name
    TEXT
    NOT
    NULL
    UNIQUE
    CHECK (
    name =
    trim
(
    name
))
    CHECK
(
    length
(
    name
) > 0)
    );

CREATE TABLE IF NOT EXISTS dim_api_key
(
    id
    INTEGER
    PRIMARY
    KEY,
    name
    TEXT
    NOT
    NULL
    UNIQUE
    CHECK (
    name =
    trim
(
    name
))
    CHECK
(
    length
(
    name
) > 0)
    );

CREATE TABLE IF NOT EXISTS dim_source
(
    id
    INTEGER
    PRIMARY
    KEY,
    name
    TEXT
    NOT
    NULL
    UNIQUE
    CHECK (
    name =
    trim
(
    name
))
    CHECK
(
    length
(
    name
) > 0)
    );

CREATE TABLE IF NOT EXISTS dim_auth_index
(
    id
    INTEGER
    PRIMARY
    KEY,
    name
    TEXT
    NOT
    NULL
    UNIQUE
    CHECK (
    name =
    trim
(
    name
))
    CHECK
(
    length
(
    name
) > 0)
    );

-- credential_key = COALESCE(source, auth_index) — captured once at write time
-- so /usage/summary 的 credential 维度对齐 SummaryData.ByCredential 既有语义。
CREATE TABLE IF NOT EXISTS dim_credential
(
    id
    INTEGER
    PRIMARY
    KEY,
    name
    TEXT
    NOT
    NULL
    UNIQUE
    CHECK (
    name =
    trim
(
    name
))
    CHECK
(
    length
(
    name
) > 0)
    );

-- v2 dimension. Provider is the upstream vendor (claude / codex / gemini /
-- openai / anthropic / ...) recorded by the runtime. dim_credential captures
-- the human credential, dim_provider captures which vendor the credential
-- spoke to — together they uniquely identify a (credential, vendor) pair so
-- shared OAuth emails no longer alias.
CREATE TABLE IF NOT EXISTS dim_provider
(
    id
    INTEGER
    PRIMARY
    KEY,
    name
    TEXT
    NOT
    NULL
    UNIQUE
    CHECK (
    name =
    trim
(
    name
))
    CHECK
(
    length
(
    name
) > 0)
    );

CREATE TABLE IF NOT EXISTS events
(
    id
    INTEGER
    PRIMARY
    KEY,

    fingerprint
    BLOB
    NOT
    NULL
    UNIQUE
    CHECK (
    length
(
    fingerprint
) = 20),

    ts_ns INTEGER NOT NULL,

    model_id INTEGER NOT NULL REFERENCES dim_model
(
    id
) ON DELETE RESTRICT,

    api_key_id INTEGER REFERENCES dim_api_key
(
    id
)
  ON DELETE RESTRICT,
    source_id INTEGER REFERENCES dim_source
(
    id
)
  ON DELETE RESTRICT,
    auth_index_id INTEGER REFERENCES dim_auth_index
(
    id
)
  ON DELETE RESTRICT,
    credential_id INTEGER REFERENCES dim_credential
(
    id
)
  ON DELETE RESTRICT,
    provider_id INTEGER REFERENCES dim_provider
(
    id
)
  ON DELETE RESTRICT,

    failed INTEGER NOT NULL CHECK
(
    failed
    IN
(
    0,
    1
)),

    input_tokens INTEGER NOT NULL DEFAULT 0 CHECK
(
    input_tokens
    >=
    0
),
    output_tokens INTEGER NOT NULL DEFAULT 0 CHECK
(
    output_tokens
    >=
    0
),
    reasoning_tokens INTEGER NOT NULL DEFAULT 0 CHECK
(
    reasoning_tokens
    >=
    0
),
    cached_tokens INTEGER NOT NULL DEFAULT 0 CHECK
(
    cached_tokens
    >=
    0
),
    cache_read_tokens INTEGER NOT NULL DEFAULT 0 CHECK
(
    cache_read_tokens
    >=
    0
),
    cache_write_tokens INTEGER NOT NULL DEFAULT 0 CHECK
(
    cache_write_tokens
    >=
    0
),
    total_tokens INTEGER NOT NULL DEFAULT 0 CHECK
(
    total_tokens
    >=
    0
),

    cost_micro INTEGER NOT NULL DEFAULT 0 CHECK
(
    cost_micro
    >=
    0
),
    cost_status TEXT NOT NULL DEFAULT 'unpriced' CHECK
(
    cost_status
    IN
(
    'complete',
    'partial',
    'unpriced'
)
    ),
    cost_reason TEXT NOT NULL DEFAULT '',

    pricing_receipt TEXT CHECK
(
    pricing_receipt
    IS
    NULL
    OR
    (
    typeof
(
    pricing_receipt
) = 'text' AND
    length
(
    CAST
(
    pricing_receipt AS
    BLOB
)) <= 16384 AND
    json_valid
(
    pricing_receipt
)
    )
    ),
    metadata TEXT
    );

CREATE INDEX IF NOT EXISTS idx_events_ts_ns ON events(ts_ns);
CREATE INDEX IF NOT EXISTS idx_events_model_ts_ns ON events(model_id, ts_ns);
CREATE INDEX IF NOT EXISTS idx_events_api_key_ts_ns ON events(api_key_id, ts_ns);
CREATE INDEX IF NOT EXISTS idx_events_source_ts_ns ON events(source_id, ts_ns);
CREATE INDEX IF NOT EXISTS idx_events_auth_index_ts_ns ON events(auth_index_id, ts_ns);
CREATE INDEX IF NOT EXISTS idx_events_cred_ts_ns ON events(credential_id, ts_ns);
CREATE INDEX IF NOT EXISTS idx_events_provider_ts_ns ON events(provider_id, ts_ns);
CREATE INDEX IF NOT EXISTS idx_events_failed_ts_ns ON events(failed, ts_ns);
CREATE INDEX IF NOT EXISTS idx_events_total_tokens ON events(total_tokens);
CREATE INDEX IF NOT EXISTS idx_events_request_id ON events(
    CASE WHEN json_valid(metadata) THEN json_extract(metadata, '$.request_id') END
    );

CREATE TABLE IF NOT EXISTS request_log_artifact
(
    request_id
    TEXT
    PRIMARY
    KEY
    CHECK (
    request_id =
    trim
(
    request_id
))
    CHECK
(
    length
(
    request_id
) > 0 AND length
(
    request_id
) <= 512),
    state TEXT NOT NULL
    CHECK
(
    state
    IN
(
    'pending',
    'available',
    'disabled',
    'not_captured',
    'publish_failed',
    'expired',
    'legacy_unknown'
)),
    location TEXT NOT NULL DEFAULT ''
    CHECK
(
    location
    IN
(
    '',
    'local',
    'home'
)),
    file_name TEXT NOT NULL DEFAULT ''
    CHECK
(
    file_name =
    trim
(
    file_name
)),
    home_ip TEXT NOT NULL DEFAULT ''
    CHECK
(
    home_ip =
    trim
(
    home_ip
)),
    reason TEXT NOT NULL DEFAULT ''
    CHECK
(
    reason =
    trim
(
    reason
)),
    revision INTEGER NOT NULL
    CHECK
(
    typeof
(
    revision
) = 'integer' AND revision > 0),
    updated_at_ns INTEGER NOT NULL
    CHECK
(
    typeof
(
    updated_at_ns
) = 'integer'),
    CHECK
(
    state
    <>
    'available'
    OR
    location
    <>
    ''
),
    CHECK
(
    state
    <>
    'available'
    OR
    location
    <>
    'local'
    OR
    length
(
    file_name
) > 0)
    ) WITHOUT ROWID;

CREATE INDEX IF NOT EXISTS idx_request_log_artifact_state_updated
    ON request_log_artifact(state, updated_at_ns);
CREATE INDEX IF NOT EXISTS idx_request_log_artifact_updated
    ON request_log_artifact(updated_at_ns);

CREATE TABLE IF NOT EXISTS hour_bucket
(
    id
    INTEGER
    PRIMARY
    KEY,

    bucket_ts_ns
    INTEGER
    NOT
    NULL,
    model_id
    INTEGER
    NOT
    NULL
    REFERENCES
    dim_model
(
    id
) ON DELETE RESTRICT,
    credential_id INTEGER REFERENCES dim_credential
(
    id
)
  ON DELETE RESTRICT,
    api_key_id INTEGER REFERENCES dim_api_key
(
    id
)
  ON DELETE RESTRICT,
    provider_id INTEGER REFERENCES dim_provider
(
    id
)
  ON DELETE RESTRICT,

    requests INTEGER NOT NULL DEFAULT 0 CHECK
(
    requests
    >=
    0
),
    success INTEGER NOT NULL DEFAULT 0 CHECK
(
    success
    >=
    0
),
    failure INTEGER NOT NULL DEFAULT 0 CHECK
(
    failure
    >=
    0
),

    input_tokens INTEGER NOT NULL DEFAULT 0 CHECK
(
    input_tokens
    >=
    0
),
    output_tokens INTEGER NOT NULL DEFAULT 0 CHECK
(
    output_tokens
    >=
    0
),
    reasoning_tokens INTEGER NOT NULL DEFAULT 0 CHECK
(
    reasoning_tokens
    >=
    0
),
    cached_tokens INTEGER NOT NULL DEFAULT 0 CHECK
(
    cached_tokens
    >=
    0
),
    cache_read_tokens INTEGER NOT NULL DEFAULT 0 CHECK
(
    cache_read_tokens
    >=
    0
),
    cache_write_tokens INTEGER NOT NULL DEFAULT 0 CHECK
(
    cache_write_tokens
    >=
    0
),
    total_tokens INTEGER NOT NULL DEFAULT 0 CHECK
(
    total_tokens
    >=
    0
),

    cost_micro INTEGER NOT NULL DEFAULT 0 CHECK
(
    cost_micro
    >=
    0
),
    complete_requests INTEGER NOT NULL DEFAULT 0 CHECK
(
    complete_requests
    >=
    0
),
    partial_requests INTEGER NOT NULL DEFAULT 0 CHECK
(
    partial_requests
    >=
    0
),
    unpriced_requests INTEGER NOT NULL DEFAULT 0 CHECK
(
    unpriced_requests
    >=
    0
),
    complete_tokens INTEGER NOT NULL DEFAULT 0 CHECK
(
    complete_tokens
    >=
    0
),
    partial_tokens INTEGER NOT NULL DEFAULT 0 CHECK
(
    partial_tokens
    >=
    0
),
    unpriced_tokens INTEGER NOT NULL DEFAULT 0 CHECK
(
    unpriced_tokens
    >=
    0
),
    latency_ms_total INTEGER NOT NULL DEFAULT 0 CHECK
(
    latency_ms_total
    >=
    0
),
    latency_samples INTEGER NOT NULL DEFAULT 0 CHECK
(
    latency_samples
    >=
    0
),
    CHECK
(
    success
    +
    failure =
    requests
)
    );

-- NULL 维度通过 ifnull(...,0) 表达式参与唯一性,无需 0 哨兵 FK 行。
CREATE UNIQUE INDEX IF NOT EXISTS uq_hour_bucket_key ON hour_bucket(
    bucket_ts_ns,
    model_id,
    ifnull(credential_id, 0),
    ifnull(api_key_id, 0),
    ifnull(provider_id, 0)
    );

CREATE INDEX IF NOT EXISTS idx_hour_bucket_ts_ns ON hour_bucket(bucket_ts_ns);
CREATE INDEX IF NOT EXISTS idx_hour_bucket_model_ts_ns ON hour_bucket(model_id, bucket_ts_ns);
CREATE INDEX IF NOT EXISTS idx_hour_bucket_cred_ts_ns ON hour_bucket(credential_id, bucket_ts_ns);
CREATE INDEX IF NOT EXISTS idx_hour_bucket_apikey_ts_ns ON hour_bucket(api_key_id, bucket_ts_ns);
CREATE INDEX IF NOT EXISTS idx_hour_bucket_prov_ts_ns ON hour_bucket(provider_id, bucket_ts_ns);

CREATE TABLE IF NOT EXISTS day_bucket
(
    id
    INTEGER
    PRIMARY
    KEY,

    bucket_ts_ns
    INTEGER
    NOT
    NULL,
    model_id
    INTEGER
    NOT
    NULL
    REFERENCES
    dim_model
(
    id
) ON DELETE RESTRICT,
    credential_id INTEGER REFERENCES dim_credential
(
    id
)
  ON DELETE RESTRICT,
    api_key_id INTEGER REFERENCES dim_api_key
(
    id
)
  ON DELETE RESTRICT,
    provider_id INTEGER REFERENCES dim_provider
(
    id
)
  ON DELETE RESTRICT,

    requests INTEGER NOT NULL DEFAULT 0 CHECK
(
    requests
    >=
    0
),
    success INTEGER NOT NULL DEFAULT 0 CHECK
(
    success
    >=
    0
),
    failure INTEGER NOT NULL DEFAULT 0 CHECK
(
    failure
    >=
    0
),

    input_tokens INTEGER NOT NULL DEFAULT 0 CHECK
(
    input_tokens
    >=
    0
),
    output_tokens INTEGER NOT NULL DEFAULT 0 CHECK
(
    output_tokens
    >=
    0
),
    reasoning_tokens INTEGER NOT NULL DEFAULT 0 CHECK
(
    reasoning_tokens
    >=
    0
),
    cached_tokens INTEGER NOT NULL DEFAULT 0 CHECK
(
    cached_tokens
    >=
    0
),
    cache_read_tokens INTEGER NOT NULL DEFAULT 0 CHECK
(
    cache_read_tokens
    >=
    0
),
    cache_write_tokens INTEGER NOT NULL DEFAULT 0 CHECK
(
    cache_write_tokens
    >=
    0
),
    total_tokens INTEGER NOT NULL DEFAULT 0 CHECK
(
    total_tokens
    >=
    0
),

    cost_micro INTEGER NOT NULL DEFAULT 0 CHECK
(
    cost_micro
    >=
    0
),
    complete_requests INTEGER NOT NULL DEFAULT 0 CHECK
(
    complete_requests
    >=
    0
),
    partial_requests INTEGER NOT NULL DEFAULT 0 CHECK
(
    partial_requests
    >=
    0
),
    unpriced_requests INTEGER NOT NULL DEFAULT 0 CHECK
(
    unpriced_requests
    >=
    0
),
    complete_tokens INTEGER NOT NULL DEFAULT 0 CHECK
(
    complete_tokens
    >=
    0
),
    partial_tokens INTEGER NOT NULL DEFAULT 0 CHECK
(
    partial_tokens
    >=
    0
),
    unpriced_tokens INTEGER NOT NULL DEFAULT 0 CHECK
(
    unpriced_tokens
    >=
    0
),
    latency_ms_total INTEGER NOT NULL DEFAULT 0 CHECK
(
    latency_ms_total
    >=
    0
),
    latency_samples INTEGER NOT NULL DEFAULT 0 CHECK
(
    latency_samples
    >=
    0
),
    CHECK
(
    success
    +
    failure =
    requests
)
    );

CREATE UNIQUE INDEX IF NOT EXISTS uq_day_bucket_key ON day_bucket(
    bucket_ts_ns,
    model_id,
    ifnull(credential_id, 0),
    ifnull(api_key_id, 0),
    ifnull(provider_id, 0)
    );

CREATE INDEX IF NOT EXISTS idx_day_bucket_ts_ns ON day_bucket(bucket_ts_ns);
CREATE INDEX IF NOT EXISTS idx_day_bucket_model_ts_ns ON day_bucket(model_id, bucket_ts_ns);
CREATE INDEX IF NOT EXISTS idx_day_bucket_cred_ts_ns ON day_bucket(credential_id, bucket_ts_ns);
CREATE INDEX IF NOT EXISTS idx_day_bucket_apikey_ts_ns ON day_bucket(api_key_id, bucket_ts_ns);
CREATE INDEX IF NOT EXISTS idx_day_bucket_prov_ts_ns ON day_bucket(provider_id, bucket_ts_ns);

-- Marks UTC days whose hour buckets changed after their day bucket may already
-- have been materialized. The marker is written and cleared transactionally so
-- late events survive restarts and are eventually included in long-range reads.
CREATE TABLE IF NOT EXISTS dirty_day
(
    day_ts_ns
    INTEGER
    PRIMARY
    KEY
) WITHOUT ROWID;

CREATE TABLE IF NOT EXISTS api_key_reservation
(
    id
    TEXT
    PRIMARY
    KEY
    CHECK (
    id =
    trim
(
    id
))
    CHECK
(
    length
(
    id
) = 68),
    key_identity TEXT NOT NULL
    CHECK
(
    key_identity =
    trim
(
    key_identity
))
    CHECK
(
    length
(
    key_identity
) = 71),
    logical_request_id TEXT NOT NULL
    CHECK
(
    logical_request_id =
    trim
(
    logical_request_id
))
    CHECK
(
    length
(
    logical_request_id
) > 0 AND length
(
    logical_request_id
) <= 512),
    policy_profile TEXT NOT NULL
    CHECK
(
    policy_profile =
    trim
(
    policy_profile
))
    CHECK
(
    length
(
    policy_profile
) > 0 AND length
(
    policy_profile
) <= 128),
    requested_model TEXT NOT NULL
    CHECK
(
    requested_model =
    trim
(
    requested_model
)),
    canonical_model TEXT NOT NULL
    CHECK
(
    canonical_model =
    trim
(
    canonical_model
))
    CHECK
(
    length
(
    canonical_model
) > 0),
    providers_json TEXT NOT NULL
    CHECK
(
    json_valid
(
    providers_json
))
    CHECK
(
    json_type
(
    providers_json
) = 'array'),
    state TEXT NOT NULL
    CHECK
(
    state
    IN
(
    'open',
    'unknown',
    'settled',
    'released',
    'committed',
    'expired_committed'
)),
    created_at_ns INTEGER NOT NULL,
    expires_at_ns INTEGER NOT NULL,
    updated_at_ns INTEGER NOT NULL,
    terminal_at_ns INTEGER,
    terminal_reason TEXT NOT NULL DEFAULT ''
    CHECK
(
    length
(
    terminal_reason
) <= 1024),
    CHECK
(
    expires_at_ns >
    created_at_ns
),
    CHECK
(
    updated_at_ns
    >=
    created_at_ns
),
    CHECK
(
(
    terminal_at_ns
    IS
    NULL
) =
(
    state
    IN
(
    'open',
    'unknown'
))),
    UNIQUE
(
    key_identity,
    logical_request_id,
    policy_profile
)
    ) WITHOUT ROWID;

CREATE INDEX IF NOT EXISTS idx_api_key_reservation_expiry
    ON api_key_reservation(state, expires_at_ns);
CREATE INDEX IF NOT EXISTS idx_api_key_reservation_identity
    ON api_key_reservation(key_identity, policy_profile, created_at_ns);

CREATE TABLE IF NOT EXISTS api_key_reservation_window
(
    reservation_id
    TEXT
    NOT
    NULL
    REFERENCES
    api_key_reservation
(
    id
) ON DELETE CASCADE,
    window_ns INTEGER NOT NULL CHECK
(
    window_ns >
    0
),
    window_start_ns INTEGER NOT NULL,
    window_end_ns INTEGER NOT NULL,
    max_requests INTEGER NOT NULL DEFAULT 0 CHECK
(
    max_requests
    >=
    0
),
    max_tokens INTEGER NOT NULL DEFAULT 0 CHECK
(
    max_tokens
    >=
    0
),
    max_cost_micro INTEGER NOT NULL DEFAULT 0 CHECK
(
    max_cost_micro
    >=
    0
),
    reserved_requests INTEGER NOT NULL DEFAULT 0 CHECK
(
    reserved_requests
    >=
    0
),
    reserved_tokens INTEGER NOT NULL DEFAULT 0 CHECK
(
    reserved_tokens
    >=
    0
),
    reserved_cost_micro INTEGER NOT NULL DEFAULT 0 CHECK
(
    reserved_cost_micro
    >=
    0
),
    actual_requests INTEGER NOT NULL DEFAULT 0 CHECK
(
    actual_requests
    >=
    0
),
    actual_tokens INTEGER NOT NULL DEFAULT 0 CHECK
(
    actual_tokens
    >=
    0
),
    actual_cost_micro INTEGER NOT NULL DEFAULT 0 CHECK
(
    actual_cost_micro
    >=
    0
),
    CHECK
(
    window_end_ns >
    window_start_ns
),
    CHECK
(
    window_end_ns
    -
    window_start_ns =
    window_ns
),
    CHECK
(
    max_requests >
    0
    OR
    max_tokens >
    0
    OR
    max_cost_micro >
    0
),
    CHECK
(
(
    max_requests =
    0
    AND
    reserved_requests =
    0
) OR reserved_requests <= max_requests),
    CHECK
(
(
    max_tokens =
    0
    AND
    reserved_tokens =
    0
) OR reserved_tokens <= max_tokens),
    CHECK
(
(
    max_cost_micro =
    0
    AND
    reserved_cost_micro =
    0
) OR reserved_cost_micro <= max_cost_micro),
    PRIMARY KEY
(
    reservation_id,
    window_ns,
    window_start_ns
)
    )
  WITHOUT ROWID;

CREATE INDEX IF NOT EXISTS idx_api_key_reservation_window_lookup
    ON api_key_reservation_window(window_ns, window_start_ns, window_end_ns);

CREATE TABLE IF NOT EXISTS api_key_reservation_event
(
    reservation_id
    TEXT
    NOT
    NULL
    REFERENCES
    api_key_reservation
(
    id
) ON DELETE CASCADE,
    event_fingerprint BLOB NOT NULL CHECK
(
    length
(
    event_fingerprint
) = 20),
    total_tokens INTEGER NOT NULL CHECK
(
    total_tokens
    >=
    0
),
    cost_micro INTEGER NOT NULL CHECK
(
    cost_micro
    >=
    0
),
    cost_status TEXT NOT NULL CHECK
(
    cost_status
    IN
(
    'complete',
    'partial',
    'unpriced'
)),
    created_at_ns INTEGER NOT NULL,
    PRIMARY KEY
(
    reservation_id,
    event_fingerprint
)
    )
  WITHOUT ROWID;
