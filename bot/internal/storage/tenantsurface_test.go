package storage

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// This file is the STATIC half of the tenant-isolation audit required by issue
// #17. The dynamic half lives in bot/integration (schema flags, policies and
// three-tenant cross-reads against a real PostgreSQL); what can only be checked
// here is the shape of the code that reaches the database at all.
//
// The three rules below are each a way the "InTenant is the only path to tenant
// data" invariant could be lost by accident in a multi-tenant codebase, and
// none of them is visible in a behavioural test: a new repository that opens
// its own pool, a new package that starts querying a tenant table, or an
// existing one that reaches past InTenant through the exported DB.Pool. Each
// allowlist below is deliberately exact -- a stale entry fails the test too, so
// a package that stops touching the database cannot keep a permission it no
// longer needs.

// rlsTables are the tables under FORCE ROW LEVEL SECURITY. Every read and write
// of one of them must happen inside a storage.DB.InTenant transaction, which
// is the only place app.current_owner_user_id is ever set.
var rlsTables = []string{
	"messages",
	"notification_outbox",
	"chats",
	"media_files",
	"data_erasure_requests",
}

// poolHolders are the packages allowed to hold a *pgxpool.Pool directly.
//
//   - storage owns the pool and the InTenant helper built on it;
//   - users queries the users table, which is the ROOT of the tenant
//     relationship: you cannot set app.current_owner_user_id before knowing
//     which owner_user_id a telegram_user_id maps to, so that table carries no
//     RLS policy and is reached without a tenant context by construction.
//
// business also queries outside RLS (business_connections, the resolution
// table, for the same circularity reason) but does so through a narrow
// two-method interface rather than a pool, so it does not appear here.
var poolHolders = []string{
	"internal/storage",
	"internal/users",
}

// rlsTableUsers are the packages allowed to name an RLS-protected table in
// their SQL. Each one takes a *storage.DB and goes through InTenant.
//
// storage does not appear: the statements that create those tables live in
// .sql migration files, not in Go.
var rlsTableUsers = []string{
	"internal/erasure",
	"internal/media",
	"internal/messages",
	"internal/outbox",
}

// poolReachers are the packages allowed to name the exported DB.Pool field.
// Reaching it from a repository would be a query on the bare pool, i.e. a
// query with no tenant context, which RLS answers with zero rows on a read
// and a policy violation on a write -- fail-closed, but silently wrong.
//
//   - storage exposes the field and closes it;
//   - cmd/bot wires it into the two packages of poolHolders (plus the health
//     probe, which only ever pings it).
var poolReachers = []string{
	"cmd/bot",
	"internal/storage",
}

// TestOnlyStorageAndUsersHoldADatabasePool walks the module and enforces the
// three rules above on every non-test Go file.
func TestOnlyStorageAndUsersHoldADatabasePool(t *testing.T) {
	files := moduleGoFiles(t)

	pools := map[string]bool{}
	tables := map[string]bool{}
	reachers := map[string]bool{}

	for _, file := range files {
		source, err := os.ReadFile(file.path)
		if err != nil {
			t.Fatalf("read %s: %v", file.path, err)
		}
		parsed, err := parser.ParseFile(token.NewFileSet(), file.path, source, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", file.path, err)
		}
		if importsPgxPool(parsed) {
			pools[file.pkg] = true
		}
		if mentionsRLSTable(parsed) {
			tables[file.pkg] = true
		}
		if selectsDBPool(parsed) {
			reachers[file.pkg] = true
		}
	}

	assertExactly(t, "packages importing pgxpool", pools, poolHolders)
	assertExactly(t, "packages naming an RLS-protected table", tables, rlsTableUsers)
	assertExactly(t, "packages naming DB.Pool", reachers, poolReachers)
}

type goFile struct {
	path string
	// pkg is the directory of the file relative to the module root, which is
	// the identity the allowlists are written in ("internal/outbox").
	pkg string
}

// moduleGoFiles lists every non-test Go file of the module. The walk starts two
// levels up, at the module root, because the invariant is about the WHOLE
// module: a rule checked only inside internal/storage would never see the
// package that breaks it.
func moduleGoFiles(t *testing.T) []goFile {
	t.Helper()
	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatalf("resolve module root: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "go.mod")); err != nil {
		t.Fatalf("expected the module root at %s: %v", root, err)
	}

	var files []goFile
	err = filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		relative, err := filepath.Rel(root, filepath.Dir(path))
		if err != nil {
			return err
		}
		files = append(files, goFile{path: path, pkg: filepath.ToSlash(relative)})
		return nil
	})
	if err != nil {
		t.Fatalf("walk module: %v", err)
	}
	if len(files) == 0 {
		t.Fatal("no Go file found: the audit would pass vacuously")
	}
	return files
}

