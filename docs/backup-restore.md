# Backup and restore — database and media

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

`scripts/backup-media.sh` produces, next to it, the media archives
(`backups/undelete-media-<UTC timestamp>-{full,incremental}.tar.gz` and their
sidecars) — see [Media backup](#media-backup) below.

Two limitations to know:

- **`./media` is not in the dump.** `pg_dump` only backs up the database,
  never the filesystem. The bytes of the attachments are covered by
  `scripts/backup-media.sh`, on its own cadence and with its own retention;
  a dump restored alone gives back a catalogue whose files are missing.
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
purge. Media archives extend that window further, and are **not** purged
automatically (see [Media retention](#media-retention)): a `/delete_my_data`
does not reach into them either.

For media the RPO is the same 24 h — the incremental runs in the same daily
pass, immediately after the dump. **That order is deliberate**: the archive is
taken at `T1` > dump `T0`, so any row already in the dump describes a file
that existed before `T0` and is therefore in the archive. The pair can leave
an *orphan blob* (a file downloaded between `T0` and `T1`, present on disk,
unknown to the restored catalogue), never a catalogued file with no bytes.
Taking the media first would invert exactly that, which is the worse of the
two: an orphan blob wastes disk, a missing object is a hole in the data.

Missing objects are still possible — a file purged from disk (#12) while its
row still reads `stored`, an archive lost from the chain, an incomplete
offsite copy — which is why the restore procedure ends with a reconciliation
pass, and why `make test-restore-media` asserts such a gap is *detected*
rather than silently ignored.

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

## Media backup

`scripts/backup-media.sh` covers `./media`, the half of the data `pg_dump`
never touches. PostgreSQL holds the *metadata* of an attachment (migration
`0004_media_files.sql`); the bytes live at
`./media/<owner_user_id>/<YYYY-MM>/<DD>/<file_unique_id>`.

### Why tar archives and not a filesystem snapshot

A snapshot (LVM, btrfs, ZFS) is the only way to get a genuinely atomic
point-in-time view of a tree, and it would be the better answer — where it is
available. It is not: the homelab VM runs on a plain ext4 root with no spare
LVM extent, which is also the common case on a rented VPS. A procedure that
cannot run on the target machine is not a backup procedure.

`tar` over the media tree gets close enough, because the downloader (#10)
writes each file under a temporary name and `rename()`s it into place: a file
is either absent or complete, so an archive never captures a half-written
blob. What it *can* miss is a file that appears after its path was listed —
picked up by the next incremental, and visible in the meantime as a
discrepancy the reconciliation reports. In exchange, the archives need zero
infrastructure and are portable to any offsite target.

### Full and incremental

| Mode | When | Contents |
|---|---|---|
| `full` | first run, then every `MEDIA_BACKUP_FULL_INTERVAL_DAYS` (7) | every file under `./media` |
| `incremental` | the other days | files modified or added since the base full **started** |

`MEDIA_BACKUP_MODE` (`auto` by default) forces one mode when needed. The mode
is chosen from the base full's *name*, not its mtime: copying archives offsite
rewrites mtimes and would postpone the next full indefinitely.

Incrementals select on `-newer` against the `.started` marker — an empty file
whose mtime is the instant the full began listing — and never against the
archive itself, whose mtime is the instant the full *finished*: every file
created while the full ran would fall in that gap and be backed up by nothing.

### What one run writes

For `undelete-media-20260904T041200Z-full`:

| File | Role |
|---|---|
| `.tar.gz` | the archive, member paths relative to the media root |
| `.manifest` | one `sha256  relative/path` line per member — the integrity reference |
| `.sha256` | checksum of the archive itself, to verify a transfer before extracting |
| `.meta` | mode, base full, **the coupled database dump**, counts, timestamps |
| `.started` | full only: the reference marker for the next incrementals |
| `.skipped` | only if paths had to be excluded (see below) |

The five files travel **together**. An archive without its manifest is still
extractable but no longer verifiable.

`.meta` is what couples the two halves:

```
schema=1
archive=undelete-media-20260904T041200Z-full.tar.gz
mode=full
base_full=-
db_dump=undelete-20260904T041000Z.sql.gz
file_count=1284
archive_bytes=3183129016
archive_sha256=<hex>
```

`db_dump` names the most recent dump present when the archive was taken: the
database state this tree is coherent with.

A path containing a backslash cannot be represented in a `sha256sum`-format
manifest, so it is excluded, listed in `.skipped`, and the script exits **2**
(the archive is written; the anomaly is not swallowed). Nothing the bot
produces has such a name — this only fires on a file dropped into `./media` by
hand.

### Media RPO / RTO and disk space

| Parameter | Value |
|---|---|
| Cadence | daily incremental, weekly full (same pass as the dump, right after it) |
| **Media RPO** | **24 h**, same as the database |
| **Media RTO** | measured by `make test-restore-media` (extraction only), reported as `measured media RTO (extraction only): Ns` |
| Space, steady state | ≈ (size of `./media`) × number of fulls kept + the deltas |

The archives are `gzip`-compressed, but the payload is photos, video and
voice notes — already-compressed formats. **Budget the archives at roughly
the size of `./media` itself**; the compression gain is real only on
documents. With the default 7-day full interval and 4 fulls kept, provision
about **4 × the size of `./media`**, plus the incrementals (one day of new
attachments each).

Check before enabling, and again whenever `./media` grows:

```sh
du -sh ./media ./backups
df -h .
```

The real RTO for the media is dominated by *transferring* the archives back
from the offsite target, not by extracting them. Measure that leg separately;
`make test-restore-media` times a local extraction and nothing else.

### Media retention

**`scripts/backup-media.sh` deletes nothing.** Unlike `scripts/backup.sh`,
there is no `BACKUP_RETENTION_DAYS` purge here, and this is deliberate: an
incremental chain is only restorable as long as the full it is based on still
exists, so an automated purge is one badly chosen predicate away from
silently truncating the chain — the kind of failure that only shows up on the
day of the restore.

Retention is therefore **manual**, and consists of deleting **whole chains**:
a full *and* every incremental that names it in `base_full`. Suggested policy:
keep the last 4 chains (≈ 1 month).

```sh
# 1. List the chains, oldest first.
ls -1 backups/undelete-media-*-full.tar.gz

# 2. For a chain to be dropped, check what depends on it BEFORE deleting.
grep -l 'base_full=undelete-media-<timestamp>-full.tar.gz' backups/*.meta

# 3. Delete the full, its sidecars, and the incrementals listed at step 2.
#    Never with a blanket `rm backups/undelete-media-*`.
```

Deleting archives is on the runbook's closed list of destructive actions
(`docs/runbook.md` §0): it is done knowingly, never "to free some space" in
the middle of an incident.

### Offsite copy and encryption

The archives sit on the same disk as the data they protect: a copy off the
machine is what makes them a backup rather than a second copy. The archives
are self-contained and content-addressed, so any transport works.

Encryption is **an ops decision for Louis, and is not enabled by this repo** —
the commands below are documented, not wired in. Note what they protect
against: an archive contains message attachments in the clear, so anyone
holding the file holds the content.

```sh
# age — one recipient, key kept off the machine (encrypt-only key on the VM).
age -r age1<recipient> -o archive.tar.gz.age archive.tar.gz
age -d -i ~/.age/key.txt -o archive.tar.gz archive.tar.gz.age

# rclone crypt — encrypts names and contents, syncs to the remote in one pass.
rclone sync ./backups remote-crypt:undelete/backups --checksum
```

Whichever is chosen: copy the `.manifest` and `.sha256` sidecars too, and
verify the archive **after** the round trip, not before.

```sh
# From inside ./backups: the sidecar names the archive without a directory.
( cd backups && sha256sum -c undelete-media-<timestamp>-full.sha256 )
```

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

## Automated media restore recipe

```sh
make test-restore-media
```

`scripts/restore-test.sh` proves a dump comes back. It proves nothing about
`./media`, and a restored catalogue whose files are gone is a database full of
dangling references. `scripts/restore-media-test.sh` puts the **pair** on
trial, with the same safety properties as the database recipe (throwaway
containers with unique names, no named volume, no published port, a
`mktemp -d` instead of the repository's `./media` and `./backups`, cleanup
limited to its own two containers, refusal to start if
`MIGRATION_DATABASE_URL` / `DATABASE_URL` are exported).

In one call it:

1. starts a throwaway PostgreSQL 16 **source**, applies the migrations, and
   creates a synthetic tenant plus a media tree in the real layout
   (`<owner>/<YYYY-MM>/<DD>/<file_unique_id>`);
2. registers four `media_files` rows in `stored`: two covered by the full, one
   added afterwards so only the incremental can carry it, and one deliberately
   pointing at a **file that was never written** — the missing object;
3. runs **the real `scripts/backup.sh`**, then **the real
   `scripts/backup-media.sh`** (full, then incremental);
4. checks the coupling: the full's `.meta` must name the dump of the window;
5. checks the incremental contains **exactly** the file added since the full;
6. verifies each archive against its `.sha256` sidecar and with `gzip -t`;
7. restores in the documented order — database into a distinct target
   verified empty beforehand, then media into an **empty** directory, full
   then incremental;
8. verifies both MANIFESTs against the restored tree, hash by hash;
9. **reconciles** the restored catalogue against the restored tree and asserts
   the missing object is reported (1 missing, 0 corrupt);
10. alters one restored file and asserts it is now reported as corrupt — so
    "the file is there" is never mistaken for "the file is intact";
11. removes, via a `trap`, only its own containers and temporary directory.

Output: an `[OK]` / `[FAIL]` verdict per check, the measured media RTO, and a
non-zero exit code if any single check fails.

Both recipes need Docker. Neither runs in CI today (`.github/workflows/ci.yml`
covers lint, unit tests and the Postgres integration suite): they are run
locally, on the cadence below.

## Recipe cadence

**Weekly**, locally on the homelab. A monthly test leaves too much time for a
silent regression (added migration, PostgreSQL image change, truncated
archive) to take hold unseen.

Suggested `crontab -e` entry — Sunday 04:00, journal kept so the measured RTO
can be re-read:

```cron
0 4 * * 0 cd /path/to/Undelete && env -u MIGRATION_DATABASE_URL -u DATABASE_URL make test-restore >> /var/log/undelete-restore-test.log 2>&1
30 4 * * 0 cd /path/to/Undelete && env -u MIGRATION_DATABASE_URL -u DATABASE_URL make test-restore-media >> /var/log/undelete-restore-test.log 2>&1
```

The two recipes are scheduled half an hour apart rather than chained: each is
self-contained, and a failure of the first must not cancel the second — losing
the media verdict because the database one failed would hide half the picture
exactly when it matters.

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
4. **Restore the media — after the database, never before.** The catalogue is
   what says which files must exist; restoring the tree first leaves nothing
   to check it against.

   Pick the **chain** whose window matches the dump just restored: the full
   named in the `.meta` of the incrementals, then every incremental of that
   chain **in chronological order** (their names sort correctly). Order
   matters: a later incremental legitimately overwrites an earlier version of
   the same path.

   ```sh
   # a. Verify the archives BEFORE extracting anything.
   ( cd backups && sha256sum -c undelete-media-<full-ts>-full.sha256 )
   gzip -t backups/undelete-media-<full-ts>-full.tar.gz

   # b. Extract into a NEW media root, never over the one in service.
   mkdir -p /srv/undelete-restore/media
   tar -xzf backups/undelete-media-<full-ts>-full.tar.gz -C /srv/undelete-restore/media
   for incr in backups/undelete-media-*-incremental.tar.gz; do
     grep -q 'base_full=undelete-media-<full-ts>-full.tar.gz' "${incr%.tar.gz}.meta" || continue
     tar -xzf "$incr" -C /srv/undelete-restore/media
   done

   # c. Verify the extracted tree against the MANIFESTs, hash by hash.
   #    This is what turns "it extracted" into "every byte came back".
   ( cd /srv/undelete-restore/media \
     && sha256sum -c /path/to/Undelete/backups/undelete-media-<full-ts>-full.manifest )
   ```

   The manifest paths are relative to the media root, so the `sha256sum -c`
   must run **from inside** the restored tree, with the manifest addressed by
   an absolute path.
5. **Reconcile the catalogue against the tree.** The final check, and the one
   that decides whether the restoration is complete: every `media_files` row
   in `stored` must have its file present, with the right `sha256`.

   ```sql
   -- Inventory to reconcile. As superuser, RLS is bypassed; from the
   -- application role, set the tenant context first (constraint #4).
   SELECT relative_path, sha256 FROM media_files
    WHERE status = 'stored' AND relative_path IS NOT NULL;
   ```

   Two discrepancies to expect, with different meanings:
   - **missing object** (row in `stored`, no file): a genuine hole. It comes
     from a purged file, a lost archive or an incomplete offsite copy. The
     reconciliation command of #12 repairs it by moving the row to `purged`,
     so the deletion alert can still state that a media existed.
   - **orphan blob** (file present, no row): harmless, and expected — it is
     the file downloaded between the dump and the media archive (see the RPO
     section). It costs disk, not data.

   `make test-restore-media` runs exactly this comparison on throwaway
   containers and asserts both a missing object and an altered file are
   reported. Run it if you want to see the shape of the output before doing it
   for real.
6. **Verify before switching over.** The same checks as the recipe:
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
7. **Switch over.** Point `MIGRATION_DATABASE_URL` and `DATABASE_URL` at the
   restored database, point the `./media` bind mount at the restored tree,
   restart the bot, and **only once the service is verified** decide the fate
   of the old volume and of the old media directory. Keeping both for at least
   one retention cycle remains the prudent choice.

At restart, the binary replays its embedded migrations: versions already
present in `schema_migrations` are skipped, a migration added since the
archive is applied. An old-archive restoration therefore brings itself back
up to the current schema level.
