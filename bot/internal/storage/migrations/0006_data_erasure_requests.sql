-- Migration 0006: data_erasure_requests, the challenge behind /delete_my_data.
--
-- The command is a two-step act: /delete_my_data issues a code, and
-- /delete_my_data <code> spends it. This table is what makes the second step
-- verifiable, and it lives in PostgreSQL rather than in the process memory for
-- one reason: a code issued a minute before a restart must still be honoured,
-- and a code already spent must still be refused after one. An in-memory
-- challenge would forget both, and the second forgetting is the dangerous one
-- (a spent code becoming live again is a replayable erasure).
--
-- What is stored is the SHA-256 of the code, never the code itself. The rows
-- travel into every pg_dump like everything else, and a dump must not hand a
-- reader a live erasure token. The hash is enough: confirmation hashes what
-- the owner typed and compares, it never needs to read the code back.
CREATE TABLE data_erasure_requests (
    id                      BIGSERIAL PRIMARY KEY,
    owner_user_id           BIGINT NOT NULL REFERENCES users(id) ON DELETE CASCADE,

    -- Kept alongside owner_user_id so the confirmation can be logged and
    -- answered without a join, exactly like notification_outbox does.
    owner_telegram_user_id  BIGINT NOT NULL,
    -- The connection the request was typed through. Informative: the erasure
    -- covers the TENANT, every connection of that owner included, never one
    -- connection in isolation.
    business_connection_id  TEXT NOT NULL,

    code_sha256             TEXT NOT NULL CHECK (code_sha256 ~ '^[0-9a-f]{64}$'),

    -- pending   : issued, not spent yet.
    -- consumed  : spent, the deletion is running (or was interrupted).
    -- completed : the deletion went through to its last step.
    --
    -- There is no 'cancelled': a code that is never spent simply expires, and
    -- the erasure deletes every row of the tenant but the one it completed.
    status                  TEXT NOT NULL DEFAULT 'pending'
                            CHECK (status IN ('pending', 'consumed', 'completed')),

    created_at              TIMESTAMPTZ NOT NULL DEFAULT now(),
    -- expires_at is evaluated on the PostgreSQL clock, like every outbox
    -- deadline: a drift between the bot clock and the database clock must not
    -- keep a code alive past its window.
    expires_at              TIMESTAMPTZ NOT NULL,
    consumed_at             TIMESTAMPTZ NULL,
    completed_at            TIMESTAMPTZ NULL,

    -- One row per (tenant, code). The claim is an UPDATE keyed on this pair
    -- with status = 'pending' in its WHERE: PostgreSQL serialises the two
    -- writers, so of two submissions of the same code exactly one updates a
    -- row and the other sees none. That is the whole single-use guarantee.
    UNIQUE (owner_user_id, code_sha256),

    CONSTRAINT data_erasure_requests_consumed_is_stamped CHECK (
        status = 'pending' OR consumed_at IS NOT NULL
    ),
    CONSTRAINT data_erasure_requests_completed_is_stamped CHECK (
        status <> 'completed' OR completed_at IS NOT NULL
    )
);

-- Issuing a code first clears the tenant's other pending rows, and the erasure
-- clears everything but the row it completed: both scan by (owner, status).
CREATE INDEX idx_data_erasure_requests_owner_status
    ON data_erasure_requests (owner_user_id, status);

-- Same RLS mounting as messages, chats, notification_outbox and media_files: a
-- live erasure token is tenant data, and the tenant scope is precisely what
-- keeps a code issued to one owner from being spent by another. FORCE is
-- required, ENABLE alone does not apply to the table owner (cf. 0001). Without
-- a tenant context, NULLIF(current_setting(...), '') is NULL and matches no
-- row: the table is fail-closed, and the repository therefore only ever reaches
-- it through storage.DB.InTenant.
ALTER TABLE data_erasure_requests ENABLE ROW LEVEL SECURITY;
ALTER TABLE data_erasure_requests FORCE ROW LEVEL SECURITY;
CREATE POLICY data_erasure_requests_tenant_isolation ON data_erasure_requests
    USING (owner_user_id = NULLIF(current_setting('app.current_owner_user_id', true), '')::bigint)
    WITH CHECK (owner_user_id = NULLIF(current_setting('app.current_owner_user_id', true), '')::bigint);

-- Explicit grants, as in 0002 and 0004: this migration may run in a database
-- where the owner role's default privileges were not configured.
GRANT SELECT, INSERT, UPDATE, DELETE ON data_erasure_requests TO undelete_app;
GRANT USAGE, SELECT ON SEQUENCE data_erasure_requests_id_seq TO undelete_app;
