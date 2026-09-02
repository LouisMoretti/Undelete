// This file is NOT under the `integration` tag: the guards it contains are
// pure logic (no Docker, no PostgreSQL) and must be exercised by a plain
// `go test ./...`. They are what prevents the destructive suite from running
// against a production database; keeping them behind a tag would mean only
// verifying them when you already accept wiping everything.
//
// The helpers themselves live here and are used by postgres_test.go, which
// stays under the `integration` tag since it needs a real database.

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

// replaceDSNUser replays a DSN with different credentials. Parsing goes
// through net/url: a textual split on "@" picks the wrong separator as soon
// as a password contains one (a legal and common case with generated
// passwords), and would then produce a DSN pointing at a nonexistent host —
// the calling test would conclude the role was rejected when it never
// reached the server.
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
			// The case that broke the textual split: by splitting on ALL "@"
			// characters, the old version glued the end of the original
			// password ("ss") onto the new password. The DSN targeted the
			// right host but authenticated with "integration-only@ss" — the
			// pool failed on an authentication refusal, never on the role
			// check the test claims to verify.
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
			// Symmetric: the new password must be re-encoded so the produced
			// DSN stays readable as-is by pgx.
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
			// We verify the semantics of the produced DSN, not its shape:
			// host, database and options unchanged, credentials exactly the
			// requested ones.
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