func importsPgxPool(parsed *ast.File) bool {
	for _, spec := range parsed.Imports {
		if strings.Trim(spec.Path.Value, `"`) == "github.com/jackc/pgx/v5/pgxpool" {
			return true
		}
	}
	return false
}

// selectsDBPool reports a `something.Pool` selector in real code. The AST is
// what makes this precise: a textual search would also flag every declaration
// and comment mentioning the *pgxpool.Pool TYPE, which is a different thing
// entirely from reaching into storage.DB's exported field.
func selectsDBPool(parsed *ast.File) bool {
	found := false
	ast.Inspect(parsed, func(node ast.Node) bool {
		selector, ok := node.(*ast.SelectorExpr)
		if !ok || selector.Sel.Name != "Pool" {
			return true
		}
		// pgxpool.Pool is the package-qualified type name, not a field access.
		if qualifier, ok := selector.X.(*ast.Ident); ok && qualifier.Name == "pgxpool" {
			return true
		}
		found = true
		return false
	})
	return found
}

// mentionsRLSTable reports an SQL statement naming an RLS-protected table. It
// looks inside STRING LITERALS only -- which is where every query of this
// module lives -- so the prose of a comment cannot trip it, and each literal is
// judged on its own.
//
// Two things are required of a literal, and both are what keeps the rule from
// being either blind or noisy:
//
//   - it reads as a statement (SELECT/INSERT/UPDATE/DELETE), so an error
//     message that happens to say "on messages" is not a database access;
//   - it names a table after the keyword that would introduce it, on a source
//     NORMALISED to upper case with whitespace folded to single spaces.
//
// The normalisation is the point of this half. SQL here is written in wrapped
// raw string literals, so the keyword and the table name it introduces are
// routinely separated by a newline and two tabs, and nothing forces a new
// package to write its statements in capitals. Matching the raw text missed
// both -- and a rule that only catches the formatting the current code happens
// to use is not a rule, it is a coincidence.
func mentionsRLSTable(parsed *ast.File) bool {
	found := false
	ast.Inspect(parsed, func(node ast.Node) bool {
		if found {
			return false
		}
		literal, ok := node.(*ast.BasicLit)
		if !ok || literal.Kind != token.STRING {
			return true
		}
		if isRLSStatement(literal.Value) {
			found = true
			return false
		}
		return true
	})
	return found
}

// isRLSStatement applies the two conditions of mentionsRLSTable to one literal.
func isRLSStatement(literal string) bool {
	normalised := normaliseSQL(literal)

	statement := false
	for _, verb := range []string{"SELECT ", "INSERT ", "UPDATE ", "DELETE "} {
		if strings.Contains(normalised, verb) {
			statement = true
			break
		}
	}
	if !statement {
		return false
	}

	for _, table := range rlsTables {
		for _, keyword := range []string{"FROM ", "INTO ", "UPDATE ", "TABLE ", "JOIN ", "ON "} {
			if containsTableReference(normalised, keyword+normaliseSQL(table)) {
				return true
			}
		}
	}
	return false
}

// normaliseSQL folds a source file into the single form the keyword match is
// written against: upper case, one space between tokens.
func normaliseSQL(source string) string {
	return strings.Join(strings.Fields(strings.ToUpper(source)), " ")
}

// containsTableReference reports "<keyword> <table>" occurring in normalised as
// a whole name. The right-hand boundary is what keeps the widened match honest:
// without it, a future `FROM messages_archive` would be read as a reference to
// `messages`.
func containsTableReference(normalised, reference string) bool {
	for offset := 0; ; {
		index := strings.Index(normalised[offset:], reference)
		if index < 0 {
			return false
		}
		end := offset + index + len(reference)
		if end == len(normalised) || !isNameByte(normalised[end]) {
			return true
		}
		offset = end
	}
}

func isNameByte(b byte) bool {
	return b == '_' || (b >= '0' && b <= '9') || (b >= 'A' && b <= 'Z')
}

// compareAllowlist is the exactness rule itself, kept apart from the reporting
// so it can be tested on its own: what appeared without being listed, and what
// is listed but no longer appears.
func compareAllowlist(got map[string]bool, want []string) (unexpected, stale []string) {
	allowed := make(map[string]bool, len(want))
	for _, pkg := range want {
		allowed[pkg] = true
	}

	for pkg := range got {
		if !allowed[pkg] {
			unexpected = append(unexpected, pkg)
		}
	}
	for _, pkg := range want {
		if !got[pkg] {
			stale = append(stale, pkg)
		}
	}
	sort.Strings(unexpected)
	sort.Strings(stale)
	return unexpected, stale
}

