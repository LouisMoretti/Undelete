# Undelete — Agent Instructions

## Commands
| Task | Command |
|------|---------|
| Build + lint + vet + fmt | `make check` |
| Run integration tests (Docker) | `make test-integration` |
| Verify a backup is restorable (Docker) | `make test-restore` |
| Verify dump + media restore together (Docker) | `make test-restore-media` |
| Start dev stack | `docker compose up --build -d` |
| View bot logs | `docker compose logs -f bot` |
| Stop stack | `docker compose down` (never `-v` — deletes the DB volume) |
| Preflight before deploy | `sh scripts/preflight.sh` |

## Project Structure
- `bot/` — Go 1.23 module (`github.com/LouisMoretti/Undelete/bot`)
- `bot/cmd/bot/main.go` — entrypoint
- `bot/internal/` — packages: `app`, `business`, `config`, `messages`, `outbox`, `storage`, `telegram`, `users`
- `db/init/01-app-role.sh` — creates restricted `undelete_app` role (runs on Postgres init)
- `scripts/test-integration.sh` — spins up throwaway Postgres 16 container for tests

## Critical Constraints (do not violate)
1. **Two separate DSNs required**: `MIGRATION_DATABASE_URL` (owner) ≠ `DATABASE_URL` (app role). `config.Load()` fails if equal.
2. **Explicit `allowed_updates`**: `business_connection`, `business_message`, `edited_business_message`, `deleted_business_messages`. Without these, Telegram sends nothing.
3. **Sequential update processing**: `Poller` handles updates one at a time. Parallel processing would race deletions before message persistence.
4. **`InTenant` is the ONLY path to `messages`/`notification_outbox`**: sets `app.current_owner_user_id` LOCAL per transaction. `PurgeExpired` loops tenant-by-tenant.
5. **FORCE ROW LEVEL SECURITY** on `messages`. `ENABLE` alone doesn't apply to table owner.
6. **Alerts sent WITHOUT `business_connection_id`**: that field would send as the owner into the monitored chat.

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
- `OWNER_TELEGRAM_USER_ID` — mono-tenant guard (Phase 1). Empty in dev only.
- `BACKUP_RETENTION_DAYS` — daily pg_dump retention (media archives are **not** purged automatically)
- `MEDIA_BACKUP_MODE` (`auto`) / `MEDIA_BACKUP_FULL_INTERVAL_DAYS` (`7`) — media full/incremental cadence

## Key Architecture Notes
- Migrations run at boot with owner DSN, BEFORE app pool opens
- Outbox: `deleted_at` + notification chunks written atomically; worker processes leases with exponential backoff, respects 429 `retry_after`
- Retention purge runs daily, separate from poller loop (poller must stay responsive)
- Media retention (`internal/media/purge`) extends that daily cycle to `./media`: the blob is unlinked BEFORE the row is marked `purged`, so a crash between the two leaves only the mismatch the catalogue can detect on its own. The reconciliation repairs both directions (row without file, file without row), always bounded per run and resumed by cursor. `MEDIA_PURGE_DRY_RUN=true` logs without deleting.
- Logs: `slog` JSON, never contain message content