// Package storage gère la connexion PostgreSQL, le runner de migrations et
// le helper InTenant qui pose le contexte RLS avant toute requête sur une
// table protégée.
package storage

import (
	"context"
	"embed"
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

// DB encapsule le pool de connexions applicatif (rôle undelete_app,
// contraint par RLS). Toute requête sur une table RLS doit impérativement
// passer par InTenant : le pool nu n'a pas de contexte "owner" posé.
type DB struct {
	Pool *pgxpool.Pool
}

const runtimeRole = "undelete_app"

// NewPool ouvre le pool applicatif. dsn doit être DATABASE_URL (rôle
// undelete_app), jamais MIGRATION_DATABASE_URL. La comparaison textuelle des
// DSN dans config.Load reste le garde-fou demandé, mais elle ne suffit pas :
// deux chaînes différentes peuvent désigner le même superuser. On vérifie donc
// aussi l'identité et les attributs du rôle réellement authentifié.
func NewPool(ctx context.Context, dsn string) (*DB, error) {
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		return nil, fmt.Errorf("ouverture du pool applicatif: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("ping du pool applicatif: %w", err)
	}

	var role string
	var isSuperuser, bypassRLS bool
	if err := pool.QueryRow(ctx, `
		SELECT current_user, rolsuper, rolbypassrls
		FROM pg_catalog.pg_roles
		WHERE rolname = current_user
	`).Scan(&role, &isSuperuser, &bypassRLS); err != nil {
		pool.Close()
		return nil, fmt.Errorf("vérification du rôle applicatif: %w", err)
	}
	if role != runtimeRole || isSuperuser || bypassRLS {
		pool.Close()
		return nil, fmt.Errorf("rôle runtime PostgreSQL dangereux: rôle=%q superuser=%t bypassrls=%t; attendu %q sans privilèges de contournement RLS",
			role, isSuperuser, bypassRLS, runtimeRole)
	}

	return &DB{Pool: pool}, nil
}

func (db *DB) Close() {
	db.Pool.Close()
}

// InTenant ouvre une transaction, y pose app.current_owner_user_id via
// set_config(..., true), puis exécute fn dans cette transaction.
//
// Le troisième argument de set_config à true = LOCAL : la variable ne vit
// que le temps de la transaction courante. C'est indispensable avec un
// pooler de connexions (pgxpool réutilise les connexions physiques entre
// transactions) : sans LOCAL, un réglage SESSION resterait posé sur la
// connexion et "fuiterait" vers la prochaine transaction d'un autre tenant
// qui récupérerait la même connexion physique.
//
// C'est le SEUL point d'entrée légitime pour toute requête sur la table
// messages (protégée par FORCE ROW LEVEL SECURITY). Ne jamais requêter
// messages directement via db.Pool.
func (db *DB) InTenant(ctx context.Context, ownerID int64, fn func(pgx.Tx) error) error {
	tx, err := db.Pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("ouverture transaction InTenant: %w", err)
	}
	defer tx.Rollback(ctx) // no-op si Commit a déjà eu lieu

	if _, err := tx.Exec(ctx, `SELECT set_config('app.current_owner_user_id', $1, true)`, strconv.FormatInt(ownerID, 10)); err != nil {
		return fmt.Errorf("pose du contexte RLS: %w", err)
	}

	if err := fn(tx); err != nil {
		return err
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit InTenant: %w", err)
	}
	return nil
}

