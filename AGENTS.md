# Undelete — Agent Instructions

## Commands
| Task | Command |
|------|---------|
| Build + lint + vet + fmt (+ tidy check) | `make check` |
| Unit tests | `make test` |
| Unit + integration coverage (floor 75%) | `make test-coverage` |
| Run integration tests (Docker) | `make test-integration` |
| Verify a backup is restorable (Docker) | `make test-restore` |
| Verify dump + media restore together (Docker) | `make test-restore-media` |
| Start dev stack | `docker compose up --build -d` |
| View bot logs | `docker compose logs -f bot` |
| Stop stack | `docker compose down` (never `-v` — deletes the DB volume) |
| Preflight before deploy | `sh scripts/preflight.sh` |

## Project Structure
- `bot/` — Go 1.25 module (`github.com/LouisMoretti/Undelete/bot`)
- `bot/cmd/bot/main.go` — entrypoint (background loops depend on narrow interfaces, unit-tested in `main_loop_test.go`)
- `bot/internal/` — packages: `app`, `business`, `config`, `erasure`, `health`, `media` (+`fetch`, `purge`, `store`), `messages`, `metrics`, `outbox`, `privacy`, `storage`, `telegram` (+`telegramtest` helpers), `tenantexcl`, `users`
- `db/init/01-app-role.sh` — creates restricted `undelete_app` role (runs on Postgres init)
- `scripts/test-integration.sh` — spins up throwaway Postgres 16 container for tests

## Critical Constraints (do not violate)
1. **Two separate DSNs required**: `MIGRATION_DATABASE_URL` (owner) ≠ `DATABASE_URL` (app role). `config.Load()` fails if equal.
2. **Explicit `allowed_updates`**: `business_connection`, `business_message`, `edited_business_message`, `deleted_business_messages`. Without these, Telegram sends nothing.
3. **Sequential update processing**: `Poller` handles updates one at a time. Parallel processing would race deletions before message persistence.
4. **`InTenant` is the ONLY path to `messages`/`notification_outbox`**: sets `app.current_owner_user_id` LOCAL per transaction. `PurgeExpired` loops tenant-by-tenant. Enforced by `bot/internal/storage/tenantsurface_test.go`: only `storage`/`users` may hold a pool, only the four repositories may name an RLS table, only `storage`/`cmd/bot` may reach `DB.Pool` — each allowlist exact.
5. **FORCE ROW LEVEL SECURITY** on `messages`. `ENABLE` alone doesn't apply to table owner.
6. **Alerts sent WITHOUT `business_connection_id`**: that field would send as the owner into the monitored chat.
7. **A Business connection never changes owner** (constraint 9 in the README): the upsert in `business/service.go` guards its `ON CONFLICT` clause on `owner_user_id`, so an update claiming an existing connection for another holder writes nothing (`ErrConnectionOwnerConflict`).

## Operations
- `docs/runbook.md` is the reference procedure: preflight → backup → migration → rollout → verification, plus rollback, secret rotation and staging recipe. Follow its order.
- **Destructive actions are a closed list** (`docker compose down -v`, `docker volume rm/prune`, `docker system prune`, `DROP DATABASE`/`TRUNCATE`, restoring a dump over prod, deleting `./backups`). None is ever required to deploy or roll back. **Never run or suggest one without Louis's explicit confirmation** — see the boxed section at the top of `docs/runbook.md`.
- Rolling back the DB is the last resort: migrations are additive, so an older bot runs fine on a newer schema. Restoring loses every message captured since the dump.

