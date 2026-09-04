-- Migration 0004: media_files, the attachment catalogue.
--
-- Phase 2 principle: PostgreSQL stores the METADATA of an attachment, never
-- the blob. The bytes live on disk under ./media, referenced by a relative
-- path generated on our side. Storing the blobs here would blow up the size
-- of every pg_dump (cf. docs/backup-restore.md, RPO/RTO measured on a
-- text-only database) for no benefit: the file is not queryable data.
--
-- Downloading the files, backing them up (#13) and purging them from disk
-- (#12) are out of scope for this migration: the table only carries the
-- status that those two will drive.
CREATE TABLE media_files (
    id                      BIGSERIAL PRIMARY KEY,

    -- Full message identity, same quadruple as messages (cf. 0001):
    -- message_id is unique only within a chat, never globally.
    --
    -- Deliberately NO foreign key to messages. The link is logical, not
    -- enforced: a media row must be able to survive a discrepancy with the
    -- messages table (retention purging the message before the file has been
    -- purged from disk would, with an ON DELETE CASCADE, silently erase the
    -- only record of a file still sitting in ./media -- an orphan blob that
    -- nothing would ever come back to delete). owner_user_id, on the other
    -- hand, does reference users: a deleted tenant leaves nothing behind.
    owner_user_id           BIGINT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    business_connection_id  TEXT NOT NULL,
    chat_id                 BIGINT NOT NULL,
    message_id              BIGINT NOT NULL,

    -- file_index orders the attachments WITHIN one message (0..N). A single
    -- message can carry several files, and an album (media_group_id) spreads
    -- them across several messages: the two mechanisms coexist.
    file_index              INT NOT NULL DEFAULT 0 CHECK (file_index >= 0),

    -- telegram_file_id is only valid for the bot that received it and can
    -- change between updates; telegram_file_unique_id is stable and identifies
    -- the same content across messages (it is NOT a download handle).
    telegram_file_id        TEXT NOT NULL,
    telegram_file_unique_id TEXT NOT NULL,

    media_type              TEXT NOT NULL CHECK (media_type IN (
                                'photo', 'video', 'animation', 'document',
                                'audio', 'voice', 'video_note', 'sticker',
                                'unknown')),
    mime_type               TEXT NULL,
    byte_size               BIGINT NULL CHECK (byte_size IS NULL OR byte_size >= 0),
    width                   INT NULL,
    height                  INT NULL,
    duration_sec            INT NULL,

    -- relative_path is RELATIVE to ./media and generated server-side. It is
    -- never derived from a Telegram-provided file name: those are attacker
    -- controlled (the sender chooses them) and would be the direct route to
    -- writing outside the media directory. The CHECK below is the last line of
    -- defence, doubled by media.ValidateRelativePath on the Go side.
    relative_path           TEXT NULL,
    thumbnail_relative_path TEXT NULL,

    -- sha256 of the stored file, lowercase hex. Set at download time, used to
    -- detect a truncated or corrupted file (and, later, deduplication).
    sha256                  TEXT NULL CHECK (sha256 IS NULL OR sha256 ~ '^[0-9a-f]{64}$'),

    -- pending  : row created on receipt, nothing on disk yet.
    -- stored   : file written under ./media, path and hash known.
    -- purged   : file deleted from disk by retention; the row is kept so the
    --            deletion alert can still state that a media existed.
    status                  TEXT NOT NULL DEFAULT 'pending'
                            CHECK (status IN ('pending', 'stored', 'purged')),

    -- media_group_id groups the messages of one album, as sent by Telegram.
    -- NULL for a lone attachment.
    media_group_id          TEXT NULL,

    created_at              TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at              TIMESTAMPTZ NOT NULL DEFAULT now(),

    -- Anti-collision: one row per (message, file_index). Telegram redelivers
    -- updates (poller restart before offset confirmation); the upsert in
    -- media.Repository.Save keys on exactly this constraint, so a redelivery
    -- refreshes the metadata instead of creating a duplicate download job.
    UNIQUE (owner_user_id, business_connection_id, chat_id, message_id, file_index),

    -- Anti-traversal, applied identically to both paths: no leading '/'
    -- (absolute), no backslash (Windows separator, and a way to smuggle a
    -- separator past a naive check), no '.' or '..' component, no empty
    -- component ('//'), no empty string. Combined with a base directory join,
    -- this keeps every write inside ./media.
    CONSTRAINT media_files_relative_path_is_safe CHECK (
        relative_path IS NULL OR (
            relative_path <> ''
            AND relative_path !~ '^/'
            AND relative_path !~ '\\'
            AND relative_path !~ '//'
            AND relative_path !~ '/$'
            AND relative_path !~ '(^|/)\.\.?(/|$)'
        )
    ),
    CONSTRAINT media_files_thumbnail_path_is_safe CHECK (
        thumbnail_relative_path IS NULL OR (
            thumbnail_relative_path <> ''
            AND thumbnail_relative_path !~ '^/'
            AND thumbnail_relative_path !~ '\\'
            AND thumbnail_relative_path !~ '//'
            AND thumbnail_relative_path !~ '/$'
            AND thumbnail_relative_path !~ '(^|/)\.\.?(/|$)'
        )
    ),

    -- A 'stored' row without a path would describe a file nobody can find, and
    -- that the disk purge would therefore never delete.
    CONSTRAINT media_files_stored_has_path CHECK (
        status <> 'stored' OR relative_path IS NOT NULL
    )
);

-- Download queue and disk purge both scan by tenant then by status
-- (constraint #4: those loops go tenant by tenant, never globally).
CREATE INDEX idx_media_files_owner_status ON media_files (owner_user_id, status);

-- Albums: partial index, since media_group_id is NULL for the vast majority of
-- attachments (a lone media carries no group).
CREATE INDEX idx_media_files_media_group
    ON media_files (owner_user_id, media_group_id)
    WHERE media_group_id IS NOT NULL;

-- Same RLS mounting as messages, notification_outbox and chats: an attachment
-- is tenant personal data. FORCE is required, ENABLE alone does not apply to
-- the table owner (cf. the detailed comment in 0001). Without a tenant
-- context, NULLIF(current_setting(...), '') is NULL and matches no row: the
-- table is fail-closed, and media.Repository therefore only ever reaches it
-- through storage.DB.InTenant.
ALTER TABLE media_files ENABLE ROW LEVEL SECURITY;
ALTER TABLE media_files FORCE ROW LEVEL SECURITY;
CREATE POLICY media_files_tenant_isolation ON media_files
    USING (owner_user_id = NULLIF(current_setting('app.current_owner_user_id', true), '')::bigint)
    WITH CHECK (owner_user_id = NULLIF(current_setting('app.current_owner_user_id', true), '')::bigint);

-- Explicit grants, as in 0002 and 0003: this migration may run in a database
-- where the owner role's default privileges were not configured.
GRANT SELECT, INSERT, UPDATE, DELETE ON media_files TO undelete_app;
GRANT USAGE, SELECT ON SEQUENCE media_files_id_seq TO undelete_app;