// RunMigrations applique les migrations embarquées avec le DSN propriétaire
// (superuser). Doit être appelée au boot du binaire, avant l'ouverture du
// pool applicatif : le rôle undelete_app n'a pas les droits DDL
// (NOCREATEDB NOCREATEROLE et pas de droits d'écriture sur le schéma avant
// que db/init/01-app-role.sh ait posé les GRANT nécessaires).
func RunMigrations(ctx context.Context, migrationDSN string, logger *slog.Logger) error {
	conn, err := pgx.Connect(ctx, migrationDSN)
	if err != nil {
		return fmt.Errorf("connexion migration: %w", err)
	}
	defer conn.Close(ctx)

	// Verrou de session couvrant tout le runner : deux réplicas qui démarrent
	// ensemble ne doivent pas lire simultanément "migration absente" puis
	// exécuter le même DDL. La fermeture de conn libère toujours ce verrou,
	// y compris sur un retour d'erreur.
	const migrationLockID int64 = 74617309141001
	if _, err := conn.Exec(ctx, `SELECT pg_advisory_lock($1)`, migrationLockID); err != nil {
		return fmt.Errorf("verrouillage du runner de migrations: %w", err)
	}

	if _, err := conn.Exec(ctx, `
		CREATE TABLE IF NOT EXISTS schema_migrations (
			version    INT PRIMARY KEY,
			applied_at TIMESTAMPTZ NOT NULL DEFAULT now()
		)
	`); err != nil {
		return fmt.Errorf("création schema_migrations: %w", err)
	}

	entries, err := fs.ReadDir(migrationsFS, "migrations")
	if err != nil {
		return fmt.Errorf("lecture des migrations embarquées: %w", err)
	}

	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".sql") {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names) // tri par nom : le préfixe numérique fixe l'ordre d'application

	// Valider toutes les versions avant d'exécuter le moindre DDL évite qu'un
	// second fichier avec le même préfixe soit silencieusement considéré comme
	// déjà appliqué à cause de la clé primaire schema_migrations(version).
	seenVersions := make(map[int]string, len(names))
	for _, name := range names {
		version, err := parseVersion(name)
		if err != nil {
			return fmt.Errorf("nom de migration invalide %q: %w", name, err)
		}
		if version <= 0 {
			return fmt.Errorf("version de migration non positive %d dans %q", version, name)
		}
		if previous, exists := seenVersions[version]; exists {
			return fmt.Errorf("version de migration %d dupliquée dans %q et %q", version, previous, name)
		}
		seenVersions[version] = name
	}

	for _, name := range names {
		version, err := parseVersion(name)
		if err != nil {
			return fmt.Errorf("nom de migration invalide %q: %w", name, err)
		}

		var alreadyApplied bool
		if err := conn.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM schema_migrations WHERE version = $1)`, version).Scan(&alreadyApplied); err != nil {
			return fmt.Errorf("vérification migration %d: %w", version, err)
		}
		if alreadyApplied {
			continue
		}

		sqlBytes, err := migrationsFS.ReadFile(path.Join("migrations", name))
		if err != nil {
			return fmt.Errorf("lecture migration %s: %w", name, err)
		}

		// Chaque migration dans sa propre transaction : une migration qui
		// échoue à mi-chemin ne doit pas laisser le schéma dans un état
		// partiel silencieusement marqué comme appliqué.
		tx, err := conn.Begin(ctx)
		if err != nil {
			return fmt.Errorf("ouverture transaction migration %d: %w", version, err)
		}

		if _, err := tx.Exec(ctx, string(sqlBytes)); err != nil {
			tx.Rollback(ctx)
			return fmt.Errorf("application migration %d (%s): %w", version, name, err)
		}
		if _, err := tx.Exec(ctx, `INSERT INTO schema_migrations (version) VALUES ($1)`, version); err != nil {
			tx.Rollback(ctx)
			return fmt.Errorf("enregistrement migration %d: %w", version, err)
		}
		if err := tx.Commit(ctx); err != nil {
			return fmt.Errorf("commit migration %d: %w", version, err)
		}

		logger.Info("migration appliquée", slog.Int("version", version), slog.String("name", name))
	}

	return nil
}

// parseVersion extrait le préfixe numérique d'un nom de fichier de
// migration, ex: "0001_init.sql" -> 1.
func parseVersion(name string) (int, error) {
	prefix, _, found := strings.Cut(name, "_")
	if !found {
		return 0, fmt.Errorf("format attendu <version>_<nom>.sql")
	}
	version, err := strconv.Atoi(prefix)
	if err != nil {
		return 0, fmt.Errorf("préfixe numérique invalide: %w", err)
	}
	return version, nil
}
