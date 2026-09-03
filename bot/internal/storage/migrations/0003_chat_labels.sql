-- Migration 0003: chat labels, so deletion alerts are readable without having
-- to decode a numeric chat_id.
--
-- Constraint #8: this table is NOT a selection table. It contains no
-- activation flag and is never consulted to decide whether a message should
-- be saved or notified -- only for DISPLAYING the label in the alert. A row
-- is written for ALL chats seen, with no exception or filter.
--
-- title carries the display label already computed on the application side
-- (Title for chats that have one, first name + last name for private chats,
-- which Telegram never describes by a title). username and type are kept
-- separate; the alert format composes them itself.
--
-- last_seen_at documents label freshness (a renamed contact updates its row
-- at the next message); no purge relies on it in Phase 1.
CREATE TABLE chats (
    owner_user_id           BIGINT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    business_connection_id  TEXT NOT NULL,
    chat_id                 BIGINT NOT NULL,
    title                   TEXT NOT NULL DEFAULT '',
    username                TEXT NOT NULL DEFAULT '',
    type                    TEXT NOT NULL DEFAULT '',
    last_seen_at            TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (owner_user_id, business_connection_id, chat_id)
);

-- Same RLS mechanism as messages and notification_outbox: a chat label is a
-- tenant's personal data and therefore gets the same FORCE isolation (ENABLE
-- alone does not apply to the table owner).
--
-- Unlike business_connections, there is NO circularity in carrying a policy
-- here: the chats row is written in the same transaction as the corresponding
-- message, hence always after business.Service.Resolve provided owner_user_id
-- and InTenant set app.current_owner_user_id. Its read (deletion alert)
-- happens in the MarkDeleted transaction, where the context is set for the
-- same reasons. No code path needs to read chats to discover an owner.
ALTER TABLE chats ENABLE ROW LEVEL SECURITY;
ALTER TABLE chats FORCE ROW LEVEL SECURITY;
CREATE POLICY chats_tenant_isolation ON chats
    USING (owner_user_id = NULLIF(current_setting('app.current_owner_user_id', true), '')::bigint)
    WITH CHECK (owner_user_id = NULLIF(current_setting('app.current_owner_user_id', true), '')::bigint);

-- Explicit grants, as in 0002: this migration may run in a database where the
-- owner role's default privileges were not configured. No sequence to grant
-- here, the primary key is composite.
GRANT SELECT, INSERT, UPDATE, DELETE ON chats TO undelete_app;

-- Deliberately NO backfill: chats already known from messages never had their
-- label persisted (the Telegram update carrying it has passed), so there is
-- nothing to copy. These chats remain without a row until their next message,
-- and the alert then falls back to "chat <id>".
