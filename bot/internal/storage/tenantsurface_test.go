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
		if mentionsRLSTable(string(source)) {
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

// mentionsRLSTable looks for a table name preceded by the SQL keyword that
// would introduce it. Matching the bare name would flag every comment that
// merely says "messages".
func mentionsRLSTable(source string) bool {
	for _, table := range rlsTables {
		for _, keyword := range []string{"FROM ", "INTO ", "UPDATE ", "TABLE ", "JOIN ", "ON "} {
			if strings.Contains(source, keyword+table) {
				return true
			}
		}
	}
	return false
}

func assertExactly(t *testing.T, what string, got map[string]bool, want []string) {
	t.Helper()

	allowed := make(map[string]bool, len(want))
	for _, pkg := range want {
		allowed[pkg] = true
	}

	var unexpected []string
	for pkg := range got {
		if !allowed[pkg] {
			unexpected = append(unexpected, pkg)
		}
	}
	var stale []string
	for _, pkg := range want {
		if !got[pkg] {
			stale = append(stale, pkg)
		}
	}
	sort.Strings(unexpected)
	sort.Strings(stale)

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
