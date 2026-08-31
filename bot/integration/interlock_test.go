// Ce fichier n'est PAS sous le tag `integration` : les garde-fous qu'il
// contient sont de la logique pure (pas de Docker, pas de PostgreSQL) et
// doivent être exercés par un simple `go test ./...`. Ce sont eux qui
// empêchent la suite destructive de s'exécuter sur une base de production ;
// les laisser sous un tag revenait à ne les vérifier que dans le cas où on
// accepte déjà de tout effacer.
//
// Les helpers eux-mêmes vivent ici et sont utilisés par postgres_test.go, qui
// reste sous le tag `integration` puisqu'il lui faut une vraie base.

package integration_test

import (
	"fmt"
	"net/url"
	"testing"
)

func validateExplicitDestructiveOptIn(optIn string) error {
	const required = "I_UNDERSTAND_THIS_WILL_DELETE_DATA"
	if optIn != required {
		return fmt.Errorf("refusing destructive integration test: POSTGRES_INTEGRATION_ALLOW_DESTRUCTIVE must equal %q", required)
	}
	return nil
}

func validateDestructiveInterlock(optIn, databaseName string) error {
	if err := validateExplicitDestructiveOptIn(optIn); err != nil {
		return err
	}
	const requiredDatabase = "undelete_integration"
	if databaseName != requiredDatabase {
		return fmt.Errorf("refusing destructive integration test: server reports database %q, require exact dedicated name %q", databaseName, requiredDatabase)
	}
	return nil
}

// replaceDSNUser rejoue un DSN avec d'autres identifiants. Le parsing passe
// par net/url : un découpage textuel sur "@" se trompe de séparateur dès
// qu'un mot de passe en contient un (cas légal et fréquent avec des mots de
// passe générés), et produit alors un DSN vers un hôte inexistant — le test
// appelant conclurait à un rejet du rôle alors qu'il n'a jamais atteint le
// serveur.
func replaceDSNUser(dsn, user, password string) (string, error) {
	parsed, err := url.Parse(dsn)
	if err != nil {
		return "", fmt.Errorf("parse DSN: %w", err)
	}
	parsed.User = url.UserPassword(user, password)
	return parsed.String(), nil
}

func TestDestructiveInterlockRefusesUnsafeConfiguration(t *testing.T) {
	tests := []struct {
		name         string
		optIn        string
		databaseName string
	}{
		{name: "missing explicit opt-in", databaseName: "undelete_integration"},
		{name: "wrong explicit opt-in", optIn: "true", databaseName: "undelete_integration"},
		{name: "production database name", optIn: "I_UNDERSTAND_THIS_WILL_DELETE_DATA", databaseName: "undelete"},
		{name: "near-match database name", optIn: "I_UNDERSTAND_THIS_WILL_DELETE_DATA", databaseName: "undelete_integration_copy"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := validateDestructiveInterlock(tt.optIn, tt.databaseName); err == nil {
				t.Fatal("unsafe integration configuration unexpectedly accepted")
			}
		})
	}
}

func TestDestructiveInterlockAcceptsExactConfiguration(t *testing.T) {
	if err := validateDestructiveInterlock("I_UNDERSTAND_THIS_WILL_DELETE_DATA", "undelete_integration"); err != nil {
		t.Fatalf("exact integration configuration rejected: %v", err)
	}
}

func TestReplaceDSNUserRewritesCredentialsAndKeepsTarget(t *testing.T) {
	tests := []struct {
		name     string
		dsn      string
		user     string
		password string
	}{
		{
			name:     "simple credentials",
			dsn:      "postgres://undelete_app:runtime@127.0.0.1:5432/undelete_integration?sslmode=disable",
			user:     "integration_wrong_role",
			password: "integration-only",
		},
		{
			// Le cas qui cassait le découpage textuel : en scindant sur TOUS
			// les "@", l'ancienne version recollait la fin du mot de passe
			// d'origine ("ss") au nouveau mot de passe. Le DSN visait le bon
			// hôte mais s'authentifiait avec "integration-only@ss" — le pool
			// échouait sur un refus d'authentification, jamais sur le contrôle
			// de rôle que le test prétend vérifier.
			name:     "literal at sign in the original password",
			dsn:      "postgres://undelete_app:p@ss@127.0.0.1:5432/undelete_integration?sslmode=disable",
			user:     "integration_bypass_role",
			password: "integration-only",
		},
		{
			name:     "percent-encoded at sign in the original password",
			dsn:      "postgres://undelete_app:p%40ss@127.0.0.1:5432/undelete_integration?sslmode=disable",
			user:     "integration_bypass_role",
			password: "integration-only",
		},
		{
			// Symétrique : le nouveau mot de passe doit être ré-encodé pour
			// que le DSN produit reste relisible tel quel par pgx.
			name:     "at sign in the replacement password",
			dsn:      "postgres://undelete_app:runtime@db.internal:5432/undelete_integration",
			user:     "integration_wrong_role",
			password: "p@ss:word",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := replaceDSNUser(tt.dsn, tt.user, tt.password)
			if err != nil {
				t.Fatalf("replaceDSNUser: %v", err)
			}
			// On vérifie la sémantique du DSN produit, pas sa forme : hôte,
			// base et options inchangés, identifiants exactement ceux demandés.
			parsed, err := url.Parse(got)
			if err != nil {
				t.Fatalf("parse rewritten DSN %q: %v", got, err)
			}
			original, err := url.Parse(tt.dsn)
			if err != nil {
				t.Fatalf("parse original DSN: %v", err)
			}
			if parsed.Host != original.Host || parsed.Path != original.Path || parsed.RawQuery != original.RawQuery {
				t.Fatalf("rewritten DSN targets %s%s?%s, want %s%s?%s",
					parsed.Host, parsed.Path, parsed.RawQuery, original.Host, original.Path, original.RawQuery)
			}
			if parsed.User.Username() != tt.user {
				t.Fatalf("rewritten user = %q, want %q", parsed.User.Username(), tt.user)
			}
			if gotPassword, _ := parsed.User.Password(); gotPassword != tt.password {
				t.Fatalf("rewritten password = %q, want %q", gotPassword, tt.password)
			}
		})
	}
}

func TestReplaceDSNUserRejectsUnparseableDSN(t *testing.T) {
	if _, err := replaceDSNUser("postgres://%zz@host/db", "user", "password"); err == nil {
		t.Fatal("unparseable DSN unexpectedly accepted")
	}
}
