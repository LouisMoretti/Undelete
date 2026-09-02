# undelete

Anti-delete Telegram bot connected via **Telegram Business / Business
Automation** (connected business bot — not a classic group bot, and certainly
not an MTProto userbot).

As soon as the account holder connects the bot to their Telegram Business
account, the bot automatically saves messages from **all private
conversations that this Business connection gives it access to**. There is no
conversation selector on the `undelete` side: no list of chats to check, no
allowlist, no per-conversation preference. When Telegram reports a deletion,
the bot retrieves the original content from the database (saved at the time
of reception, because the deletion event does not carry the content) and
notifies the account holder.

## Scope of this phase (Phase 1)

Mono-tenant, text-only messages, plaintext content in the database. The schema
is already multi-tenant and under Row Level Security (RLS) to prepare for the
following phases. Media, encryption and GDPR commands are marked
`// TODO Phase N` in the code, not implemented.

## Telegram setup (3 steps)

1. **Create the bot** via [@BotFather](https://t.me/BotFather): `/newbot`,
   retrieve the token (`TELEGRAM_BOT_TOKEN`).
2. **Enable Business Mode** in BotFather: `/mybots` → select the bot → *Business
   Mode* → *Turn on*. **Without this step, Telegram refuses any Business
   connection attempt** — the bot simply does not appear in the list of
   available chatbots.
3. **Connect the bot** from the account holder's Telegram account:
   *Settings* → *Telegram Business* → *Chatbots* → select the bot.
   This is the step that triggers the `business_connection` update received by
   the bot.

> Honest note: the *Telegram Business* entry in settings was long reserved for
> Telegram Premium accounts. The current MTProto documentation indicates that
> connecting a *bot* to Business would no longer require Premium on the side of
> the user connecting the bot — verify this yourself on a real non-Premium
> account before relying on it in production; the documentation and the app's
> actual behavior sometimes diverge.

## Getting started

```bash
cp .env.example .env
# edit .env: TELEGRAM_BOT_TOKEN, Postgres passwords, etc.

docker compose up --build -d
docker compose logs -f bot
```

At boot, the binary applies migrations with the owner DSN
(`MIGRATION_DATABASE_URL`) then opens the application pool with the restricted
DSN (`DATABASE_URL`, role `undelete_app`), before starting long polling.

## Supervision (probes and metrics)

The bot opens a dedicated HTTP server on `HEALTH_ADDR` (default `:9090`, empty
value = server disabled). This port stays internal to the Docker network: it
is not published on the host, just like Postgres.

| Route | Response |
| --- | --- |
| `GET /livez` | `200 {"status":"ok"}` as soon as the process serves HTTP, no external dependency. |
| `GET /readyz` | `200` if the database responds (ping, 2 s) **and** if the last successful `getUpdates` is less than 90 s old; otherwise `503 {"status":"degraded","checks":{...}}`. |
| `GET /metrics` | Prometheus text exposition. |

The Compose healthcheck of the `bot` service queries `/livez` and not `/readyz`:
liveness must depend on neither Postgres nor Telegram, otherwise an external
incident would restart a perfectly healthy bot in a loop.

Exposed metrics (counters, except the last one which is a gauge):
`undelete_updates_total`, `undelete_update_errors_total`,
`undelete_outbox_retries_total`, `undelete_outbox_failed_total`,
`undelete_deletions_total`, `undelete_outbox_backlog`.

`undelete_outbox_failed_total` counts alerts abandoned permanently
(non-replayable 4xx, or exhausted attempts). They leave
`undelete_outbox_backlog`, which only counts `pending`/`processing`: without
this counter, a wave of failures would read as a simple backlog decrease.

No series has a label, and the list of names is hard-coded: cardinality is
bounded by construction and no identifier, name, message text or token can end
up in a scrape. `/readyz` responses follow the same rule: degraded checks are
described by short, fixed reasons (`unreachable`, `stale`,
`no_successful_poll_yet`), never by the PostgreSQL error message, which would
contain the DSN.

## Operations

The homelab deployment is fully manual: **[`docs/runbook.md`](docs/runbook.md)**
is the reference procedure (preflight, backup → migration → rollout →
verification order, rollback, secret rotation, staging recipe), and the closed
list of destructive actions forbidden without explicit confirmation.

Before any deployment:

```bash
sh scripts/preflight.sh
```

Read-only check of required variables, `.env` permissions, disk space, the
distinction between the two DSNs, PostgreSQL roles (`undelete_app` without RLS
bypass privilege) and the validity of the Telegram token via `getMe` — the
token is never displayed. Exits with code 1 if any check fails.

The dump restoration procedure is documented separately in
`docs/backup-restore.md` (delivered by PR #40, the bottom layer in the stack — exists as soon as the stack is merged).

## PostgreSQL 16 integration tests

The real suite (no mocks) verifies migrations and their re-run, the runtime
role, refusals of dangerous roles and DDL, fail-closed RLS, CRUD isolation of
two tenants and `PurgeExpired` tenant by tenant.

```bash
make test-integration
```

The command starts a throwaway PostgreSQL 16 container, without a Docker
volume, then removes only that container at the end. It does not prune or
modify any existing Docker resource. If Docker is not available, it fails
explicitly and accepts instead two DSNs pointing to a local PostgreSQL 16
instance prepared with `db/init/01-app-role.sh`. This external mode refuses
any operation as long as the database reported by `current_database()` is not
exactly named `undelete_integration` and the literal destructive opt-in is not
provided:

```bash
POSTGRES_INTEGRATION_ADMIN_DSN='postgres://...' \
POSTGRES_INTEGRATION_RUNTIME_DSN='postgres://undelete_app:...' \
POSTGRES_INTEGRATION_ALLOW_DESTRUCTIVE=I_UNDERSTAND_THIS_WILL_DELETE_DATA \
make test-integration
```

The Docker recipe sets this opt-in itself, only for its ephemeral container and
its dedicated database.

The script also wires `OUTBOX_TEST_DATABASE_URL` (runtime DSN) and
`OUTBOX_TEST_MIGRATION_DATABASE_URL` (admin DSN) to the same ephemeral
database, so that the outbox tests in `bot/internal/outbox` — gated by these
variables and otherwise silently skipped — run in the same execution, after the
migrations laid down by the `bot/integration` suite. Values provided by the
caller are respected as-is.

## CI

`.github/workflows/ci.yml` runs on every `pull_request` and every `push` to
`main`, with two jobs:

- **`lint + unit tests`**: `gofmt -l` (fails on output), `go vet ./...`,
  `go test ./...` on the `bot/` module.
- **`PostgreSQL 16 integration`**: `make test-integration`, which starts its
  own throwaway PostgreSQL 16 container on the runner and runs the integration
  suite then the outbox tests.

No secret is used, no real database is exposed: only ephemeral containers with
throwaway credentials. The `GITHUB_TOKEN` token has `contents: read`.

**Branch protection to enable on the GitHub side** (*Settings → Branches → Add
branch ruleset* / *Add rule* on `main`) — not automatable from this repository:

1. *Require a pull request before merging*.
2. *Require status checks to pass before merging*, then select the two checks
   `lint + unit tests` and `PostgreSQL 16 integration` (they only appear in the
   list after the workflow has run once).
3. *Require branches to be up to date before merging*.
4. Forbid force-pushes on `main`.

## Architecture

```
                    getUpdates (long polling, explicit allowed_updates)
                              │
                              ▼
                    ┌───────────────────┐
                    │  telegram.Poller  │  sequential, backoff, offset
                    └─────────┬─────────┘  advances even if the handler fails
                              │ Update
                              ▼
                    ┌───────────────────┐
                    │   app.Handler     │  routes by update type
                    └─────────┬─────────┘
                              │
          ┌───────────────────┼───────────────────┐
          ▼                   ▼                   ▼
  business.Service    messages.Repository   outbox.Worker
  (resolution:         (InTenant + RLS)      (lease + backoff,
  cache→DB→API)         deleted_at + outbox   sendMessage without
                         atomically)           business_connection_id)
                              │
                              ▼
                        PostgreSQL 16
  users / business_connections / chats / messages / notification_outbox
             (FORCE RLS on content and per-tenant outbox)
```

- **`db/init/01-app-role.sh`** creates the application role `undelete_app`
  (`NOSUPERUSER NOCREATEDB NOCREATEROLE NOBYPASSRLS`) on the first start of
  the Postgres container.
- **`storage.RunMigrations`** applies `internal/storage/migrations/*.sql`
  with the owner DSN, at boot, before opening the application pool.
- **`storage.DB.InTenant`** is the only legitimate entry point to the
  `chats`, `messages` and `notification_outbox` tables: it sets
  `app.current_owner_user_id` to `LOCAL` (transaction scope) before any
  query.
- **Durable outbox**: `deleted_at` and the notification chunks are written in
  the same transaction. A unique constraint absorbs redeliveries.
  The worker picks up `pending` jobs or expired `processing` leases,
  respects `retry_after` on 429 and applies exponential backoff on
  network errors and 5xx. The `pending`, `processing`, `sent` and `failed`
  states remain observable without logging the payload.
- **Readable alerts**: each deletion notification carries the chat (its
  label when known, its `chat_id` always), the sender, the type and
  the send date in UTC, before the restored content. The label comes from the
  `chats` table, written on every message received — it is a DISPLAY label,
  never a filter (cf. pitfall n°8). A chat without a known label displays
  "chat &lt;id&gt;".

### Alert delivery guarantees

- **At-least-once, not exactly-once.** The worker sends the alert to Telegram
  *then* acknowledges the outbox row. A crash in between leaves the row in
  `processing`; its lease expires and a worker picks it up again. **An alert
  already received can therefore be delivered a second time.** No deduplication
  is done on the Telegram side: the reverse (acknowledging before sending)
  would trade this duplicate for a lost alert, which is unacceptable for this
  product.
- **Order guaranteed within a message, not between messages.** The chunks of a
  single deleted message (same `business_connection_id`, `chat_id`,
  `message_id`, `event_type`) leave in `chunk_index` order: `Claim`
  refuses a chunk as long as a lower-index chunk is not `sent` or
  `failed`. **No order is guaranteed BETWEEN two different messages** — a
  message rescheduled by a backoff can arrive after a message deleted later.
  Alerts carry the chat identifier, never a sequence number: do not rely on
  their order of arrival.
- **Single clock.** Retry deadlines and lease expirations are
  entirely evaluated and written by PostgreSQL (`clock_timestamp()`). A
  drift between the bot's clock and the database's can neither hide a job
  nor make it claimable too early.

## The pitfalls (non-negotiable constraints)

1. **Explicit `allowed_updates`** in `getUpdates`:
   `business_connection`, `business_message`, `edited_business_message`,
   `deleted_business_messages`. Without it, Telegram sends nothing at all —
   no error, just silence.
2. **Two Postgres roles, two DSNs.** `config.Load()` refuses to start if
   `DATABASE_URL == MIGRATION_DATABASE_URL`: otherwise the bot would run with
   the owner role's privileges and RLS would be decorative.
3. **`FORCE ROW LEVEL SECURITY`** on `messages`. `ENABLE` alone does not
   apply to the table owner. No RLS on `business_connections`
   (resolution table, queried before the owner is known).
4. **`InTenant`** is the only path to `messages`. The retention purge
   cannot be a global `DELETE` through the bare pool: with `FORCE RLS` and
   no context set, the query would succeed and delete zero rows, without any
   error. `PurgeExpired` loops tenant by tenant.
5. **Sequential update processing.** A parallel worker pool could
   process a deletion before the corresponding message. Future
   scaling will use sharding on `chat_id`, never an unordered pool.
6. **`message_ids` is an array.** A batch deletion arrives in a single
   `deleted_business_messages` update.
7. **Alerts are sent FROM the bot**, without `business_connection_id`: that
   field would send the message *as* the account holder, in the monitored
   conversation.
8. **Exhaustive and automatic saving.** No table, command, environment
   variable or condition lets you choose which chats to record. `is_enabled`
   concerns the Business connection as a whole, never an individual
   conversation. The `chats` table is no exception to anything: it stores a
   label to make alerts readable, with no activation flag, and is never
   consulted to decide what to save or notify.

## Privacy

- Every private conversation exposed by the active Business connection is
  saved in full (text), with no per-conversation opt-out.
- **Important technical limitation**: the Bot API gives the bot neither
  retroactive access to the account history nor visibility into a conversation
  that Telegram does not explicitly expose to it via the Business connection.
  `undelete` keeps exhaustively what Telegram delivers to it through this
  connection **from its activation onward**, and nothing more — neither before,
  nor outside the scope that Telegram decides to transmit.
- Logs (`log/slog`, JSON) never contain message content: identifiers, types
  and counters only.
- Retention configurable per user (`retention_days`, 1 to 365 days), purged
  daily.
- Database backups (`scripts/backup.sh`) do not cover `./media` (Phase 2). The
  backup retention duration (`BACKUP_RETENTION_DAYS`) is, in effect, the
  residual survival time of data after a future `/delete_my_data` command:
  rows deleted in the database remain present in already-written archives
  until their own purge. To be documented explicitly in a future `/privacy`
  command.

## Roadmap by phases

- **Phase 1 (this task)**: mono-tenant, plaintext text, RLS in place.
- **Phase 2**: media (`media_files` table, backup of `./media` separately
  from SQL dumps), GDPR commands (`/delete_my_data`, `/privacy`).
- **Phase 3**: real multi-tenancy (several simultaneous account holders,
  removal of the `OWNER_TELEGRAM_USER_ID` guard).
- **Phase 4**: content encryption (`text_encrypted BYTEA`, AES-256-GCM,
  per-tenant key) replacing plaintext `text_content`.
