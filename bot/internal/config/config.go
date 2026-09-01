// Package config charge et valide la configuration du bot depuis les
// variables d'environnement.
package config

import (
	"fmt"
	"net"
	"os"
	"strconv"
)

// Config regroupe la configuration runtime du bot.
type Config struct {
	// DatabaseURL est le DSN applicatif, connecté avec le rôle undelete_app
	// (NOSUPERUSER NOCREATEDB NOCREATEROLE NOBYPASSRLS). C'est le SEUL DSN
	// utilisé après le boot, une fois les migrations appliquées.
	DatabaseURL string

	// MigrationDatabaseURL est le DSN propriétaire (POSTGRES_USER,
	// superuser dans l'image Postgres officielle). Utilisé UNIQUEMENT au
	// boot, pour appliquer les migrations, jamais pour du trafic runtime.
	MigrationDatabaseURL string

	// TelegramBotToken est le jeton du bot, tel que fourni par BotFather.
	TelegramBotToken string

	// OwnerTelegramUserID, si non nul, restreint le bot à un unique
	// titulaire Telegram Business (garde-fou mono-tenant Phase 1). Une
	// connexion Business provenant d'un autre telegram_user_id est
	// refusée. 0 = pas de restriction (déconseillé en dehors du dev local).
	OwnerTelegramUserID int64

	// HealthAddr est l'adresse d'écoute des probes /livez, /readyz et
	// /metrics. Vaut defaultHealthAddr si HEALTH_ADDR n'est pas définie ;
	// une valeur explicitement VIDE désactive le serveur (aucun port
	// ouvert). Ces endpoints n'exposent aucun contenu utilisateur, mais ils
	// restent destinés au réseau interne : ne pas les publier tels quels.
	HealthAddr string
}

// defaultHealthAddr : port dédié à la supervision, distinct de tout trafic
// applicatif (le bot n'écoute rien d'autre, il est en long polling sortant).
const defaultHealthAddr = ":9090"

// Load lit la configuration depuis l'environnement et la valide.
//
// Refuse de démarrer si DatabaseURL == MigrationDatabaseURL : si les deux
// DSN pointent vers le même rôle, l'application tournerait avec les
// privilèges superuser du rôle de migration, et FORCE ROW LEVEL SECURITY
// sur messages deviendrait purement décoratif (un superuser contourne RLS
// de fait via BYPASSRLS implicite / propriété de table). C'est la
// contrainte de sécurité la plus silencieuse du projet : rien ne plante en
// apparence, la table est juste totalement ouverte.
func Load() (*Config, error) {
	cfg := &Config{
		DatabaseURL:          os.Getenv("DATABASE_URL"),
		MigrationDatabaseURL: os.Getenv("MIGRATION_DATABASE_URL"),
		TelegramBotToken:     os.Getenv("TELEGRAM_BOT_TOKEN"),
		HealthAddr:           defaultHealthAddr,
	}

	// LookupEnv et non Getenv : "variable absente" (on veut le défaut) et
	// "variable posée à vide" (on veut désactiver le serveur) sont deux
	// intentions différentes.
	if raw, ok := os.LookupEnv("HEALTH_ADDR"); ok {
		cfg.HealthAddr = raw
	}

	// Validée ici, et pas seulement au net.Listen : une valeur mal formée
	// (« 9090 » sans deux-points) laisserait le bot démarrer normalement puis
	// perdre TOUTES ses probes et ses métriques sur un unique log Error, sans
	// que rien d'autre ne bouge. Une supervision muette est exactement ce que
	// l'issue #6 cherche à éliminer : on échoue au démarrage, franchement.
	if cfg.HealthAddr != "" {
		if _, _, err := net.SplitHostPort(cfg.HealthAddr); err != nil {
			return nil, fmt.Errorf("HEALTH_ADDR invalide (attendu « hôte:port », par exemple %q ; vide pour désactiver le serveur de santé): %w", defaultHealthAddr, err)
		}
	}

	if cfg.DatabaseURL == "" {
		return nil, fmt.Errorf("DATABASE_URL est requis")
	}
	if cfg.MigrationDatabaseURL == "" {
		return nil, fmt.Errorf("MIGRATION_DATABASE_URL est requis")
	}
	if cfg.TelegramBotToken == "" {
		return nil, fmt.Errorf("TELEGRAM_BOT_TOKEN est requis")
	}

	if cfg.DatabaseURL == cfg.MigrationDatabaseURL {
		return nil, fmt.Errorf("DATABASE_URL et MIGRATION_DATABASE_URL sont identiques : " +
			"l'application tournerait avec le rôle propriétaire (superuser) et FORCE ROW LEVEL SECURITY " +
			"sur messages serait décoratif ; utilisez le rôle restreint undelete_app pour DATABASE_URL")
	}

	if raw := os.Getenv("OWNER_TELEGRAM_USER_ID"); raw != "" {
		id, err := strconv.ParseInt(raw, 10, 64)
		if err != nil {
			return nil, fmt.Errorf("OWNER_TELEGRAM_USER_ID invalide: %w", err)
		}
		cfg.OwnerTelegramUserID = id
	}

	return cfg, nil
}
