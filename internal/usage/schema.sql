-- Usage DB schema v2
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
OR IGNORE INTO meta(key, value) VALUES ('schema_version', '2');

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
