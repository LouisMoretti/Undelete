-- Migration 0008: at most one live /delete_my_data code per tenant.
--
-- Issue used to clear the tenant's pending rows and insert the new one in one
-- READ COMMITTED transaction. Two concurrent Requests of the same tenant (two
-- chats, two poller shards) each cleared a snapshot that did not contain the
-- other's uncommitted row, and both inserted: two live codes. The invariant
-- now lives in the schema, and Issue replaces the pending row with a single
-- INSERT ... ON CONFLICT on this index, so the last request wins atomically.
--
-- Pre-existing duplicates are collapsed first, keeping the newest pending row
-- of each tenant (the code the owner received last). The table is under FORCE
-- ROW LEVEL SECURITY, which also binds its owner: without a tenant context
-- that DELETE would match nothing and the index build would then fail on the
-- duplicates. FORCE is lifted for the statement and restored in the same
-- transaction, so no other session ever sees the table without it.
ALTER TABLE data_erasure_requests NO FORCE ROW LEVEL SECURITY;

DELETE FROM data_erasure_requests older
WHERE older.status = 'pending'
  AND EXISTS (
      SELECT 1 FROM data_erasure_requests newer
      WHERE newer.owner_user_id = older.owner_user_id
        AND newer.status = 'pending'
        AND newer.id > older.id
  );

ALTER TABLE data_erasure_requests FORCE ROW LEVEL SECURITY;

CREATE UNIQUE INDEX data_erasure_requests_one_pending_per_owner
    ON data_erasure_requests (owner_user_id)
    WHERE status = 'pending';
