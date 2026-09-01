package config

import (
	"os"
	"strings"
	"testing"
)

func validEnv(t *testing.T) {
	t.Helper()
	t.Setenv("DATABASE_URL", "postgres://undelete_app:app@postgres/undelete")
	t.Setenv("MIGRATION_DATABASE_URL", "postgres://owner:owner@postgres/undelete")
	t.Setenv("TELEGRAM_BOT_TOKEN", "not-a-real-bot-token")
	t.Setenv("OWNER_TELEGRAM_USER_ID", "")
	t.Setenv("HEALTH_ADDR", defaultHealthAddr)
}

func TestLoadHealthAddr(t *testing.T) {
	cases := map[string]struct {
		env   string
		unset bool
		want  string
	}{
		"valeur explicite":                 {env: "127.0.0.1:9999", want: "127.0.0.1:9999"},
		"vide désactive le serveur":        {env: "", want: ""},
		"valeur par défaut si non définie": {unset: true, want: defaultHealthAddr},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			validEnv(t)
			t.Setenv("HEALTH_ADDR", tc.env)
			if tc.unset {
				// validEnv/t.Setenv a déjà enregistré la restauration : on
				// peut retirer la variable pour tester le cas « absente »,
				// distinct du cas « posée à vide ».
				os.Unsetenv("HEALTH_ADDR")
			}

			cfg, err := Load()
			if err != nil {
				t.Fatalf("Load() erreur inattendue: %v", err)
			}
			if cfg.HealthAddr != tc.want {
				t.Fatalf("HealthAddr = %q, attendu %q", cfg.HealthAddr, tc.want)
			}
		})
	}
}

// Une adresse mal formée doit tomber au démarrage : sinon le bot tourne
// normalement, net.Listen échoue dans sa goroutine et la supervision est
// perdue en silence, sur un seul log.
func TestLoadRejectsMalformedHealthAddr(t *testing.T) {
	for _, addr := range []string{"9090", "127.0.0.1", ":"} {
		t.Run(addr, func(t *testing.T) {
			validEnv(t)
			t.Setenv("HEALTH_ADDR", addr)

			cfg, err := Load()
			if addr == ":" {
				// « :port » vide reste un port valide au sens de
				// SplitHostPort (écoute sur un port libre) : on n'invente
				// pas une règle plus stricte que net.Listen.
				if err != nil {
					t.Fatalf("HEALTH_ADDR=%q rejetée à tort: %v", addr, err)
				}
				return
			}
			if err == nil {
				t.Fatalf("HEALTH_ADDR=%q acceptée, attendu une erreur (HealthAddr=%q)", addr, cfg.HealthAddr)
			}
			if !strings.Contains(err.Error(), "HEALTH_ADDR") {
				t.Fatalf("erreur peu explicite: %v", err)
			}
		})
	}
}

func TestLoadRejectsIdenticalDatabaseURLs(t *testing.T) {
	validEnv(t)
	t.Setenv("MIGRATION_DATABASE_URL", "postgres://undelete_app:app@postgres/undelete")

	_, err := Load()
	if err == nil || !strings.Contains(err.Error(), "identiques") {
		t.Fatalf("Load() erreur = %v, attendu refus des DSN identiques", err)
	}
}

func TestLoadParsesOwnerGuard(t *testing.T) {
	validEnv(t)
	t.Setenv("OWNER_TELEGRAM_USER_ID", "123456789")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() erreur inattendue: %v", err)
	}
	if cfg.OwnerTelegramUserID != 123456789 {
		t.Fatalf("OwnerTelegramUserID = %d", cfg.OwnerTelegramUserID)
	}
}