## Testing
- Unit tests: `cd bot && go test ./...` (no special tags)
- Integration tests: `make test-integration` (requires Docker) OR provide `POSTGRES_INTEGRATION_ADMIN_DSN`, `POSTGRES_INTEGRATION_RUNTIME_DSN`, `POSTGRES_INTEGRATION_ALLOW_DESTRUCTIVE=I_UNDERSTAND_THIS_WILL_DELETE_DATA`
- Integration tests run against real Postgres 16, verify migrations, RLS, multi-tenant isolation, outbox retry/backoff
- Media restore test: `make test-restore-media` (requires Docker) — runs the real `scripts/backup.sh` **and** `scripts/backup-media.sh` (full then incremental), restores the pair into a throwaway container plus an empty directory, verifies the `.meta` coupling, the `.sha256`/MANIFEST integrity, and asserts a missing object and an altered file are both detected. Same guardrails as `test-restore`.
- Restore test: `make test-restore` (requires Docker) — dumps a throwaway source DB with `scripts/backup.sh`, restores into a **separate blank** target container, checks gzip integrity, tables, `schema_migrations`, row counts, canary rows, `FORCE RLS`, and reports the measured RTO. Refuses to run if `MIGRATION_DATABASE_URL`/`DATABASE_URL` are set. Never touches existing volumes. See `docs/backup-restore.md` (RPO/RTO, weekly recipe).

## Environment
- Copy `.env.example` → `.env`, fill in tokens/passwords
- `OWNER_ALLOWLIST_TELEGRAM_USER_IDS` — onboarding allowlist (comma/space separated Telegram user ids). **Empty = open onboarding**: any Business account holder can connect and becomes a tenant. `OWNER_TELEGRAM_USER_ID` was removed in Phase 3 and `config.Load()` now **fails** if it still holds a value.
- `BACKUP_RETENTION_DAYS` — daily pg_dump retention (media archives are **not** purged automatically)
- `BACKUP_PING_URL` — optional dead man's switch pinged after every fully successful backup pass (dump AND media)
- `MEDIA_BACKUP_MODE` (`auto`) / `MEDIA_BACKUP_FULL_INTERVAL_DAYS` (`7`) — media full/incremental cadence

## Key Architecture Notes
- Migrations run at boot with owner DSN, BEFORE app pool opens
- Outbox: `deleted_at` + notification chunks written atomically; worker processes leases with exponential backoff, honours 429 `retry_after` exactly (stored, never slept). Fast lane: 10 attempts (≈8.5 min of backoff cumulated); then `failed` + 6h slow-lane resweep with a fresh budget -- an alert is deferred, never abandoned. `failed` rows do not block later chunks. The backlog gauge counts `failed` too (undelivered work, even while parked in the slow lane).
- Retention purge runs daily, separate from poller loop (poller must stay responsive). The media retention takes the shared side of the tenant exclusion per tenant; `EraseTenant` takes none (erasure holds the exclusive side while calling it -- re-acquiring would self-deadlock).
- Media retention (`internal/media/purge`) extends that daily cycle to `./media`: the blob is unlinked BEFORE the row is marked `purged`, so a crash between the two leaves only the mismatch the catalogue can detect on its own. The reconciliation repairs both directions (row without file, file without row), always bounded per run and resumed by cursor. `MEDIA_PURGE_DRY_RUN=true` logs without deleting.
- Multi-tenancy: `business.Service` resolves a connection through cache → database → Telegram API. The cache is **bounded (4096 entries, LRU) and expiring (1 min)**: open onboarding makes the id space stranger-driven, and a connection disabled out of band must stop being served as enabled. `DisableOwner` patches it synchronously (an erasure cannot wait a minute). A connection Telegram answers `400` for is memoised as revoked
- Commands (`/privacy`): read from `business_message` only — `allowed_updates` never delivers a plain `message`. That the holder's own outgoing messages arrive as `business_message` is read from the Bot API contract and has not been exercised against a real Business account in this repository — verify it manually on a real account before relying on it. Answered ONLY to the sender when they are the owner of the connection the command arrived through, as a direct message without `business_connection_id`. The answer is cut on paragraph boundaries (never mid-word) and every message is labelled `Privacy policy (i/n)`, label included in the 4096-unit budget, so a delivery that stops short reads as incomplete; the whole send is bounded by `commandAnswerTimeout` because it runs on the poller's goroutine. The policy has one source of truth, `internal/privacy/policy.md`, embedded with `go:embed`; its version and effective date are parsed back from it, never duplicated in Go
- Logs: `slog` JSON, never contain message content