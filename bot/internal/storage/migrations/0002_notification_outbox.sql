-- Durable outbox for deletion alerts. The payload is user content and
-- therefore receives the same FORCE RLS isolation as messages.
CREATE TABLE notification_outbox (
    id                      BIGSERIAL PRIMARY KEY,
    owner_user_id           BIGINT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    owner_telegram_user_id  BIGINT NOT NULL,
    business_connection_id  TEXT NOT NULL,
    chat_id                 BIGINT NOT NULL,
    message_id              BIGINT NOT NULL,
    event_type              TEXT NOT NULL,
    chunk_index             INT NOT NULL DEFAULT 0,
    payload_text            TEXT NOT NULL,
    status                  TEXT NOT NULL DEFAULT 'pending'
                            CHECK (status IN ('pending', 'processing', 'sent', 'failed')),
    attempts                INT NOT NULL DEFAULT 0 CHECK (attempts >= 0),
    next_attempt_at         TIMESTAMPTZ NOT NULL DEFAULT now(),
    locked_until            TIMESTAMPTZ NULL,
    lease_token             TEXT NULL,
    last_error_class        TEXT NULL,
    created_at              TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at              TIMESTAMPTZ NOT NULL DEFAULT now(),
    sent_at                 TIMESTAMPTZ NULL,
    UNIQUE (owner_user_id, business_connection_id, chat_id, message_id, event_type, chunk_index)
);

CREATE INDEX idx_notification_outbox_due
    ON notification_outbox (owner_user_id, next_attempt_at, id)
    WHERE status IN ('pending', 'processing');

ALTER TABLE notification_outbox ENABLE ROW LEVEL SECURITY;
ALTER TABLE notification_outbox FORCE ROW LEVEL SECURITY;
CREATE POLICY notification_outbox_tenant_isolation ON notification_outbox
    USING (owner_user_id = NULLIF(current_setting('app.current_owner_user_id', true), '')::bigint)
    WITH CHECK (owner_user_id = NULLIF(current_setting('app.current_owner_user_id', true), '')::bigint);

-- Explicit grants: this migration may run in a database where default
-- privileges were not configured by the owner role.
GRANT SELECT, INSERT, UPDATE, DELETE ON notification_outbox TO undelete_app;
GRANT USAGE, SELECT ON SEQUENCE notification_outbox_id_seq TO undelete_app;
