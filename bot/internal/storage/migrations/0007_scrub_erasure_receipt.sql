-- Migration 0007: scrub the identifying columns of a completed erasure receipt.
--
-- /delete_my_data deliberately keeps one row per erased tenant: the completed
-- request is the receipt that lets a replayed confirmation be answered "already
-- erased" instead of "unknown code". That receipt needs the tenant key
-- (owner_user_id, which the RLS policy and every repository method scope on),
-- the code hash (which the replay looks up) and the timestamps -- and nothing
-- else.
--
-- owner_telegram_user_id and business_connection_id were written at Issue time
-- so a pending request could be logged and answered without a join. Once the
-- request is completed they serve no reader: the replay is answered from the
-- Business connection the message arrived through (resolved by
-- business.Service, which is also what keeps a disabled connection
-- authenticating control commands), never from this row. Completed rows are
-- therefore scrubbed of both columns by the same statement that flips them to
-- 'completed' (see Repository.Complete): the receipt keeps no Telegram
-- identifier and no connection identifier.
--
-- The columns stay for pending and consumed rows -- Issue still writes them
-- NOT NULL -- so this migration only relaxes the nullability, it does not
-- remove anything. Additive, backwards compatible: an older bot runs
-- unchanged on this schema.
ALTER TABLE data_erasure_requests
    ALTER COLUMN owner_telegram_user_id DROP NOT NULL,
    ALTER COLUMN business_connection_id DROP NOT NULL;
