# Runbook — deployment, update and rollback

Operating `undelete` on the homelab (Proxmox → NixOS VM → Docker Compose
launched by hand). No deployment CI: **every production deployment is a manual
execution of this procedure**, in order.

All commands are run from the repository root on the VM.

> **Dependencies between PRs.** This runbook references two elements delivered
> by other PRs in the same stack: the HTTP probes `/livez`, `/readyz` and
> `/metrics` on port `9090` (**available after the probes PR, #6**) and
> `make test-restore` + `docs/backup-restore.md` (**available after the
> test-restore PR, #7**). The affected steps are marked *(after #6)* /
> *(after #7)* and have an alternative applicable today.

---

## 0. Destructive actions — closed list

> ### ⛔ FORBIDDEN WITHOUT EXPLICIT CONFIRMATION FROM LOUIS
>
> The commands below destroy data that **nothing restores** (the dumps in
> `./backups` cover the database, never the volume nor `./media`).
> None of them is required to deploy, update or roll back. They must **never**
> be run "to unblock" an incident, nor be proposed as a remedy by an agent.
>
> | Command | Irreversible effect |
> |----------|--------------------|
> | `docker compose down -v` | deletes the `postgres_data` volume → **the entire database is lost** |
> | `docker volume rm undelete_postgres_data` | same, without even a clean shutdown |
> | `docker volume prune` / `docker system prune` | can take `postgres_data` and other VM volumes with it |
> | `DROP DATABASE` / `DROP SCHEMA` / `TRUNCATE` | empties the database under the running bot |
> | `psql < dump.sql` on the production database | overwrites the current state (restoration: see §3.3) |
> | `rm -rf ./backups` (or deleting dumps outside the retention purge) | removes the only safety net |
>
> **Operating rule**: stopping the stack is done with `make down`
> (= `docker compose down`, **without `-v`**), which preserves `postgres_data`.
> Any disk-space purge is done on the *files* in `./backups` (oldest dumps),
> never on Docker resources.

---

## 1. Preflight

### 1.1 Automatic

```bash
sh scripts/preflight.sh
```

**Read-only** script (no writes, no deletions), replayable.
It reports one line per check and exits with code 1 at the first `[ECHEC]`:

| Check | Detail |
|---|---|
| `.env` present and loaded | parsed key=value, never sourced; the environment overrides the file, like docker compose |
| `.env` permissions | expected `600` or `400` (it contains the token and the Postgres passwords) |
| required variables | `POSTGRES_USER`, `POSTGRES_PASSWORD`, `POSTGRES_DB`, `APP_DB_PASSWORD`, `MIGRATION_DATABASE_URL`, `DATABASE_URL`, `TELEGRAM_BOT_TOKEN` (cf. `.env.example`) |
| `OWNER_TELEGRAM_USER_ID` | Phase 1 mono-tenant guard; **fails if empty** outside local dev |
| `BACKUP_RETENTION_DAYS` | integer; absent ⇒ `backup.sh` applies 14 days |
| distinct DSNs | `DATABASE_URL ≠ MIGRATION_DATABASE_URL`, same rule as `config.Load()` |
| disk space | threshold `PREFLIGHT_MIN_DISK_GB` (default 2 GB) on the repository FS |
| `./backups` and `./media` | present and writable (compose bind mounts) |
| PostgreSQL roles | owner role reachable; `undelete_app` exists, `NOSUPERUSER` and `NOBYPASSRLS` |
| Telegram token | `getMe` on api.telegram.org; **the token is never displayed**, any API output is masked |

A `[SKIP]` is not blocking: it signals a check **not performed** (missing
tool, unreachable database), to be re-run from a place that allows it.

Adjustable disk threshold:

```bash
PREFLIGHT_MIN_DISK_GB=10 sh scripts/preflight.sh
```

**Re-run the role check from the Docker network.** From the VM, the DSNs in
`.env` point to the `postgres` host of the compose network and do not resolve:
the check exits with `[SKIP]`. Once the stack is up, re-run it from the
Postgres container, which embeds `psql`:

```bash
docker compose exec -T postgres \
  sh -c 'psql -U "$POSTGRES_USER" -d "$POSTGRES_DB" -tAX -f -' <<'SQL'
SELECT rolname, rolsuper, rolbypassrls FROM pg_roles
 WHERE rolname IN ('undelete_app', current_user);
SQL
```

> **`$POSTGRES_USER` and `$POSTGRES_DB` are resolved inside the container**,
> hence the `sh -c '…'` in single quotes. On the VM side these variables are
> not defined: `docker compose` reads `.env` for itself and exports nothing to
> the operator's shell. A command that lets them expand on the host reduces to
> `psql -U "" -d ""` and fails to connect. Same remark applies to all the
> `psql` blocks in this runbook.

Expected: `undelete_app|f|f`. If `undelete_app` is missing, it means
`db/init/01-app-role.sh` has not run — it only runs on the **first** start of
the `postgres_data` volume, never on an already-initialized volume.

### 1.2 Manual checklist

- [ ] `git status` clean and expected branch/tag (`git log --oneline -1`).
- [ ] `.env` at `600`, owner = the user who runs compose.
- [ ] Disk space: `df -h .` — plan for the database **plus** the dump retention.
- [ ] `docker compose config` reports no unsubstituted variable.
- [ ] `make check` green (build + vet + gofmt) on the commit to deploy.
- [ ] Recent backups present: `ls -lh backups/ | tail -5`.
- [ ] Bot still connected on the Telegram side (Business Mode active, cf. README).
- [ ] Deployment window accepted: during the rollout, deleted messages are
      **not** caught up retroactively (the Bot API does not replay history) —
      unconsumed updates nevertheless stay queued on the Telegram side for up
      to 24 h and are processed at restart.

---

## 2. Deployment / update procedure

**Non-negotiable order: backup → migration → rollout → verification.**

### Step 1 — Backup

The `backup` service of the compose runs in a loop (one immediate dump then
every 24 h). Before a deployment, force a fresh dump **now**:

```bash
docker compose exec -T -e BACKUP_DIR=/backups backup sh /scripts/backup.sh
ls -lh backups/ | tail -3
```

The script writes `backups/undelete-<UTC timestamp>.sql.gz`, then purges
archives older than `BACKUP_RETENTION_DAYS` days (files only). A failed
`pg_dump` does not leave a truncated archive: the `trap` removes it.

> **`-e BACKUP_DIR=/backups` is not optional.** The service loop sets this
> variable in its own `entrypoint`; an `exec` session is a fresh process that
> does not inherit it. Without it, `backup.sh` falls back to its default
> `./backups`, resolved from the container's `working_dir` (`/backups`):
> the dump would go into `./backups/backups/` and the retention purge would
> apply to that subdirectory, letting the real archives accumulate.

If the stack is stopped, start Postgres alone first:

```bash
docker compose up -d postgres
docker compose exec -T -e BACKUP_DIR=/backups backup sh /scripts/backup.sh
```

> **Do not deploy without a fresh dump.** Step 2 applies migrations with the
> owner role; this is the only moment where the schema can change in a
> non-reversible way.

Note the dump name: it is the rollback point of §3.3.

### Step 2 — Migration

Migrations are not run separately: `bot/cmd/bot/main.go` calls
`storage.RunMigrations` with `MIGRATION_DATABASE_URL` (owner role)
**before** opening the application pool (`DATABASE_URL`, role `undelete_app`,
without DDL rights). They therefore run at the boot of step 3.

Before deploying, look at what is going to be applied:

```bash
ls bot/internal/storage/migrations/
git diff --stat <deployed_commit>..HEAD -- bot/internal/storage/migrations/
```

A **destructive** migration (`DROP`, `ALTER ... DROP COLUMN`, `TRUNCATE`,
type change with data loss) requires re-reading §3.2 and §3.3 *before* the
rollout: the rollback becomes a dump restoration, not a simple image revert.

The application check is done after step 3, in the JSON logs:

```bash
docker compose logs bot | grep '"msg":"migration applied"'
```

Each line carries `version` and `name`. No line = no new migration to apply
(the normal case for a deployment without schema change). A migration failure
makes the binary exit with code 1 and
`"msg":"stopping after fatal error"`: the application pool is never opened,
so the bot **never** runs on a partially migrated schema.

### Step 3 — Rollout

```bash
git pull --ff-only
docker compose up --build -d      # equivalent: make up
docker compose ps
```

`--build` rebuilds the bot image from `bot/Dockerfile` (the compose builds
locally, there is no registry: `docker compose pull` only fetches
`postgres:16-alpine`). `up -d` recreates only the services whose definition or
image changed; Postgres is not restarted if nothing concerns it, and its
volume is preserved in all cases.

`docker compose ps` must show `postgres` *healthy* and `bot` *running*. The
bot waits for `service_healthy` on Postgres: a slightly slow start is normal.

### Step 4 — Verification

**a. HTTP probes** *(after #6)* — on `:9090`:

```bash
curl -fsS http://localhost:9090/livez  && echo " livez OK"
curl -fsS http://localhost:9090/readyz && echo " readyz OK"
curl -fsS http://localhost:9090/metrics | head -20
```

`/livez` = process alive; `/readyz` = migrations done, application pool open
and poller started. A persistently red `/readyz` while `/livez` is green ⇒
look at the database before touching the bot.

*Before #6*, the equivalent check is read in the logs (below) and via
`docker compose ps` (state `running`, no restart loop:
`docker compose ps --format '{{.Name}} {{.Status}}'`).

**b. Logs**:

```bash
docker compose logs -f bot        # equivalent: make logs
```

Expected at boot, in order:

- `"msg":"migration applied"` (only if there were any),
- `"msg":"poller starting"` with `allowed_updates` containing the four
  types `business_connection`, `business_message`, `edited_business_message`,
  `deleted_business_messages`.

To watch for: `"level":"ERROR"`, in particular `outbox: processing failed`
(undelivered alerts) and `retention purge: failed`. Logs never contain
message content: identifiers, types and counters only.

**c. End-to-end synthetic test** — the only check that proves the whole chain
works:

The `psql` blocks below are deliberately at the margin: the `heredoc`
delimiter must stay in column 0 to be copy-pasteable as-is.

**1.** From a second Telegram account, send a message in a private
conversation covered by the Business connection.

**2.** Verify its recording (counter, without reading the content). Replace
`<owner_id>` with the value of `OWNER_TELEGRAM_USER_ID` from `.env` — it does
not exist in the VM's shell (§1.1).

> **Tenant context, and what the command below really shows.**
> `messages`, `notification_outbox` and `chats` are in `FORCE ROW LEVEL
> SECURITY`, which applies the policy **even to the table owner**.
> But `POSTGRES_USER` is *superuser* in the official `postgres` image, and
> superuser like `BYPASSRLS` bypasses RLS **even with `FORCE`** (cf.
> `bot/internal/storage/migrations/0001_init.sql`). From this container, the
> `count(*)` is therefore global, across all tenants: in mono-tenant Phase 1
> that is the expected result, and a `0` really means "no message
> captured", not "context not set".
>
> The `set_config` is kept because it is correct and has no side effect
> here, and is **essential** as soon as these queries are re-run with a
> non-superuser role — `undelete_app`, or an owner without `BYPASSRLS`:
> there, a `SELECT count(*)` without context returns `0` **without error**, a
> zero that means nothing. It is set in the same transaction, with `users.id`
> (surrogate key) and **not** the `telegram_user_id`: `owner_user_id`
> references `users(id)`.

```bash
docker compose exec -T postgres \
  sh -c 'psql -U "$POSTGRES_USER" -d "$POSTGRES_DB" -tAX --single-transaction -f -' <<'SQL'
SELECT set_config('app.current_owner_user_id',
                  (SELECT id::text FROM users
                   WHERE telegram_user_id = <owner_id>), true);
SELECT count(*) FROM messages WHERE saved_at > now() - interval '5 minutes';
SQL
```

`--single-transaction` is not decorative: `set_config` is set to `LOCAL`
(3rd argument `true`) and does not survive the transaction. Without it, `psql -f`
executes each statement in its own transaction and the context is lost before
the `count(*)`. A `set_config` returning an empty row means the user does not
exist in the database yet — in that case, that is the real test result.

**3.** Delete this message from the second account.

**4.** **Wait for the bot's alert on the account holder's account** (a few
seconds: the outbox worker runs every second). The alert must carry the chat,
the sender, the type, the UTC date and the restored content.

**5.** Verify that no outbox job stays stuck (same context as above,
`notification_outbox` is also in `FORCE RLS`):

```bash
docker compose exec -T postgres \
  sh -c 'psql -U "$POSTGRES_USER" -d "$POSTGRES_DB" -tAX --single-transaction -f -' <<'SQL'
SELECT set_config('app.current_owner_user_id',
                  (SELECT id::text FROM users
                   WHERE telegram_user_id = <owner_id>), true);
SELECT status, count(*) FROM notification_outbox GROUP BY status;
SQL
```

`sent` expected; persistent `failed` or `processing` rows signal a send
problem on the Telegram side.

An alert received **twice** is not a bug: delivery is at-least-once by design
(cf. README).

**The deployment is considered successful only if a + b + c are green.**

---

## 3. Rollback

Reverse strategy of §2: first revert the code, touch the database only as a
last resort.

### 3.1 Return to previous code (default case)

```bash
git log --oneline -5                    # identify the stable commit
git checkout <stable_commit>            # or: git revert <faulty_commit> then push
docker compose up --build -d
docker compose logs -f bot
```

Then re-run the §2 step 4 verifications.

- **Incident detected after merge**: prefer `git revert <faulty_commit>`
  (history stays linear and pushable, no force-push — forbidden).
- **Incident in progress, immediate return needed**: `git checkout
  <stable_commit>` on the VM (detached state), then regularize with a `revert`
  on `main` when things are calm.
- **Previous image still present**: `docker image ls | grep undelete` then
  relaunch the earlier tag if the rebuild is too slow. Do not delete images
  during an incident.

### 3.2 When NOT to roll back the database

In **the vast majority of cases, we do not restore the database**. The
migrations in this project are additive (`CREATE TABLE`, `ADD COLUMN`,
constraints): an earlier version of the bot runs fine on a newer schema — the
extra columns are simply ignored.

Restoring the database would then **lose every message captured since the
dump**, i.e. exactly what the product is supposed to protect. Do not restore
if:

- the migration was additive (no `DROP` / `TRUNCATE` / type loss);
- the bug is on the code side, not the data side;
- the incident is an unavailability (bot in a crash loop): §3.1 is enough.

### 3.3 Database restoration (last resort)

**Only if** a destructive migration deleted or converted data, or if the
database is corrupted.

> ⛔ **Forbidden without explicit confirmation from Louis.** A restoration
> overwrites the current state and loses any data after the dump.

Detailed procedure: **`docs/backup-restore.md` and `make test-restore`**
*(after #7)* — these are the references to follow, including to validate the
dump **before** applying it.

Imposed order, whatever the path:

1. Stop the bot only, keep Postgres running:
   `docker compose stop bot` (never `down -v`).
2. Take a dump of the **current** state, even degraded
   (`docker compose exec -T -e BACKUP_DIR=/backups backup sh /scripts/backup.sh`): it lets you
   go back if the restoration goes badly.
3. Restore the chosen dump into a **verification** database first, never
   directly into production (this is what `make test-restore` automates).
4. Explicit confirmation from Louis, then restoration in production.
5. Restart the bot (`docker compose up -d bot`) and re-run the
   §2 step 4 verifications, synthetic test included.

---

## 4. Secret rotation

No rotation requires deleting a volume. **`docker compose down -v`
and `docker volume rm/prune` remain forbidden (§0).**

### 4.1 Telegram token (`TELEGRAM_BOT_TOKEN`)

1. **Revoke/regenerate** via [@BotFather](https://t.me/BotFather):
   `/mybots` → the bot → *API Token* → *Revoke current token*. The old token
   stops working **immediately**: from that moment the bot receives nothing.
   Do the rotation in a short, accepted window.
2. Update `.env` (`TELEGRAM_BOT_TOKEN=`), keep `chmod 600 .env`.
3. Validate the new token **before** redeploying:
   `sh scripts/preflight.sh` (the `getMe` check must be `[ OK ]`, and the token
   does not appear in any output).
4. Propagate: `docker compose up -d bot` (container recreation — a plain
   `restart` does not re-read `.env`).
5. Verify `"msg":"poller starting"` in the logs, then redo the synthetic
   test §2 step 4 c.
6. Verify that Business Mode is still active and the Business connection
   still in place on the account holder's side (README, steps 2 and 3).

### 4.2 Application role password (`APP_DB_PASSWORD`)

Order designed to **never lock ourselves out**: change the password
server-side with the owner role (always reachable), then align the DSN.

```bash
# 1. Rotation on the Postgres side, with the owner role.
#    Enter the new password via \password: it appears neither in the
#    shell history nor in the Postgres logs (unlike a plaintext
#    ALTER ROLE ... PASSWORD '...').
docker compose exec postgres \
  sh -c 'psql -U "$POSTGRES_USER" -d "$POSTGRES_DB" -c "\password undelete_app"'
```

No `-T` here: `\password` is a `psql` meta-command that prompts for input, it
needs a TTY.

2. Update `.env`: `APP_DB_PASSWORD` **and** the password embedded in
   `DATABASE_URL` (both, otherwise the bot no longer connects).
3. `sh scripts/preflight.sh` (DSNs still distinct, variables present).
4. `docker compose up -d bot`, then verify the absence of connection error
   in the logs and the synthetic test §2 step 4 c.

The running bot keeps its open connections until it is recreated: the
unavailability window is limited to the restart.
`db/init/01-app-role.sh` is **not** replayed (volume already initialized) —
the rotation is purely an `ALTER ROLE`, not a reset.

### 4.3 Owner role password (`POSTGRES_PASSWORD`)

Same logic, one notch more delicate: this role runs migrations and backups.

1. `docker compose exec postgres sh -c 'psql -U "$POSTGRES_USER" -d
   "$POSTGRES_DB" -c "\password $POSTGRES_USER"'` (without `-T`, `\password`
   prompts for input).
2. Update `.env`: `POSTGRES_PASSWORD` **and** the password in
   `MIGRATION_DATABASE_URL`.
3. `docker compose up -d bot backup` (the `backup` service also uses
   `MIGRATION_DATABASE_URL`: forgetting to recreate it would silently break
   the next day's backups).
4. Immediate check: `docker compose exec -T -e BACKUP_DIR=/backups backup sh /scripts/backup.sh`
   must produce a new archive.

> `POSTGRES_PASSWORD` in the compose only serves the **initialization** of the
> volume. Modifying it in `.env` changes nothing server-side on an existing
> volume: the `ALTER ROLE` of step 1 is the only operation that matters.
> Never "fix" a desynchronization by recreating the volume (§0).

### 4.4 After any rotation

- [ ] `git status`: `.env` untracked (it is in `.gitignore`), no secret in
      the diff.
- [ ] Old secret revoked on the issuer side (BotFather), not just replaced
      in `.env`.
- [ ] Probes/logs green and synthetic test passed again.

---

## 5. Staging recipe

There is no permanent staging environment. The recipe relies on the existing
test infrastructure, which creates its own **ephemeral** containers and does
not touch any existing Docker resource.

### 5.1 Before each deployment

```bash
make check              # build + go vet + gofmt
make test-integration   # throwaway Postgres 16: migrations, RLS, isolation, outbox
```

`scripts/test-integration.sh` starts a PostgreSQL 16 container without a
volume and deletes only that container. It refuses to work on a database whose
name is not exactly `undelete_integration` and requires the literal
destructive opt-in in external mode (README) — two guardrails never to bypass
to "test faster" on the production database.

### 5.2 Periodic recipe

```bash
sh scripts/preflight.sh   # configuration drift, disk, token validity
make test-restore         # (after #7) restoration of a real dump into a throwable database
```

A backup that has never been restored is not a backup. `make test-restore`
is the check that turns `./backups` into a real safety net.

**Suggested homelab cron** (non-root user owning the repository):

```cron
# Daily preflight: config drift, disk, token still valid.
15 6 * * *  cd /srv/undelete && sh scripts/preflight.sh >> /var/log/undelete-preflight.log 2>&1

# Weekly recipe: integration suite + restoration of a real dump.
30 3 * * 0  cd /srv/undelete && make test-integration >> /var/log/undelete-recipe.log 2>&1
45 3 * * 0  cd /srv/undelete && make test-restore     >> /var/log/undelete-recipe.log 2>&1
```

On NixOS, the declarative equivalent (`services.cron.systemCronJobs` or a
`systemd.timers`) is preferable to a manual `crontab -e`. Adapt the `/srv/undelete`
path to the actual location of the repository on the VM.

---

## 6. Media retention — first rollout

The daily retention cycle now deletes **files under `./media`**, not just rows.
That is the one operation in this project with no safety net: the backups cover
the database and never `./media` (§0), so an unlinked blob is gone. Postgres can
be restored from last night's dump; a photo cannot.

Hence the rollout, once, on the first deployment that carries the media purge:

1. **Dry run for one cycle.** In `.env`:
   ```
   MEDIA_PURGE_DRY_RUN=true
   ```
   Only a real boolean is accepted; a typo fails at startup rather than silently
   disabling retention. At boot the logs carry
   `media retention purge running in DRY RUN: no file will be deleted`.
2. **Wait for one pass.** The retention loop is on a 24h ticker and does *not*
   fire at boot: the first summary line appears one day after the rollout. Read
   it in `docker compose logs bot`, on the `retention purge complete` line:

   | Counter | Read it as |
   |---|---|
   | `media_files_deleted` | blobs the purge WOULD have deleted. Compare with the volume of media older than `retention_days`; an order of magnitude too high means the media root does not point where you think. |
   | `media_orphans_deleted` | unreferenced files older than 24h. A large number on a first run is normal (crash leftovers); a large number on every run is a bug worth reporting. |
   | `media_refused` | entries the purge declined to touch: a symlink, something that is not a plain file, a path resolving outside the root. **Never expected to be non-zero.** Investigate before switching the dry run off — the corresponding rows are deliberately left alone, nothing is lost by waiting. |
   | `media_requeued` | files missing while still within retention, sent back to the download queue. |
   | `media_rows_purged` / `media_rows_deleted` / `media_pending_deleted` | catalogue side only, no file involved. |

   A `media purge: retention stopped at its per-run bound` line means the tenant
   has more expiring media than one pass can absorb: the purge resumes the next
   day, and only sustained repetition is a problem.
3. **Switch it off** (`MEDIA_PURGE_DRY_RUN=false`, or remove the line) and
   `docker compose up -d`. Leaving the dry run on is not a safe default: a
   retention that never runs is a silent breach of the promise made to the
   owner, and nothing else in the logs says so.

The purge is idempotent and interruptible: it always unlinks the blob **before**
marking the row purged, so a crash between the two leaves only a mismatch that
the next pass repairs on its own. Restarting the bot is always a valid answer.

---

## Cheat sheet

| Need | Command |
|---|---|
| Preflight | `sh scripts/preflight.sh` |
| Immediate backup | `docker compose exec -T -e BACKUP_DIR=/backups backup sh /scripts/backup.sh` |
| Deploy / update | `make up` (`docker compose up --build -d`) |
| Logs | `make logs` (`docker compose logs -f bot`) |
| Service state | `docker compose ps` |
| Stop (volume preserved) | `make down` (`docker compose down`, **without `-v`**) |
| Build + lint | `make check` |
| Integration tests | `make test-integration` |
| Restore recipe | `make test-restore` *(after #7)* |
| Media purge, first rollout | `MEDIA_PURGE_DRY_RUN=true` in `.env`, then §6 |
| Probes | `curl -fsS localhost:9090/{livez,readyz,metrics}` *(after #6)* |
