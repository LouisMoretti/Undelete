// Package storage manages the PostgreSQL connection, the migration runner,
// and the InTenant helper that sets the RLS context before any query against
// a protected table.
package storage

import (
	"context"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"path"
	"sort"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

//go:embed migrations/*.sql
var migrationsFS embed.FS

// DB wraps the application connection pool (undelete_app role, constrained
// by RLS). Any query against an RLS table must go through InTenant: the bare
// pool has no "owner" context set.
type DB struct {
	Pool *pgxpool.Pool
}

const runtimeRole = "undelete_app"

// ErrUnsafeRuntimeRole is returned (wrapped) by NewPool when the actually
// authenticated role is not the restricted application role. Exported sentinel
// so tests can identify it via errors.Is rather than by the message text,
// which is free to evolve.
var ErrUnsafeRuntimeRole = errors.New("unsafe PostgreSQL runtime role")

// NewPool opens the application pool. dsn must be DATABASE_URL (the
// undelete_app role), never MIGRATION_DATABASE_URL. The textual DSN
// comparison in config.Load remains the required safeguard, but it is not
// sufficient: two different strings can designate the same superuser. We
// therefore also verify the identity and attributes of the actually
// authenticated role.
func NewPool(ctx context.Context, dsn string) (*DB, error) {
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		return nil, fmt.Errorf("opening application pool: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("pinging application pool: %w", err)
	}

	var role string
	var isSuperuser, bypassRLS bool
	if err := pool.QueryRow(ctx, `
		SELECT current_user, rolsuper, rolbypassrls
		FROM pg_catalog.pg_roles
		WHERE rolname = current_user
	`).Scan(&role, &isSuperuser, &bypassRLS); err != nil {
		pool.Close()
		return nil, fmt.Errorf("checking application role: %w", err)
	}
	if role != runtimeRole || isSuperuser || bypassRLS {
		pool.Close()
		return nil, fmt.Errorf("%w: role=%q superuser=%t bypassrls=%t; expected %q without RLS bypass privileges",
			ErrUnsafeRuntimeRole, role, isSuperuser, bypassRLS, runtimeRole)
	}

	return &DB{Pool: pool}, nil
}

func (db *DB) Close() {
	db.Pool.Close()
}

// InTenant opens a transaction, sets app.current_owner_user_id on it via
// set_config(..., true), then runs fn within that transaction.
//
// The third set_config argument set to true = LOCAL: the variable lives only
// for the current transaction. This is essential with a connection pooler
// (pgxpool reuses physical connections between transactions): without LOCAL,
// a SESSION setting would remain on the connection and leak into the next
// transaction of another tenant that happens to grab the same physical
// connection.
//
// This is the ONLY legitimate entry point for any query against the messages
// table (protected by FORCE ROW LEVEL SECURITY). Never query messages
// directly through db.Pool.
func (db *DB) InTenant(ctx context.Context, ownerID int64, fn func(pgx.Tx) error) error {
	tx, err := db.Pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("opening InTenant transaction: %w", err)
	}
	defer tx.Rollback(ctx) // no-op if Commit already happened

	if _, err := tx.Exec(ctx, `SELECT set_config('app.current_owner_user_id', $1, true)`, strconv.FormatInt(ownerID, 10)); err != nil {
		return fmt.Errorf("setting RLS context: %w", err)
	}

	if err := fn(tx); err != nil {
		return err
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("committing InTenant transaction: %w", err)
	}
	return nil
}

// RunMigrations applies the embedded migrations with the owner DSN
// (superuser). Must be called at binary boot, before the application pool is
// opened: the undelete_app role has no DDL privileges (NOCREATEDB NOCREATEROLE
// and no schema write rights until db/init/01-app-role.sh has issued the
// necessary GRANTs).
func RunMigrations(ctx context.Context, migrationDSN string, logger *slog.Logger) error {
	conn, err := pgx.Connect(ctx, migrationDSN)
	if err != nil {
		return fmt.Errorf("connecting for migrations: %w", err)
	}
	defer conn.Close(ctx)

	// Session lock covering the whole runner: two replicas starting at the same
	// time must not simultaneously read "migration missing" and then execute
	// the same DDL. Closing conn always releases this lock, including on error
	// return.
	const migrationLockID int64 = 74617309141001
	if _, err := conn.Exec(ctx, `SELECT pg_advisory_lock($1)`, migrationLockID); err != nil {
		return fmt.Errorf("locking the migration runner: %w", err)
	}

	if _, err := conn.Exec(ctx, `
		CREATE TABLE IF NOT EXISTS schema_migrations (
			version    INT PRIMARY KEY,
			applied_at TIMESTAMPTZ NOT NULL DEFAULT now()
		)
	`); err != nil {
		return fmt.Errorf("creating schema_migrations: %w", err)
	}

	entries, err := fs.ReadDir(migrationsFS, "migrations")
	if err != nil {
		return fmt.Errorf("reading embedded migrations: %w", err)
	}

	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".sql") {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names) // sort by name: the numeric prefix fixes the application order

	// Validating all versions before executing any DDL prevents a second file
	// with the same prefix from being silently considered already applied
	// because of the schema_migrations(version) primary key.
	seenVersions := make(map[int]string, len(names))
	for _, name := range names {
		version, err := parseVersion(name)
		if err != nil {
			return fmt.Errorf("invalid migration name %q: %w", name, err)
		}
		if version <= 0 {
			return fmt.Errorf("non-positive migration version %d in %q", version, name)
		}
		if previous, exists := seenVersions[version]; exists {
			return fmt.Errorf("duplicate migration version %d in %q and %q", version, previous, name)
		}
		seenVersions[version] = name
	}

	for _, name := range names {
		version, err := parseVersion(name)
		if err != nil {
			return fmt.Errorf("invalid migration name %q: %w", name, err)
		}

		var alreadyApplied bool
		if err := conn.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM schema_migrations WHERE version = $1)`, version).Scan(&alreadyApplied); err != nil {
			return fmt.Errorf("checking migration %d: %w", version, err)
		}
		if alreadyApplied {
			continue
		}

		sqlBytes, err := migrationsFS.ReadFile(path.Join("migrations", name))
		if err != nil {
			return fmt.Errorf("reading migration %s: %w", name, err)
		}

		// Each migration in its own transaction: a migration that fails partway
		// must not leave the schema in a partial state silently marked as
		// applied.
		tx, err := conn.Begin(ctx)
		if err != nil {
			return fmt.Errorf("opening migration %d transaction: %w", version, err)
		}

		if _, err := tx.Exec(ctx, string(sqlBytes)); err != nil {
			tx.Rollback(ctx)
			return fmt.Errorf("applying migration %d (%s): %w", version, name, err)
		}
		if _, err := tx.Exec(ctx, `INSERT INTO schema_migrations (version) VALUES ($1)`, version); err != nil {
			tx.Rollback(ctx)
			return fmt.Errorf("recording migration %d: %w", version, err)
		}
		if err := tx.Commit(ctx); err != nil {
			return fmt.Errorf("committing migration %d: %w", version, err)
		}

		logger.Info("migration applied", slog.Int("version", version), slog.String("name", name))
	}

	return nil
}

// parseVersion extracts the numeric prefix of a migration filename,
// e.g. "0001_init.sql" -> 1.
func parseVersion(name string) (int, error) {
	prefix, _, found := strings.Cut(name, "_")
	if !found {
		return 0, fmt.Errorf("expected format <version>_<name>.sql")
	}
	version, err := strconv.Atoi(prefix)
	if err != nil {
		return 0, fmt.Errorf("invalid numeric prefix: %w", err)
	}
	return version, nil
}