func assertExactly(t *testing.T, what string, got map[string]bool, want []string) {
	t.Helper()

	unexpected, stale := compareAllowlist(got, want)

	if len(unexpected) > 0 {
		t.Errorf("%s: %v are not on the allowlist. Tenant data is only reachable through storage.DB.InTenant; "+
			"if this package genuinely needs database access, add it to the allowlist in this file together with the reason it is safe.",
			what, unexpected)
	}
	if len(stale) > 0 {
		t.Errorf("%s: %v are on the allowlist but no longer match. Remove them: an allowlist entry that grants nothing "+
			"is a permission waiting to be reused by accident.", what, stale)
	}
}

// TestRLSTableDetectionSurvivesFormatting probes the audit itself. The rule it
// enforces is only worth as much as its detection: a query is not written in one
// canonical form, and a new package that names an RLS table must be caught
// whatever the gofmt of its SQL. The shapes below are all ordinary Go -- a
// wrapped raw string literal is what any query longer than a line looks like in
// this repository, and lowercase SQL is a matter of taste, not of intent --
// while the four negative cases keep the widened match from flagging prose.
func TestRLSTableDetectionSurvivesFormatting(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
		want bool
	}{
		{
			name: "single line",
			body: "const q = `SELECT * FROM messages WHERE chat_id = $1`",
			want: true,
		},
		{
			name: "wrapped over several lines",
			body: "const q = `SELECT owner_user_id\n\tFROM\n\t\tmessages\n\tWHERE id = $1`",
			want: true,
		},
		{
			name: "lowercase",
			body: "const q = `select * from messages`",
			want: true,
		},
		{
			name: "mixed case and wrapped INSERT",
			body: "const q = `insert into\n  notification_outbox (owner_user_id)\n  values ($1)`",
			want: true,
		},
		{
			name: "wrapped JOIN",
			body: "const q = `SELECT m.id\n\tFROM chats c\n\tJOIN\n\tmedia_files m ON m.chat_id = c.id`",
			want: true,
		},
		{
			name: "interpreted string literal, lowercase",
			body: "func read() { _, _ = pool.Query(ctx, \"delete from data_erasure_requests where id = $1\") }",
			want: true,
		},
		{
			name: "comment naming a table",
			body: "// The chunks are written INTO notification_outbox by the same transaction.\nconst q = 1",
			want: false,
		},
		{
			name: "error message naming a table without a statement",
			body: "const q = \"FORCE ROW LEVEL SECURITY on messages would be decorative\"",
			want: false,
		},
		{
			name: "another table whose name merely starts like one of them",
			body: "const q = `SELECT * FROM messages_archive`",
			want: false,
		},
		{
			name: "Go identifier that merely ends in a table name",
			body: "func countFromMessages() int { return len(deletedMessages) }",
			want: false,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			source := "package probe\n" + tc.body + "\n"
			parsed, err := parser.ParseFile(token.NewFileSet(), "probe.go", source, 0)
			if err != nil {
				t.Fatalf("parse probe: %v", err)
			}
			if got := mentionsRLSTable(parsed); got != tc.want {
				t.Fatalf("mentionsRLSTable(%q) = %t, want %t", tc.body, got, tc.want)
			}
		})
	}
}

// TestAllowlistComparisonReportsUnexpectedAndStale pins the exactness of the
// allowlists themselves: a package that appears without being listed is a
// violation, and a listed package that no longer matches is a permission
// waiting to be reused by accident. Both halves are what makes the audit a
// closed list rather than a minimum.
func TestAllowlistComparisonReportsUnexpectedAndStale(t *testing.T) {
	got := map[string]bool{"internal/outbox": true, "internal/privacy": true}
	unexpected, stale := compareAllowlist(got, []string{"internal/outbox", "internal/media"})

	if len(unexpected) != 1 || unexpected[0] != "internal/privacy" {
		t.Fatalf("unexpected = %v, want [internal/privacy]", unexpected)
	}
	if len(stale) != 1 || stale[0] != "internal/media" {
		t.Fatalf("stale = %v, want [internal/media]", stale)
	}

	if unexpected, stale := compareAllowlist(got, []string{"internal/outbox", "internal/privacy"}); len(unexpected) != 0 || len(stale) != 0 {
		t.Fatalf("an exact match reported (%v, %v), want nothing", unexpected, stale)
	}
}
