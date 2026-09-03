# PostgreSQL backup and restore

A backup that has never been restored is not a verified backup: it is a
hypothesis. This document describes the backup in place, the data-loss
objective (RPO), the recovery-time objective (RTO), and the periodic recipe
that proves the archive is genuinely restorable.

## Absolute rule

**Never delete or purge an existing Docker volume.** No procedure in this
document calls `docker volume rm`, `docker volume prune`,
`docker system prune` nor `docker compose down -v`. The stack's `pgdata`
volume holds the only hot copy of the data: destroying it turns an incident
into definitive loss. A restoration is **always** done to a new, explicitly
distinct target, never "over" the existing one.

## What is backed up

`scripts/backup.sh` produces `backups/undelete-<UTC timestamp>.sql.gz` via
`pg_dump "$MIGRATION_DATABASE_URL" | gzip`, then purges archives older than
`BACKUP_RETENTION_DAYS` days (14 by default).

Two limitations to know:

- **`./media` is not in the dump.** `pg_dump` only backs up the database,
  never the filesystem. As of Phase 2 (media handling), a separate backup of
  this directory will be needed.
- **Roles are not in the dump.** `undelete_app` is a cluster-wide object, not
  a database one. A restoration into a new cluster must recreate this role
  *before* replaying the archive, otherwise the `GRANT`s of migrations 0002
  and 0003 fail. In operation, `db/init/01-app-role.sh` handles this at
  cluster initialization.

## RPO — maximum accepted data loss

| Parameter | Value |
|---|---|
| Backup cadence | daily (`scripts/backup.sh`) |
| **RPO** | **24 h** — at worst, the messages received since the last successful dump |
| Archive retention | `BACKUP_RETENTION_DAYS`, 14 days by default |
| Restoration depth | 14 days; beyond that, no archive remains |

The RPO is directly the cadence: there is neither WAL archiving nor
replication, therefore no point-in-time recovery (PITR). Going below 24 h
requires running `backup.sh` more often — and increases the number of archives
to keep accordingly.

Retention also has a "privacy" reading: `BACKUP_RETENTION_DAYS` is
the residual survival time of a user's data after a deletion in the
database, since already-written archives keep containing it until their own
purge.

## RTO — recovery time

The RTO is not estimated, it is **measured at every run of the recipe**:
`scripts/restore-test.sh` times the restoration alone (archive decompression +
SQL replay, `date -u +%s` before and after) and displays it at the end of the
output:

```
restore-test: measured RTO (restore only): Ns
```

What this number covers and does not cover:

- **covered**: `gunzip` of the archive then full SQL replay on a blank
  database — the work proportional to the data volume;
- **not covered**: machine provisioning, `docker compose up`,
  PostgreSQL startup, recreation of the application role. Count these items
  on top when computing the real operational RTO.

The granularity is the second; on a test dataset, the measurement is therefore
a floor (`0s` or `1s`) and not a projection. It becomes significant when
measured against a production archive: report the observed value at each
recipe run to track its drift over time.

## Automated restore recipe

```sh
make test-restore
```

In a single call, `scripts/restore-test.sh`:

1. starts a throwaway PostgreSQL 16 **source** (`docker run --rm`, unique
   timestamped name, no port published on the host, no named volume);
2. applies `bot/internal/storage/migrations/*.sql` to it in numeric order,
   faithfully reproducing the Go runner (`storage.RunMigrations`): same DDL
   for `schema_migrations`, same sorting by file name, migration and
   version recording in a single transaction;
3. inserts recognizable synthetic data (prefix `RESTORE-TEST`, Telegram
   identifiers outside the real range) into `users`,
   `business_connections`, `chats`, `messages` and `notification_outbox`;
4. runs **the real `scripts/backup.sh`** — it is indeed the production
   script's output that is put to the test — with `BACKUP_DIR` in a
   temporary directory, never in the repository's `./backups`;
5. verifies the archive's integrity with `gzip -t`;
6. starts a second, distinct **target** container with a blank database, and
   verifies it is actually empty before restoration — if it is not,
   the script stops there, without overwriting anything;
