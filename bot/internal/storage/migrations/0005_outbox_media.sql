-- Migration 0005: carry the media of a deletion alert through the outbox.
--
-- The alert of a deleted message already travels as text chunks in
-- notification_outbox. Restoring the media (#11) adds ONE more entry per
-- message (or per album), which references the rows of media_files instead of
-- carrying the bytes: the blob stays on disk under ./media, and an outbox row
-- stays small enough for a pg_dump.
--
-- Additive migration, like every one before it: payload_kind defaults to
-- 'text', so the rows already queued keep behaving exactly as they did and an
-- older bot binary running against this schema still delivers them.
ALTER TABLE notification_outbox
    ADD COLUMN payload_kind TEXT NOT NULL DEFAULT 'text'
        CHECK (payload_kind IN ('text', 'media')),
    -- media_payload freezes, at deletion time, what has to be sent: the
    -- media_files ids, their relative paths, their order and their caption.
    -- Frozen rather than resolved at send time so a media purged from the
    -- catalogue between the deletion and the delivery still produces the
    -- documented text fallback instead of an empty alert.
    ADD COLUMN media_payload JSONB NULL,
    -- A 'media' row without payload would describe nothing to send, and the
    -- worker would have no fallback text to prefer either.
    ADD CONSTRAINT notification_outbox_media_has_payload CHECK (
        payload_kind <> 'media' OR media_payload IS NOT NULL
    );

-- file_name is the name the SENDER gave the file (documents, audio, video).
-- Restoring a deleted document under a generated name would lose information
-- the owner cannot get back, so the name is catalogued with the rest of the
-- metadata. It is deliberately NOT a storage path: the path stays
-- server-generated (cf. 0004), and this value is only ever used as the display
-- name of a multipart upload, sanitized before it reaches a header.
ALTER TABLE media_files
    ADD COLUMN file_name TEXT NULL;

-- No new grant on either table: the privileges of 0002 and 0004 are granted on
-- the tables, and a new column inherits them.
