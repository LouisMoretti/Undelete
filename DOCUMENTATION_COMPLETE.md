# Undelete - Complete Documentation

> **Undelete** is a Telegram anti-deletion bot using the **Telegram Business** API to automatically save messages from private conversations and notify the owner in case of deletion.

---

## 📌 Table of Contents

1. [Global Summary](#-global-summary)
2. [Technical Architecture](#-technical-architecture)
3. [Key Components and Their Role](#-key-components-and-their-role)
   - [cmd/bot/main.go](#1️⃣-cmdbotmaingo)
   - [internal/telegram/](#2️⃣-internaltelegram)
   - [internal/app/handler.go](#3️⃣-internalapphandlergo)
   - [internal/business/service.go](#4️⃣-internalbusinessservicego)
   - [internal/messages/repository.go](#5️⃣-internalmessagesrepositorygo)
   - [internal/outbox/](#6️⃣-internaloutbox)
   - [internal/storage/](#7️⃣-internalstorage)
   - [internal/users/repository.go](#8️⃣-internalusersrepositorygo)
   - [internal/config/config.go](#9️⃣-internalconfigconfiggo)
   - [internal/health/health.go](#🔟-internalhealthhealthgo)
   - [internal/metrics/metrics.go](#🔟-internalmetricsmetricsgo)
4. [Database (PostgreSQL 16)](#-database-postgresql-16)
5. [Data Flow](#-data-flow)
6. [Security and Non-Negotiable Constraints](#-security-and-non-negotiable-constraints)
7. [Metrics and Monitoring](#-metrics-and-monitoring)
8. [Testing](#-testing)
9. [Deployment](#-deployment)
10. [Roadmap](#-roadmap)
11. [Why "99% test coverage"?](#-why-99-test-coverage)

---

## 📌 Global Summary

**Undelete** is a bot that:
1. **Connects** to a Telegram Business account via the official API.
2. **Automatically saves** all messages from private conversations accessible through this connection.
3. **Detects message deletions** and **notifies the owner** by restoring the original content (because the deletion event does not carry the content).
4. **Manages data retention** (configurable per user, between 1 and 365 days).
5. **Guarantees multi-tenant isolation** (even though Phase 1 is mono-tenant) via **Row Level Security (RLS)** in PostgreSQL.

---

## 🏗️ Technical Architecture

The project is organized into **3 main parts**:
1. **`bot/`**: Go module (bot backend).
2. **`db/`**: Database initialization scripts (restricted PostgreSQL role).
3. **`scripts/`**: Operations scripts (backup, tests, deployment).
4. **`docs/`**: Documentation (runbook, backup-restore, etc.).

### Go Module Structure (`bot/`)

```
bot/
├── cmd/bot/
│   └── main.go          # Binary entry point
├── internal/
│   ├── app/             # Telegram update routing → business handling
│   ├── business/        # Telegram Business connection management
│   ├── config/          # Configuration loading/validation
│   ├── health/          # Supervision probes (/livez, /readyz, /metrics)
│   ├── messages/        # Message saving/retrieval (with RLS)
│   ├── metrics/         # Prometheus counters (no sensitive labels)
│   ├── outbox/          # Durable queue for Telegram alerts
│   ├── storage/         # PostgreSQL connection + migrations + InTenant (RLS)
│   ├── telegram/        # Telegram API client (long polling, alert sending)
│   └── users/           # User (tenant) management
└── integration/        # PostgreSQL integration tests
```

---

## 🔧 Key Components and Their Role

---

### 1️⃣ `cmd/bot/main.go`

- **Role**: Bot entry point.
- **Features**:
  - Loads the configuration (`config.Load()`).
  - Applies **migrations** (with the owner DSN, `MIGRATION_DATABASE_URL`).
  - Opens the **application pool** (with the restricted `undelete_app` role, `DATABASE_URL`).
  - Starts **4 goroutines**:
    1. **`Poller`** (Telegram long polling) → Processes updates **sequentially** (constraint #5).
    2. **`Outbox Worker`** → Sends deletion alerts from `notification_outbox`.
    3. **`Retention Loop`** → Purges expired messages/alerts (every 24h).
    4. **`Backlog Loop`** → Updates the `undelete_outbox_backlog` gauge (every 15s).
  - Starts the **HTTP server** for the probes (`/livez`, `/readyz`, `/metrics`).

---

### 2️⃣ `internal/telegram/`

#### 📌 `client.go`
- **Role**: Minimal HTTP client for the **Telegram Bot API** (no external dependency).
- **Features**:
  - `GetUpdates`: Long polling (timeout = 50s) with **explicit** `allowed_updates` (constraint #1).
  - `SendMessage`: Sends messages (with bounded retries for non-persisted errors).
  - `SendMessageOnce`: Sends without retry (used by the outbox to avoid duplicates).
  - `GetBusinessConnection`: Fetches a Business connection from the API (last resort if not found in cache/DB).

#### 📌 `poller.go`
- **Role**: **Long polling** loop (`getUpdates`).
- **Behavior**:
  - Processes updates **one by one** (sequential, no parallelism).
  - **Advances the offset even on error** (to avoid getting stuck on a poisoned update).
  - Handles **exponential backoff** (min: 1s, max: 1min) and respects `retry_after` (429).
  - Updates `lastSuccessUnixNano` for the `/readyz` probe.

#### 📌 `types.go`
- **Role**: Definition of the **data structures** for Telegram updates.
- **Key types**:
  - `Update`: Container for `business_connection`, `business_message`, `edited_business_message`, `deleted_business_messages`.
  - `BusinessConnection`: Business connection (with `User`, `UserChatID`, `IsEnabled`, `CanReply`).
  - `Message`: Telegram message (with `From`, `Chat`, `Text`, `BusinessConnectionID`).
  - `BusinessMessagesDeleted`: Message deletion (with `MessageIDs` **array**, constraint #6).
  - `SendMessageRequest`: Message send request (**without `business_connection_id`**, constraint #7).

#### 📌 `alerts.go`
- **Role**: Construction of **deletion alerts** (format and UTF-16 chunking).
- **Features**:
  - `BuildWelcomeMessageRequest`: Welcome message.
  - `BuildDeletionMessageRequests`: Splits the alert text into chunks ≤ 4096 UTF-16 units.
  - `buildDeletionText`: Formats the alert with:
    - Chat label (title or `@username` or `chat <id>`).
    - Sender (name + `user_id` if available).
    - Message type, UTC date, and content.

---

### 3️⃣ `internal/app/handler.go`

- **Role**: **Router** of Telegram updates to business handling.
- **Features**:
  - `HandleUpdate`: Routes to:
    - `business.HandleBusinessConnection` (new Business connection).
    - `saveMessage` (saving of a `business_message` or `edited_business_message`).
    - `handleDeleted` (processing of a deletion).
  - **Constraint #8**: **No filter** on `chat_id` or user preference → **everything is saved**.
  - **Logs**: Only **IDs, types and counters** are logged (never the content).

---

### 4️⃣ `internal/business/service.go`

- **Role**: Management of **Telegram Business connections**.
- **Features**:
  - **3-level resolution**:
    1. **In-memory cache** (for already-seen connections).
    2. **Database** (`business_connections`).
    3. **Telegram API** (`getBusinessConnection`) → **DB upsert** if new.
  - **Mono-tenant guard**: If `OWNER_TELEGRAM_USER_ID` is set, refuses connections from other users (`ErrOwnerMismatch`).
  - `HandleBusinessConnection`: Processes the `business_connection` update (upsert user + connection, sends a welcome message).
  - **No RLS** on `business_connections` (resolution table, queried before the `owner_user_id` is known).

---

### 5️⃣ `internal/messages/repository.go`

- **Role**: Saving and retrieval of **messages** (with RLS).
- **Features**:
  - `Save`:
    - **Upserts** a message (idempotent via `ON CONFLICT`).
    - **Upserts the chat label** (`chats` table) in the **same transaction**.
    - **Constraint #8**: **No condition** on `chat_id` → everything is saved.
  - `MarkDeleted`:
    - Marks `deleted_at` for deleted messages.
    - **Writes the alert chunks** into `notification_outbox` (same transaction).
    - Returns the messages **actually found** (for logs/metrics).
  - `PurgeExpired`:
    - Deletes **expired** messages (according to `retention_days`).
    - **Loops tenant by tenant** (constraint #4: a global `DELETE` is impossible with RLS).

---

### 6️⃣ `internal/outbox/`

#### 📌 `repository.go`
- **Role**: Management of the `notification_outbox` table (with RLS).
- **Features**:
  - `InsertTx`: Adds an alert chunk in a transaction (idempotent via `ON CONFLICT DO NOTHING`).
  - `Claim`:
    - **Claims a job** (`pending` or `processing` with `next_attempt_at <= now()`).
    - **Checks chunk order** (a chunk cannot be processed if a previous chunk is still `pending`/`processing`).
    - Uses `FOR UPDATE SKIP LOCKED` to avoid conflicts.
    - **PostgreSQL clock**: All dates (`clock_timestamp()`) are handled DB-side.
  - `MarkSent`/`MarkRetry`/`MarkFailed`: Updates the job status (with `lease_token` verification).
  - `CountBacklog`: Counts `pending`/`processing` jobs **tenant by tenant** (RLS).
  - `PurgeExpired`: Deletes expired `sent`/`failed` jobs.

#### 📌 `worker.go`
- **Role**: **Worker** that processes outbox alerts.
- **Features**:
  - `ProcessOne`:
    - **Claims a job** for a tenant.
    - **Sends the message** via `SendMessageOnce` (without `business_connection_id`).
    - **Handles errors**:
      - **429 (rate limit)**: Respects `retry_after`.
      - **Non-replayable 4xx**: Marks as `failed` (definitive abandonment).
      - **5xx/timeout**: Reschedules with **exponential backoff** (max: 15min).
      - **Exhausted attempts** (max: 5) → `failed`.
    - **At-least-once**: The job is marked `sent` **after** the send (a crash in between → possible redelivery).
  - **Metrics**:
    - `undelete_outbox_retries_total` (reschedules).
    - `undelete_outbox_failed_total` (definitive failures).

---

### 7️⃣ `internal/storage/`

#### 📌 `db.go`
- **Role**: Management of the **PostgreSQL connection** and **migrations**.
- **Features**:
  - `NewPool`:
    - Opens the application pool with the `undelete_app` role.
    - **Verifies the role is not superuser and has no `BYPASSRLS`** (otherwise RLS is decorative).
  - `InTenant`:
    - **Opens a transaction** and sets `app.current_owner_user_id` to **LOCAL** (transaction scope).
    - **Only legitimate entry point** for the `messages`, `chats`, `notification_outbox` tables (RLS).
  - `RunMigrations`:
    - Applies the **embedded** migrations (`migrations/*.sql`) with the owner DSN.
    - **Advisory lock** (`pg_advisory_lock`) to avoid races between replicas.
    - Each migration in its **own transaction**.

#### 📌 SQL Migrations

| Migration | Role |
|-----------|------|
| `0001_init.sql` | Initial schema: `users`, `business_connections`, `messages` (with RLS `FORCE`). |
| `0002_notification_outbox.sql` | `notification_outbox` table (durable outbox, RLS `FORCE`). |
| `0003_chat_labels.sql` | `chats` table (chat labels for alerts, RLS `FORCE`). |

---

### 8️⃣ `internal/users/repository.go`

- **Role**: Management of the `users` table (root of tenants).
- **Features**:
  - `UpsertByTelegramID`: Creates or updates a user (idempotent).
  - `ListTenantsForRetention`: Lists all tenants with their `retention_days` (for the purge).

---

### 9️⃣ `internal/config/config.go`

- **Role**: Loading and validation of the **configuration** (environment variables).
- **Required variables**:
  - `DATABASE_URL`: Application DSN (`undelete_app` role).
  - `MIGRATION_DATABASE_URL`: Owner DSN (superuser).
  - `TELEGRAM_BOT_TOKEN`: Bot token.
- **Validation**:
  - `DATABASE_URL != MIGRATION_DATABASE_URL` (otherwise RLS is decorative).
  - `HEALTH_ADDR`: Must be in `host:port` format or empty.
  - `OWNER_TELEGRAM_USER_ID`: Optional (mono-tenant guard).

---

### 🔟 `internal/health/health.go`

- **Role**: **Supervision probes** (`/livez`, `/readyz`, `/metrics`).
- **Features**:
  - `/livez`: **200 OK** as soon as the process responds (no external dependency).
  - `/readyz`: **200 OK** if:
    - The database responds (`Ping`).
    - The last successful `getUpdates` is **less than 90s** old (otherwise `stale`).
  - `/metrics`: **Prometheus** exposition (no sensitive labels).
  - **Degradation reasons**:
    - `unreachable` (dead database).
    - `stale` (poller too old).
    - `no_successful_poll_yet` (no successful poll since startup).

---

### 🔟 `internal/metrics/metrics.go`

- **Role**: **Prometheus counters** (without labels).
- **Exposed metrics**:

| Name | Type | Description |
|-----|------|-------------|
| `undelete_updates_total` | Counter | Total number of received updates. |
| `undelete_update_errors_total` | Counter | Update processing errors. |
| `undelete_outbox_retries_total` | Counter | Alert reschedules. |
| `undelete_outbox_failed_total` | Counter | Abandoned alerts (definitive failures). |
| `undelete_deletions_total` | Counter | Deleted messages found. |
| `undelete_outbox_backlog` | Gauge | Pending alerts (`pending` + `processing`). |

---

## 🗃️ Database (PostgreSQL 16)

### Tables and RLS

| Table | Role | RLS | Constraints |
|-------|------|-----|-------------|
| `users` | Users (tenants) | ❌ No | `telegram_user_id UNIQUE`, `retention_days` (1-365). |
| `business_connections` | Business connections | ❌ No | `id` (Text, primary key), `owner_user_id` (FK → `users`). |
| `messages` | Saved messages | ✅ **FORCE RLS** | Unique key: `(owner_user_id, business_connection_id, chat_id, message_id)`. |
| `chats` | Chat labels | ✅ **FORCE RLS** | Primary key: `(owner_user_id, business_connection_id, chat_id)`. |
| `notification_outbox` | Alert queue | ✅ **FORCE RLS** | Statuses: `pending`, `processing`, `sent`, `failed`. |
| `schema_migrations` | Migration history | ❌ No | `version` (primary key). |

### RLS Policy

All **RLS** tables use the same policy:

```sql
CREATE POLICY tenant_isolation ON <table>
    USING (owner_user_id = NULLIF(current_setting('app.current_owner_user_id', true), '')::bigint)
    WITH CHECK (owner_user_id = NULLIF(current_setting('app.current_owner_user_id', true), '')::bigint);
```

- **`current_setting('app.current_owner_user_id', true)`**:
  - `true` = "missing_ok" → returns `NULL` if the variable is not set (instead of raising an error).
  - **`NULLIF(..., '')`**: Handles the case where the variable is empty.
- **Effect**:
  - If `app.current_owner_user_id` is **not set** → **no row is visible** (fail-closed).
  - If set → **only the tenant's rows are accessible**.

---

## 🔄 Data Flow

### 1. Receiving a Message (`business_message`)

```
Telegram API (getUpdates)
    ↓
Poller (long polling)
    ↓
Handler.HandleUpdate (app/handler.go)
    ↓
business.Service.Resolve (cache → DB → API)
    ↓
Handler.saveMessage (app/handler.go)
    ↓
messages.Repository.Save (InTenant)
    ↓
Upsert into messages + chats (same transaction)
    ↓
Log: "message saved" (IDs only)
```

### 2. Detecting a Deletion (`deleted_business_messages`)

```
Telegram API (getUpdates)
    ↓
Poller
    ↓
Handler.HandleUpdate
    ↓
business.Service.Resolve
    ↓
Handler.handleDeleted (app/handler.go)
    ↓
messages.Repository.MarkDeleted (InTenant)
    ↓
1. Marks deleted_at in messages
2. Writes the alert chunks into notification_outbox (same transaction)
    ↓
Log: "deletion handled" (IDs + counters)
```

### 3. Sending an Alert (Outbox Worker)

```
Outbox Worker (loop every 1s)
    ↓
outbox.Repository.Claim (InTenant)
    ↓
Claims a job (status = processing, lease_token)
    ↓
telegram.Client.SendMessageOnce (without business_connection_id)
    ↓
On success:
    outbox.Repository.MarkSent (status = sent)
Otherwise:
    If 429 → MarkRetry (next_attempt_at = now() + retry_after)
    If non-replayable 4xx → MarkFailed (status = failed)
    If 5xx/timeout → MarkRetry (exponential backoff)
```

### 4. Retention Purge (Every 24h)

```
Retention Loop
    ↓
users.Repository.ListTenantsForRetention
    ↓
For each tenant:
    messages.Repository.PurgeExpired (InTenant)
        ↓
        DELETE FROM messages WHERE saved_at < now() - retention_days
    outbox.Repository.PurgeExpired (InTenant)
        ↓
        DELETE FROM notification_outbox WHERE status IN ('sent', 'failed') AND created_at < now() - retention_days
```

---

## 🔒 Security and Non-Negotiable Constraints

| Constraint | Description | Implementation |
|------------|-------------|----------------|
| **1** | Explicit `allowed_updates` | `telegram.AllowedUpdates` in `getUpdatesRequest`. |
| **2** | **2 distinct DSNs** (`DATABASE_URL` ≠ `MIGRATION_DATABASE_URL`) | Checked in `config.Load()`. |
| **3** | **FORCE RLS** on `messages` | `ALTER TABLE messages FORCE ROW LEVEL SECURITY`. |
| **4** | **`InTenant`** is the only path to `messages`/`notification_outbox` | `storage.DB.InTenant` sets `app.current_owner_user_id`. |
| **5** | **Sequential processing** of updates | `Poller.Run` processes one update at a time. |
| **6** | `message_ids` is an **array** | `BusinessMessagesDeleted.MessageIDs []int64`. |
| **7** | **No `business_connection_id`** in alerts | `SendMessageRequest` does not have this field. |
| **8** | **Exhaustive saving** | No condition on `chat_id` in `Save`/`MarkDeleted`. |

---

## 📊 Metrics and Monitoring

### HTTP Probes

| Endpoint | Response | Conditions |
|----------|---------|------------|
| `GET /livez` | `200 {"status":"ok"}` | Always OK (liveness). |
| `GET /readyz` | `200 {"status":"ok","checks":{...}}` | Database reachable **AND** last `getUpdates` < 90s. |
| `GET /readyz` | `503 {"status":"degraded","checks":{...}}` | Database or poller degraded. |
| `GET /metrics` | Prometheus text | Aggregated counters (no labels). |

### Prometheus Metrics

```promql
# Example queries
undelete_updates_total          # Received updates
undelete_deletions_total        # Deleted messages found
undelete_outbox_backlog         # Pending alerts
undelete_outbox_failed_total    # Abandoned alerts
```

---

## 🧪 Testing

### Unit Tests

- **Execution**: `cd bot && go test ./...`
- **Coverage**:
  - Business logic (`app`, `business`, `messages`, `outbox`).
  - Telegram update parsing (`telegram`).
  - Configuration (`config`).
  - Health checks (`health`).

### Integration Tests

- **Execution**: `make test-integration`
- **Environment**:
  - **Throwaway** PostgreSQL 16 container (no Docker volume).
  - Verifies:
    - **Migrations** (idempotence, re-run).
    - The **`undelete_app` role** (restrictions `NOSUPERUSER NOCREATEDB NOCREATEROLE NOBYPASSRLS`).
    - **Fail-closed RLS** (access without context → 0 rows).
    - Multi-tenant isolation (2 tenants do not see each other's data).
    - Outbox (lease, retry, backoff).
    - Retention purge.

---

## 📦 Deployment

### Prerequisites

1. **Create the bot** via [@BotFather](https://t.me/BotFather):
   - `/newbot` → Retrieve `TELEGRAM_BOT_TOKEN`.
   - Enable **Business Mode** (`/mybots` → *Business Mode* → *Turn on*).
2. **Configure PostgreSQL**:
   - The `db/init/01-app-role.sh` script creates the `undelete_app` role at container startup.
3. **Environment variables**:
   - Copy `.env.example` → `.env` and fill in:
     ```env
     TELEGRAM_BOT_TOKEN=...
     DATABASE_URL=postgres://undelete_app:...@postgres:5432/undelete?sslmode=disable
     MIGRATION_DATABASE_URL=postgres://postgres:...@postgres:5432/undelete?sslmode=disable
     OWNER_TELEGRAM_USER_ID=...  # Optional (mono-tenant)
     HEALTH_ADDR=:9090
     BACKUP_RETENTION_DAYS=7
     ```

### Launch

```bash
docker compose up --build -d  # Starts the bot + PostgreSQL
docker compose logs -f bot   # View the logs
```

### Verification

1. **Connect the bot** from Telegram:
   - *Settings* → *Telegram Business* → *Chatbots* → Select the bot.
2. **Check the probes**:
   ```bash
   curl http://localhost:9090/livez
   curl http://localhost:9090/readyz
   curl http://localhost:9090/metrics
   ```

---

## 📜 Roadmap

| Phase | Description |
|-------|-------------|
| **Phase 1** (current) | Mono-tenant, plaintext text, RLS in place. |
| **Phase 2** | Media (`media_files` table + local storage), GDPR commands (`/delete_my_data`, `/privacy`). |
| **Phase 3** | Real multi-tenancy (removal of the `OWNER_TELEGRAM_USER_ID` guard). |
| **Phase 4** | Content encryption (`text_encrypted BYTEA`, AES-256-GCM, per-tenant key). |

---

## 🔍 Why "99% test coverage"?

The **Undelete** project is **small in application code** (≈ 30 `.go` files excluding tests) but **very dense in tests** (15 `_test.go` files) for several reasons:

1. **Criticality of the constraints**:
   - An error in `allowed_updates` → **the bot receives nothing** (silent).
   - A `DATABASE_URL == MIGRATION_DATABASE_URL` → **RLS is decorative** (security compromised).
   - A missing `FORCE RLS` → **tenants see everything** (privacy violation).

2. **Complexity of the guarantees**:
   - **At-least-once** for alerts (outbox).
   - **Guaranteed order** within a message (chunks).
   - **Fail-closed RLS** (no data leak).

3. **PostgreSQL integration**:
   - The integration tests verify:
     - **Migrations** (idempotence, re-run).
     - The **`undelete_app` role** (no `BYPASSRLS`).
     - **RLS** (access without context → 0 rows).
     - **Multi-tenant isolation** (2 tenants do not see each other's data).

4. **Telegram contracts**:
   - The contract tests (`telegram/contracts_test.go`) verify that:
     - **Alerts** are formatted as in production.
     - **Chunks** respect Telegram's UTF-16 limit.

---

## 📌 Conclusion

**Undelete** is a **minimalist but ultra-secure** project:
- **Little application code** (≈ 2,000 lines of Go excluding tests) because:
  - **No frontend** (pure Telegram bot).
  - **No external dependencies** (Telegram client in native `net/http`).
  - **Simple business logic**: save everything, notify deletions.
- **Lots of tests** because:
  - **High criticality** (security, privacy, reliability).
  - **Non-negotiable constraints** (RLS, sequentiality, at-least-once).
  - **PostgreSQL integration** (migrations, RLS, outbox).

---

**In summary**:
✅ **Functional**: Saving + deletion detection + notifications.
✅ **Secure**: RLS `FORCE`, 2 DSNs, no data leak in logs/metrics.
✅ **Reliable**: Durable outbox, retries, backoff, at-least-once.
✅ **Tested**: 100% of constraints verified in integration.