7. restores (`gunzip` then `psql`) while timing the operation;
8. verifies after restoration: presence of the expected tables (`users`,
   `business_connections`, `chats`, `messages`, `notification_outbox`,
   `schema_migrations`), `schema_migrations` versions identical to those
   applied, per-table row counts equal to the source, content of the canary
   rows (message, chat label, outbox payload), and persistence of
   `FORCE ROW LEVEL SECURITY` on the three protected tables;
9. removes, via a `trap`, **only its two containers** and its
   temporary directory.

Output: a `[OK]` / `[ECHEC]` verdict per check, the measured RTO, and a
non-zero exit code if any single check fails.

The script refuses to start if `MIGRATION_DATABASE_URL` or `DATABASE_URL` is
present in the environment: it creates its two throwaway databases itself and
accepts no external target, so that no restoration can land in the dev or
prod database. If these variables are exported in your shell:

```sh
env -u MIGRATION_DATABASE_URL -u DATABASE_URL make test-restore
```

Each run uses unique container names (PID + timestamp) and a fresh
`mktemp -d`: the recipe is replayable as-is, including in parallel, without
prior cleanup.

## Recipe cadence

**Weekly**, locally on the homelab. A monthly test leaves too much time for a
silent regression (added migration, PostgreSQL image change, truncated
archive) to take hold unseen.

Suggested `crontab -e` entry — Sunday 04:00, journal kept so the measured RTO
can be re-read:

```cron
0 4 * * 0 cd /path/to/Undelete && env -u MIGRATION_DATABASE_URL -u DATABASE_URL make test-restore >> /var/log/undelete-restore-test.log 2>&1
```

Re-read the journal after each run: the final verdict and the measured RTO.
An RTO that keeps climbing announces the moment when the "full daily dump"
strategy will no longer suffice.

## Real restoration after an incident

Step-by-step procedure, to be done **towards a new target**:

1. **Choose the archive.** `ls -lt backups/undelete-*.sql.gz` — the timestamp
   is in UTC. Verify its integrity before anything else:
   `gzip -t backups/undelete-<timestamp>.sql.gz`.
2. **Prepare a blank, distinct target.** A new cluster, or at minimum
   a newly created database (`CREATE DATABASE undelete_restore;`) on a
   cluster where `db/init/01-app-role.sh` created the `undelete_app` role.
   Never restore into the database in service, and **never delete the existing
   volume**: as long as the restoration is not validated, that volume is the
   only copy of the data.
3. **Restore.** In two commands, and with `-v ON_ERROR_STOP=1`:
   ```sh
   gunzip -c backups/undelete-<timestamp>.sql.gz > /tmp/undelete-restore.sql
   psql -v ON_ERROR_STOP=1 \
     -f /tmp/undelete-restore.sql \
     "postgres://postgres:<password>@127.0.0.1:5432/undelete_restore"
   ```
   The two details of this command are what distinguish a verified restoration
   from an assumed one:
   - **`-v ON_ERROR_STOP=1`**: without it, `psql` continues after an error and
     exits 0, which would present a partial restoration as successful.
   - **no `gunzip | psql` pipe**: in a pipe, the retained exit code is the
     `psql` one, so a decompression that fails midway is masked by a
     `psql` satisfied with what it received. By decomposing, each link is
     tested on its own (this is also what `scripts/restore-test.sh` does).

   Delete `/tmp/undelete-restore.sql` afterwards: this file contains all the
   message content in plaintext.
4. **Verify before switching over.** The same checks as the recipe:
   ```sql
   SELECT version FROM schema_migrations ORDER BY version;
   SELECT count(*) FROM users;
   SELECT count(*) FROM messages;
   SELECT relname, relrowsecurity, relforcerowsecurity
     FROM pg_class
     WHERE relnamespace = 'public'::regnamespace
       AND relname IN ('messages', 'notification_outbox', 'chats');
   ```
   `relforcerowsecurity` must be true for the three tables: without it,
   multi-tenant isolation no longer holds.
5. **Switch over.** Point `MIGRATION_DATABASE_URL` and `DATABASE_URL` at the
   restored database, restart the bot, and **only once the service is
   verified** decide the fate of the old volume. Keeping it for at least one
   retention cycle remains the prudent choice.

At restart, the binary replays its embedded migrations: versions already
present in `schema_migrations` are skipped, a migration added since the
archive is applied. An old-archive restoration therefore brings itself back
up to the current schema level.
