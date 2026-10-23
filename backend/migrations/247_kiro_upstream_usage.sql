-- Native credits/context occupancy must not be stored as tokens or USD costs.
-- request_id matches the existing usage_logs request ID; retries are independent
-- observations. Include the caller API key so reused client request IDs cannot
-- associate one user's metering with another user's log.
CREATE TABLE IF NOT EXISTS kiro_upstream_usage (
    request_id VARCHAR(255) NOT NULL,
    account_id BIGINT NOT NULL,
    api_key_id BIGINT NOT NULL,
    model VARCHAR(255) NOT NULL,
    usage_source VARCHAR(32) NOT NULL,
    upstream_usage JSONB NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (request_id, account_id, api_key_id)
);
CREATE INDEX IF NOT EXISTS idx_kiro_upstream_usage_account_created
    ON kiro_upstream_usage (account_id, created_at DESC);
