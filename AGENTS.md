# Undelete — Agent Instructions

## Commands
| Task | Command |
|------|---------|
| Build + lint + vet + fmt | `make check` |
| Run integration tests (Docker) | `make test-integration` |
| Start dev stack | `docker compose up --build -d` |
| View bot logs | `docker compose logs -f bot` |
| Stop stack | `docker compose down` |

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

## Testing
- Unit tests: `cd bot && go test ./...` (no special tags)
- Integration tests: `make test-integration` (requires Docker) OR provide `POSTGRES_INTEGRATION_ADMIN_DSN`, `POSTGRES_INTEGRATION_RUNTIME_DSN`, `POSTGRES_INTEGRATION_ALLOW_DESTRUCTIVE=I_UNDERSTAND_THIS_WILL_DELETE_DATA`
- Integration tests run against real Postgres 16, verify migrations, RLS, multi-tenant isolation, outbox retry/backoff

## Environment
- Copy `.env.example` → `.env`, fill in tokens/passwords
- `OWNER_TELEGRAM_USER_ID` — mono-tenant guard (Phase 1). Empty in dev only.
- `BACKUP_RETENTION_DAYS` — daily pg_dump retention

## Key Architecture Notes
- Migrations run at boot with owner DSN, BEFORE app pool opens
- Outbox: `deleted_at` + notification chunks written atomically; worker processes leases with exponential backoff, respects 429 `retry_after`
- Retention purge runs daily, separate from poller loop (poller must stay responsive)
- Logs: `slog` JSON, never contain message content