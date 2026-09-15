package storage

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net"
	"path"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// This file covers db.go WITHOUT a database: a scripted fake speaks enough of
// the PostgreSQL wire protocol for the real pgxpool / pgx.Conn to connect, and
// the real NewPool, InTenant and RunMigrations run their full statement
// sequences against it. What the fake then observes is exactly what the
// database would see: the statements, their order, and their inlined
// parameters. The semantic half (RLS actually enforced, real DDL) stays the
// integration tests' -- what only a fake can pin is that the role guard
// interrogates the authenticated identity, that the tenant context is set
// LOCAL on every InTenant transaction, and that the migration runner takes its
// advisory lock, walks the ledger, and ships each file verbatim.
//
// The DSN forces default_query_exec_mode=simple_protocol, so every query
// arrives as one 'Q' message with its arguments inlined -- which is also what
// makes the parameters assertable. Anything unscripted fails loudly, so a
// query the test did not plan for can never silently succeed.

// PostgreSQL type OIDs the scripted RowDescriptions need.
const (
	oidText = 25
	oidBool = 16
	oidInt8 = 20
)

// The normalised statements db.go itself emits, minus the ones the fake
// answers as boilerplate (BEGIN/COMMIT/ROLLBACK, the InTenant set_config, the
// pgconn Ping). Scripted rules are consulted FIRST, so a test can also force
// one of the boilerplate statements to fail.
const (
	pingSQL                  = "-- PING"
	roleCheckSQL             = "SELECT CURRENT_USER, ROLSUPER, ROLBYPASSRLS FROM PG_CATALOG.PG_ROLES WHERE ROLNAME = CURRENT_USER"
	tenantContextPrefix      = "SELECT SET_CONFIG("
	advisoryLockPrefix       = "SELECT PG_ADVISORY_LOCK"
	advisoryLockSQL          = "SELECT PG_ADVISORY_LOCK('74617309141001')"
	createMigrationsTableSQL = "CREATE TABLE IF NOT EXISTS SCHEMA_MIGRATIONS"
	migrationExistsPrefix    = "SELECT EXISTS (SELECT 1 FROM SCHEMA_MIGRATIONS WHERE VERSION ="
	insertVersionSQL         = "INSERT INTO SCHEMA_MIGRATIONS"
)

type pgColumn struct {
	name string
	oid  uint32
}

// pgReply is one scripted server answer: an optional RowDescription with its
// DataRows, the CommandComplete tag, or an ErrorResponse when err is set.
type pgReply struct {
	columns []pgColumn
	rows    [][]string
	tag     string
	err     string
}

// scriptedPG is the fake server and its script. Each rule holds a FIFO of
// replies for one statement prefix, so a reply queue can say "first attempt
// fails, second succeeds" for the same statement.
type scriptedPG struct {
	mu    sync.Mutex
	rules []scriptRule
	log   []string
}

type scriptRule struct {
	prefix  string
	replies []pgReply
}

func (s *scriptedPG) on(prefix string, replies ...pgReply) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.rules = append(s.rules, scriptRule{prefix: prefix, replies: replies})
}

func (s *scriptedPG) record(sql string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.log = append(s.log, sql)
}

// queries returns the recorded statements verbatim, which is what proves a
// migration file travelled unmodified.
func (s *scriptedPG) queries() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]string, len(s.log))
	copy(out, s.log)
	return out
}

func (s *scriptedPG) normalizedQueries() []string {
	out := s.queries()
	for i, q := range out {
		out[i] = normSQL(q)
	}
	return out
}

// respond answers one simple-protocol Query. Rules come first so a test can
// script a failure for a statement that is normally boilerplate; the
// boilerplate fallback covers the transaction commands, the InTenant
// set_config and the pgconn Ping; everything else must have been scripted.
func (s *scriptedPG) respond(sql string) pgReply {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := normSQL(sql)
	for i := range s.rules {
		if strings.HasPrefix(n, s.rules[i].prefix) {
			if len(s.rules[i].replies) == 0 {
				return pgReply{err: "no reply left for " + s.rules[i].prefix}
			}
			reply := s.rules[i].replies[0]
			s.rules[i].replies = s.rules[i].replies[1:]
			return reply
		}
	}
	switch n {
	case "BEGIN", "COMMIT", "ROLLBACK":
		return pgReply{tag: n}
	case pingSQL:
		return pgReply{tag: ""}
	}
	if strings.HasPrefix(n, tenantContextPrefix) {
		return pgReply{tag: "SELECT 1"}
	}
	return pgReply{err: "unscripted query: " + n}
}

// newScriptedServer starts the fake on a loopback port and returns its
// address. The whole server is torn down with the test.
func newScriptedServer(t *testing.T) (*scriptedPG, string) {
	t.Helper()
	srv := &scriptedPG{}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	go srv.serve(ln)
	t.Cleanup(func() { ln.Close() })
	return srv, ln.Addr().String()
}

// poolDSN is the DSN of the application role against the fake: simple
// protocol so statements are assertable, one pooled connection so the
// recorded stream is totally ordered, and a health-check period no test can
// outlive so no background Ping interleaves itself into the log.
func poolDSN(addr string) string {
	return fmt.Sprintf(
		"postgres://undelete_app@%s/undelete?sslmode=disable&default_query_exec_mode=simple_protocol&pool_max_conns=1&health_check_period=1h",
		addr)
}

// ownerDSN is the migration DSN: same fake, owner role, no pool parameters.
func ownerDSN(addr string) string {
	return fmt.Sprintf(
		"postgres://owner@%s/undelete?sslmode=disable&default_query_exec_mode=simple_protocol",
		addr)
}

// newScriptedDB returns a DB whose pool really speaks to the fake, bypassing
// NewPool's role guard (which the pool tests exercise on their own).
func newScriptedDB(t *testing.T) (*DB, *scriptedPG) {
	t.Helper()
	srv, addr := newScriptedServer(t)
	pool, err := pgxpool.New(context.Background(), poolDSN(addr))
	if err != nil {
		t.Fatalf("pgxpool.New: %v", err)
	}
	t.Cleanup(pool.Close)
	return &DB{Pool: pool}, srv
}

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// embeddedMigrations lists the embedded .sql files sorted by name, the exact
// enumeration RunMigrations itself performs.
func embeddedMigrations(t *testing.T) []string {
	t.Helper()
	entries, err := fs.ReadDir(migrationsFS, "migrations")
	if err != nil {
		t.Fatalf("reading embedded migrations: %v", err)
	}
	var names []string
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".sql") {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	return names
}

func migrationContent(t *testing.T, name string) string {
	t.Helper()
	b, err := migrationsFS.ReadFile(path.Join("migrations", name))
	if err != nil {
		t.Fatalf("reading migration %s: %v", name, err)
	}
	return string(b)
}

// migrationPrefix keys a script rule to one migration file: the normalised
// first line of the file, which every current file opens with a distinctive
// comment. The empty check keeps a future file starting with a blank line
// from silently producing a rule that matches everything.
func migrationPrefix(t *testing.T, name string) string {
	t.Helper()
	firstLine := strings.SplitN(migrationContent(t, name), "\n", 2)[0]
	prefix := normSQL(firstLine)
	if prefix == "" {
		t.Fatalf("migration %s starts with an empty first line: no rule can key on it", name)
	}
	return prefix
}

// ledgerReply answers the schema_migrations EXISTS check.
func ledgerReply(applied bool) pgReply {
	value := "f"
	if applied {
		value = "t"
	}
	return pgReply{
		columns: []pgColumn{{name: "exists", oid: oidBool}},
		rows:    [][]string{{value}},
		tag:     "SELECT 1",
	}
}

// countExact counts the recorded statements normalising exactly to want.
func countExact(queries []string, want string) int {
	n := 0
	for _, q := range queries {
		if normSQL(q) == want {
			n++
		}
	}
	return n
}

// countPrefix counts the recorded statements normalising with prefix.
func countPrefix(queries []string, prefix string) int {
	n := 0
	for _, q := range queries {
		if strings.HasPrefix(normSQL(q), prefix) {
			n++
		}
	}
	return n
}

func (s *scriptedPG) serve(ln net.Listener) {
	for {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		go s.handle(conn)
	}
}

func (s *scriptedPG) handle(conn net.Conn) {
	defer conn.Close()
	if !s.handshake(conn) {
		return
	}
	inTx := false
	for {
		msgType, payload, err := readPGMessage(conn)
		if err != nil {
			return
		}
		switch msgType {
		case 'Q':
			// The Query body carries the statement terminated by a NUL.
			sql := strings.TrimRight(string(payload), "\x00")
			s.record(sql)
			switch normSQL(sql) {
			case "BEGIN":
				inTx = true
			case "COMMIT", "ROLLBACK":
				inTx = false
			}
			if !s.answer(conn, s.respond(sql), inTx) {
				return
			}
		case 'X': // Terminate
			return
		default:
			return
		}
	}
}

// handshake reads the startup packet and answers the minimum a pgx client
// needs to consider the connection usable: AuthenticationOk, the parameter
// statuses the simple-protocol sanitizer insists on, and ReadyForQuery.
func (s *scriptedPG) handshake(conn net.Conn) bool {
	head := make([]byte, 4)
	if _, err := io.ReadFull(conn, head); err != nil {
		return false
	}
	length := binary.BigEndian.Uint32(head)
	if length < 8 || length > 1<<16 {
		return false
	}
	body := make([]byte, length-4)
	if _, err := io.ReadFull(conn, body); err != nil {
		return false
	}
	if binary.BigEndian.Uint32(body[:4])>>16 != 3 {
		return false // not protocol 3.x
	}
	ok := writeMessage(conn, 'R', binary.BigEndian.AppendUint32(nil, 0))
	ok = ok && writeParameterStatus(conn, "client_encoding", "UTF8")
	ok = ok && writeParameterStatus(conn, "standard_conforming_strings", "on")
	ok = ok && writeParameterStatus(conn, "server_version", "16.0")
	backendKey := binary.BigEndian.AppendUint32(binary.BigEndian.AppendUint32(nil, 27), 1)
	ok = ok && writeMessage(conn, 'K', backendKey)
	return ok && writeReadyForQuery(conn, 'I')
}

func (s *scriptedPG) answer(conn net.Conn, reply pgReply, inTx bool) bool {
	if reply.err != "" {
		if !writeError(conn, reply.err) {
			return false
		}
	} else {
		if len(reply.columns) > 0 {
			if !writeRowDescription(conn, reply.columns) {
				return false
			}
			for _, row := range reply.rows {
				if !writeDataRow(conn, row) {
					return false
				}
			}
		}
		if !writeCommandComplete(conn, reply.tag) {
			return false
		}
	}
	status := byte('I')
	if inTx {
		status = 'T'
	}
	return writeReadyForQuery(conn, status)
}

func readPGMessage(conn net.Conn) (byte, []byte, error) {
	head := make([]byte, 5)
	if _, err := io.ReadFull(conn, head); err != nil {
		return 0, nil, err
	}
	length := binary.BigEndian.Uint32(head[1:])
	if length < 4 || length > 1<<20 {
		return 0, nil, fmt.Errorf("bad message length %d", length)
	}
	payload := make([]byte, length-4)
	if _, err := io.ReadFull(conn, payload); err != nil {
		return 0, nil, err
	}
	return head[0], payload, nil
}

func writeMessage(conn net.Conn, msgType byte, payload []byte) bool {
	buf := make([]byte, 0, 5+len(payload))
	buf = append(buf, msgType)
	buf = binary.BigEndian.AppendUint32(buf, uint32(len(payload)+4))
	buf = append(buf, payload...)
	_, err := conn.Write(buf)
	return err == nil
}

func writeParameterStatus(conn net.Conn, name, value string) bool {
	var p []byte
	p = append(p, name...)
	p = append(p, 0)
	p = append(p, value...)
	p = append(p, 0)
	return writeMessage(conn, 'S', p)
}

func writeRowDescription(conn net.Conn, columns []pgColumn) bool {
	var p []byte
	p = binary.BigEndian.AppendUint16(p, uint16(len(columns)))
	for _, c := range columns {
		p = append(p, c.name...)
		p = append(p, 0)
		p = binary.BigEndian.AppendUint32(p, 0) // table oid
		p = binary.BigEndian.AppendUint16(p, 0) // column attribute
		p = binary.BigEndian.AppendUint32(p, c.oid)
		p = binary.BigEndian.AppendUint16(p, 0) // type length, unused by the client
		p = binary.BigEndian.AppendUint32(p, 0) // typmod
		p = binary.BigEndian.AppendUint16(p, 0) // format 0: text
	}
	return writeMessage(conn, 'T', p)
}

func writeDataRow(conn net.Conn, values []string) bool {
	var p []byte
	p = binary.BigEndian.AppendUint16(p, uint16(len(values)))
	for _, v := range values {
		p = binary.BigEndian.AppendUint32(p, uint32(len(v)))
		p = append(p, v...)
	}
	return writeMessage(conn, 'D', p)
}

func writeCommandComplete(conn net.Conn, tag string) bool {
	return writeMessage(conn, 'C', append([]byte(tag), 0))
}

func writeError(conn net.Conn, message string) bool {
	p := []byte{'S'}
	p = append(p, "ERROR"...)
	p = append(p, 0, 'C')
	p = append(p, "XX000"...)
	p = append(p, 0, 'M')
	p = append(p, message...)
	p = append(p, 0, 0)
	return writeMessage(conn, 'E', p)
}

func writeReadyForQuery(conn net.Conn, status byte) bool {
	return writeMessage(conn, 'Z', []byte{status})
}

// normSQL folds a statement into the single form the scripted rules and the
// assertions below are written against: upper case, whitespace collapsed, and
// the space padding pgx wraps around every simple-protocol argument removed.
func normSQL(sql string) string {
	folded := strings.Join(strings.Fields(strings.ToUpper(sql)), " ")
	folded = strings.ReplaceAll(folded, "( ", "(")
	folded = strings.ReplaceAll(folded, " ,", ",")
	folded = strings.ReplaceAll(folded, " )", ")")
	return folded
}

// roleCheckReply answers the NewPool identity check with one pg_roles row.
func roleCheckReply(role string, superuser, bypassRLS bool) pgReply {
	super, bypass := "f", "f"
	if superuser {
		super = "t"
	}
	if bypassRLS {
		bypass = "t"
	}
	return pgReply{
		columns: []pgColumn{
			{name: "current_user", oid: oidText},
			{name: "rolsuper", oid: oidBool},
			{name: "rolbypassrls", oid: oidBool},
		},
		rows: [][]string{{role, super, bypass}},
		tag:  "SELECT 1",
	}
}

// TestNewPoolVerifiesTheAuthenticatedRuntimeRole pins the second half of the
// DSN safeguard: whatever DATABASE_URL says, the pool refuses to exist unless
// the AUTHENTICATED role is the restricted undelete_app, neither superuser nor
// RLS-bypassing. The textual comparison in config.Load cannot catch two
// different strings designating the same privileged role; this check can.
func TestNewPoolVerifiesTheAuthenticatedRuntimeRole(t *testing.T) {
	t.Run("the restricted role opens the pool", func(t *testing.T) {
		srv, addr := newScriptedServer(t)
		srv.on(roleCheckSQL, roleCheckReply("undelete_app", false, false))

		db, err := NewPool(context.Background(), poolDSN(addr))
		if err != nil {
			t.Fatalf("NewPool: %v", err)
		}
		if db.Pool == nil {
			t.Fatal("NewPool returned a DB without a pool")
		}
		defer db.Close()

		queries := srv.normalizedQueries()
		if len(queries) != 2 || queries[0] != pingSQL || queries[1] != roleCheckSQL {
			t.Fatalf("queries = %v, want the connection ping then the role check", queries)
		}
	})

	for _, tc := range []struct {
		name string
		role string
		rows [][]string
		want string // a fragment of the report the error must carry
	}{
		{
			name: "another role is refused",
			role: "somebody_else",
			rows: [][]string{{"somebody_else", "f", "f"}},
			want: `role="somebody_else"`,
		},
		{
			name: "a superuser undelete_app is refused",
			role: "undelete_app",
			rows: [][]string{{"undelete_app", "t", "f"}},
			want: "superuser=true",
		},
		{
			name: "an RLS-bypassing undelete_app is refused",
			role: "undelete_app",
			rows: [][]string{{"undelete_app", "f", "t"}},
			want: "bypassrls=true",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, addr := newScriptedServer(t)
			reply := roleCheckReply(tc.role, false, false)
			reply.rows = tc.rows
			srv.on(roleCheckSQL, reply)

			_, err := NewPool(context.Background(), poolDSN(addr))
			if !errors.Is(err, ErrUnsafeRuntimeRole) {
				t.Fatalf("NewPool error = %v, want ErrUnsafeRuntimeRole", err)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error %q does not report %q", err, tc.want)
			}
		})
	}
}

// TestNewPoolFailuresSurfaceAtTheirStage: every stage NewPool passes through
// can fail, and each failure must name its stage rather than leak a half-open
// pool -- in particular a database that answers but whose catalog cannot be
// read must not hand out a pool whose role was never verified.
func TestNewPoolFailuresSurfaceAtTheirStage(t *testing.T) {
	t.Run("an unparseable DSN fails at construction", func(t *testing.T) {
		_, err := NewPool(context.Background(), "not a dsn")
		if err == nil || !strings.Contains(err.Error(), "opening application pool") {
			t.Fatalf("NewPool error = %v, want the construction stage", err)
		}
	})

	t.Run("an unreachable server fails at the ping", func(t *testing.T) {
		_, err := NewPool(context.Background(), "postgres://undelete_app@127.0.0.1:1/undelete?sslmode=disable")
		if err == nil || !strings.Contains(err.Error(), "pinging application pool") {
			t.Fatalf("NewPool error = %v, want the ping stage", err)
		}
	})

	t.Run("a broken role check fails before the pool is returned", func(t *testing.T) {
		srv, addr := newScriptedServer(t)
		srv.on(roleCheckSQL, pgReply{err: "pg_roles unavailable"})

		_, err := NewPool(context.Background(), poolDSN(addr))
		if err == nil || !strings.Contains(err.Error(), "checking application role") {
			t.Fatalf("NewPool error = %v, want the role check stage", err)
		}
		if errors.Is(err, ErrUnsafeRuntimeRole) {
			t.Fatalf("a broken check is not a verdict on the role: %v", err)
		}
	})
}

// TestInTenantRunsTheFnInsideATenantScopedTransaction pins the InTenant
// contract on the wire: one transaction, opened with
// app.current_owner_user_id set to THIS owner and LOCAL to that transaction
// (the set_config third argument true), the fn's statements inside it, and a
// commit at the end. LOCAL is what keeps a pooled connection from leaking one
// tenant's context into the next tenant's transaction.
func TestInTenantRunsTheFnInsideATenantScopedTransaction(t *testing.T) {
	db, srv := newScriptedDB(t)
	srv.on("SELECT COUNT(*) FROM MESSAGES", pgReply{
		columns: []pgColumn{{name: "count", oid: oidInt8}},
		rows:    [][]string{{"0"}},
		tag:     "SELECT 1",
	})

	var messages int64
	err := db.InTenant(context.Background(), 11, func(tx pgx.Tx) error {
		return tx.QueryRow(context.Background(),
			`SELECT count(*) FROM messages WHERE chat_id = $1`, 42).Scan(&messages)
	})
	if err != nil {
		t.Fatalf("InTenant: %v", err)
	}
	if messages != 0 {
		t.Fatalf("the fn read count = %d, want the 0 the database returned", messages)
	}

	want := []string{
		"BEGIN",
		"SELECT SET_CONFIG('APP.CURRENT_OWNER_USER_ID', '11', TRUE)",
		"SELECT COUNT(*) FROM MESSAGES WHERE CHAT_ID = '42'",
		"COMMIT",
	}
	queries := srv.normalizedQueries()
	if len(queries) != len(want) {
		t.Fatalf("queries = %v, want %v", queries, want)
	}
	for i := range want {
		if queries[i] != want[i] {
			t.Fatalf("query %d = %q, want %q (stream: %v)", i, queries[i], want[i], queries)
		}
	}
}

// TestInTenantSurfacesTheFnErrorAndRollsBack: an fn that fails must abort the
// transaction (nothing it wrote survives) and return the fn's error itself,
// unwrapped, so callers can branch on their own sentinel values.
func TestInTenantSurfacesTheFnErrorAndRollsBack(t *testing.T) {
	db, srv := newScriptedDB(t)

	boom := errors.New("fn failed")
	err := db.InTenant(context.Background(), 11, func(tx pgx.Tx) error { return boom })
	if !errors.Is(err, boom) {
		t.Fatalf("InTenant error = %v, want the fn error itself", err)
	}

	queries := srv.normalizedQueries()
	want := []string{
		"BEGIN",
		"SELECT SET_CONFIG('APP.CURRENT_OWNER_USER_ID', '11', TRUE)",
		"ROLLBACK",
	}
	if len(queries) != len(want) {
		t.Fatalf("queries = %v, want %v", queries, want)
	}
	for i := range want {
		if queries[i] != want[i] {
			t.Fatalf("query %d = %q, want %q", i, queries[i], want[i])
		}
	}
	if countExact(srv.queries(), "COMMIT") != 0 {
		t.Fatalf("a failed InTenant transaction committed: %v", srv.normalizedQueries())
	}
}

// TestInTenantFailuresSurfaceAtTheirStage: the transaction scaffolding itself
// can fail at each of its three steps, and each failure must name its stage.
func TestInTenantFailuresSurfaceAtTheirStage(t *testing.T) {
	t.Run("the transaction cannot open", func(t *testing.T) {
		db, srv := newScriptedDB(t)
		srv.on("BEGIN", pgReply{err: "cannot begin"})

		err := db.InTenant(context.Background(), 11, func(tx pgx.Tx) error { return nil })
		if err == nil || !strings.Contains(err.Error(), "opening InTenant transaction") {
			t.Fatalf("InTenant error = %v, want the opening stage", err)
		}
		if queries := srv.normalizedQueries(); len(queries) != 1 || queries[0] != "BEGIN" {
			t.Fatalf("queries = %v, want only the refused BEGIN", queries)
		}
	})

	t.Run("the tenant context cannot be set", func(t *testing.T) {
		db, srv := newScriptedDB(t)
		srv.on(tenantContextPrefix, pgReply{err: "unknown parameter"})

		err := db.InTenant(context.Background(), 11, func(tx pgx.Tx) error { return nil })
		if err == nil || !strings.Contains(err.Error(), "setting RLS context") {
			t.Fatalf("InTenant error = %v, want the context stage", err)
		}
		if last := srv.normalizedQueries(); len(last) == 0 || last[len(last)-1] != "ROLLBACK" {
			t.Fatalf("queries = %v, want the failed transaction rolled back", last)
		}
	})

	t.Run("the transaction cannot commit", func(t *testing.T) {
		db, srv := newScriptedDB(t)
		srv.on("COMMIT", pgReply{err: "cannot commit"})

		err := db.InTenant(context.Background(), 11, func(tx pgx.Tx) error { return nil })
		if err == nil || !strings.Contains(err.Error(), "committing InTenant transaction") {
			t.Fatalf("InTenant error = %v, want the commit stage", err)
		}
		if queries := srv.normalizedQueries(); len(queries) != 3 || queries[2] != "COMMIT" {
			t.Fatalf("queries = %v, want begin, context, refused commit", queries)
		}
	})
}

// TestRunMigrationsAppliesEveryEmbeddedMigrationInOrder pins the boot
// sequence on the wire: the session advisory lock with its fixed id is taken
// BEFORE anything else (two replicas must not run the same DDL concurrently,
// and the id is a cross-release constant: an old and a new replica starting
// together must still serialise on the same lock), then the ledger table is
// bootstrapped, then every embedded file travels VERBATIM -- one transaction
// per migration, each version recorded before its commit.
func TestRunMigrationsAppliesEveryEmbeddedMigrationInOrder(t *testing.T) {
	srv, addr := newScriptedServer(t)
	names := embeddedMigrations(t)
	if len(names) == 0 {
		t.Fatal("no embedded migration found: the test would pass vacuously")
	}

	srv.on(advisoryLockPrefix, pgReply{tag: "SELECT 1"})
	srv.on(createMigrationsTableSQL, pgReply{tag: "CREATE TABLE"})
	notApplied := make([]pgReply, len(names))
	for i := range notApplied {
		notApplied[i] = ledgerReply(false)
	}
	srv.on(migrationExistsPrefix, notApplied...)
	for _, name := range names {
		srv.on(migrationPrefix(t, name), pgReply{tag: "CREATE TABLE"})
	}
	inserts := make([]pgReply, len(names))
	for i := range inserts {
		inserts[i] = pgReply{tag: "INSERT 0 1"}
	}
	srv.on(insertVersionSQL, inserts...)

	if err := RunMigrations(context.Background(), ownerDSN(addr), discardLogger()); err != nil {
		t.Fatalf("RunMigrations: %v", err)
	}

	raw := srv.queries()
	if len(raw) != 2+5*len(names) {
		t.Fatalf("got %d queries, want %d (lock, bootstrap, then 5 per migration): %v",
			len(raw), 2+5*len(names), srv.normalizedQueries())
	}
	if got := normSQL(raw[0]); got != advisoryLockSQL {
		t.Fatalf("query 0 = %q, want the session advisory lock %q taken first", got, advisoryLockSQL)
	}
	if got, want := normSQL(raw[1]),
		"CREATE TABLE IF NOT EXISTS SCHEMA_MIGRATIONS (VERSION INT PRIMARY KEY, APPLIED_AT TIMESTAMPTZ NOT NULL DEFAULT NOW())"; got != want {
		t.Fatalf("query 1 = %q, want the ledger bootstrap %q", got, want)
	}
	for i, name := range names {
		version, err := parseVersion(name)
		if err != nil {
			t.Fatalf("parseVersion(%s): %v", name, err)
		}
		block := raw[2+5*i : 2+5*i+5]
		if got, want := normSQL(block[0]), fmt.Sprintf(
			"SELECT EXISTS (SELECT 1 FROM SCHEMA_MIGRATIONS WHERE VERSION = '%d')", version); got != want {
			t.Fatalf("migration %s: ledger check = %q, want %q", name, got, want)
		}
		if got := normSQL(block[1]); got != "BEGIN" {
			t.Fatalf("migration %s: expected its own transaction to open, got %q", name, got)
		}
		if block[2] != migrationContent(t, name) {
			t.Fatalf("migration %s did not travel verbatim: %q", name, block[2])
		}
		if got, want := normSQL(block[3]), fmt.Sprintf(
			"INSERT INTO SCHEMA_MIGRATIONS (VERSION) VALUES ('%d')", version); got != want {
			t.Fatalf("migration %s: recording = %q, want %q", name, got, want)
		}
		if got := normSQL(block[4]); got != "COMMIT" {
			t.Fatalf("migration %s: expected a commit, got %q", name, got)
		}
	}
}

// TestRunMigrationsSkipsVersionsTheLedgerAlreadyRecords: a version present in
// schema_migrations is never re-executed -- rerunning the binary on an
// existing database must issue no DDL at all beyond the ledger checks.
func TestRunMigrationsSkipsVersionsTheLedgerAlreadyRecords(t *testing.T) {
	srv, addr := newScriptedServer(t)
	names := embeddedMigrations(t)

	srv.on(advisoryLockPrefix, pgReply{tag: "SELECT 1"})
	srv.on(createMigrationsTableSQL, pgReply{tag: "CREATE TABLE"})
	ledger := make([]pgReply, len(names))
	for i, name := range names {
		version, err := parseVersion(name)
		if err != nil {
			t.Fatalf("parseVersion(%s): %v", name, err)
		}
		// Everything already applied except version 2.
		ledger[i] = ledgerReply(version != 2)
	}
	srv.on(migrationExistsPrefix, ledger...)
	srv.on(migrationPrefix(t, "0002_notification_outbox.sql"), pgReply{tag: "CREATE TABLE"})
	srv.on(insertVersionSQL, pgReply{tag: "INSERT 0 1"})

	if err := RunMigrations(context.Background(), ownerDSN(addr), discardLogger()); err != nil {
		t.Fatalf("RunMigrations: %v", err)
	}

	raw := srv.queries()
	if len(raw) != 2+len(names)+4 {
		t.Fatalf("got %d queries, want %d (lock, bootstrap, one ledger check per version, then only version 2's block): %v",
			len(raw), 2+len(names)+4, srv.normalizedQueries())
	}
	// The ledger checks interleave with the applied block: check 1, check 2
	// (not applied), version 2's transaction, check 3, ... So walk a cursor
	// instead of slicing the checks out as one contiguous run.
	idx := 2
	for _, name := range names {
		version, err := parseVersion(name)
		if err != nil {
			t.Fatalf("parseVersion(%s): %v", name, err)
		}
		if got, want := normSQL(raw[idx]), fmt.Sprintf(
			"SELECT EXISTS (SELECT 1 FROM SCHEMA_MIGRATIONS WHERE VERSION = '%d')", version); got != want {
			t.Fatalf("ledger check for %s = %q, want %q", name, got, want)
		}
		idx++
		if version != 2 {
			continue
		}
		block := raw[idx : idx+4]
		if got := normSQL(block[0]); got != "BEGIN" {
			t.Fatalf("query after the check = %q, want BEGIN", got)
		}
		if block[1] != migrationContent(t, "0002_notification_outbox.sql") {
			t.Fatalf("the only migration executed = %q, want 0002 verbatim", block[1])
		}
		if got, want := normSQL(block[2]), "INSERT INTO SCHEMA_MIGRATIONS (VERSION) VALUES ('2')"; got != want {
			t.Fatalf("recording = %q, want %q", got, want)
		}
		if got := normSQL(block[3]); got != "COMMIT" {
			t.Fatalf("query after the recording = %q, want COMMIT", got)
		}
		idx += 4
	}
	if idx != len(raw) {
		t.Fatalf("walked %d queries, stream holds %d: %v", idx, len(raw), srv.normalizedQueries())
	}
}

// TestRunMigrationsFailuresSurfaceAtTheirStage: every stage of the runner can
// fail, and each failure must name its stage -- a migration half-applied but
// reported as done would leave the schema in a state no version number
// describes. The apply/record failures must also roll their transaction back.
func TestRunMigrationsFailuresSurfaceAtTheirStage(t *testing.T) {
	t.Run("the owner connection is refused", func(t *testing.T) {
		err := RunMigrations(context.Background(),
			"postgres://owner@127.0.0.1:1/undelete?sslmode=disable", discardLogger())
		if err == nil || !strings.Contains(err.Error(), "connecting for migrations") {
			t.Fatalf("RunMigrations error = %v, want the connection stage", err)
		}
	})

	t.Run("the advisory lock is denied", func(t *testing.T) {
		srv, addr := newScriptedServer(t)
		srv.on(advisoryLockPrefix, pgReply{err: "lock denied"})

		err := RunMigrations(context.Background(), ownerDSN(addr), discardLogger())
		if err == nil || !strings.Contains(err.Error(), "locking the migration runner") {
			t.Fatalf("RunMigrations error = %v, want the lock stage", err)
		}
		if countPrefix(srv.queries(), createMigrationsTableSQL) != 0 {
			t.Fatalf("DDL ran without the lock: %v", srv.normalizedQueries())
		}
	})

	t.Run("the ledger table cannot be created", func(t *testing.T) {
		srv, addr := newScriptedServer(t)
		srv.on(advisoryLockPrefix, pgReply{tag: "SELECT 1"})
		srv.on(createMigrationsTableSQL, pgReply{err: "permission denied"})

		err := RunMigrations(context.Background(), ownerDSN(addr), discardLogger())
		if err == nil || !strings.Contains(err.Error(), "creating schema_migrations") {
			t.Fatalf("RunMigrations error = %v, want the bootstrap stage", err)
		}
	})

	t.Run("a ledger check fails", func(t *testing.T) {
		srv, addr := newScriptedServer(t)
		srv.on(advisoryLockPrefix, pgReply{tag: "SELECT 1"})
		srv.on(createMigrationsTableSQL, pgReply{tag: "CREATE TABLE"})
		srv.on(migrationExistsPrefix, pgReply{err: "catalog corrupted"})

		err := RunMigrations(context.Background(), ownerDSN(addr), discardLogger())
		if err == nil || !strings.Contains(err.Error(), "checking migration 1") {
			t.Fatalf("RunMigrations error = %v, want the check stage", err)
		}
		if countExact(srv.queries(), "BEGIN") != 0 {
			t.Fatalf("a migration ran although its ledger check failed: %v", srv.normalizedQueries())
		}
	})

	t.Run("a migration's transaction cannot open", func(t *testing.T) {
		srv, addr := newScriptedServer(t)
		srv.on(advisoryLockPrefix, pgReply{tag: "SELECT 1"})
		srv.on(createMigrationsTableSQL, pgReply{tag: "CREATE TABLE"})
		srv.on(migrationExistsPrefix, ledgerReply(false))
		srv.on(migrationPrefix(t, "0001_init.sql"), pgReply{tag: "CREATE TABLE"})
		srv.on("BEGIN", pgReply{err: "cannot begin"})

		err := RunMigrations(context.Background(), ownerDSN(addr), discardLogger())
		if err == nil || !strings.Contains(err.Error(), "opening migration 1 transaction") {
			t.Fatalf("RunMigrations error = %v, want the transaction stage", err)
		}
	})

	t.Run("applying a migration fails and rolls back", func(t *testing.T) {
		srv, addr := newScriptedServer(t)
		srv.on(advisoryLockPrefix, pgReply{tag: "SELECT 1"})
		srv.on(createMigrationsTableSQL, pgReply{tag: "CREATE TABLE"})
		srv.on(migrationExistsPrefix, ledgerReply(false))
		srv.on(migrationPrefix(t, "0001_init.sql"), pgReply{err: "syntax error"})

		err := RunMigrations(context.Background(), ownerDSN(addr), discardLogger())
		if err == nil || !strings.Contains(err.Error(), "applying migration 1") {
			t.Fatalf("RunMigrations error = %v, want the apply stage", err)
		}
		raw := srv.queries()
		if countPrefix(raw, insertVersionSQL) != 0 {
			t.Fatalf("a failed migration was recorded as applied: %v", srv.normalizedQueries())
		}
		if countExact(raw, "COMMIT") != 0 {
			t.Fatalf("a failed migration was committed: %v", srv.normalizedQueries())
		}
		if last := normSQL(raw[len(raw)-1]); last != "ROLLBACK" {
			t.Fatalf("last query = %q, want the failed transaction rolled back", last)
		}
	})

	t.Run("recording a migration fails and rolls back", func(t *testing.T) {
		srv, addr := newScriptedServer(t)
		srv.on(advisoryLockPrefix, pgReply{tag: "SELECT 1"})
		srv.on(createMigrationsTableSQL, pgReply{tag: "CREATE TABLE"})
		srv.on(migrationExistsPrefix, ledgerReply(false))
		srv.on(migrationPrefix(t, "0001_init.sql"), pgReply{tag: "CREATE TABLE"})
		srv.on(insertVersionSQL, pgReply{err: "unique violation"})

		err := RunMigrations(context.Background(), ownerDSN(addr), discardLogger())
		if err == nil || !strings.Contains(err.Error(), "recording migration 1") {
			t.Fatalf("RunMigrations error = %v, want the recording stage", err)
		}
		raw := srv.queries()
		if countExact(raw, "COMMIT") != 0 {
			t.Fatalf("an unrecorded migration was committed: %v", srv.normalizedQueries())
		}
		if last := normSQL(raw[len(raw)-1]); last != "ROLLBACK" {
			t.Fatalf("last query = %q, want the failed transaction rolled back", last)
		}
	})

	t.Run("committing a migration fails", func(t *testing.T) {
		srv, addr := newScriptedServer(t)
		srv.on(advisoryLockPrefix, pgReply{tag: "SELECT 1"})
		srv.on(createMigrationsTableSQL, pgReply{tag: "CREATE TABLE"})
		srv.on(migrationExistsPrefix, ledgerReply(false))
		srv.on(migrationPrefix(t, "0001_init.sql"), pgReply{tag: "CREATE TABLE"})
		srv.on(insertVersionSQL, pgReply{tag: "INSERT 0 1"})
		srv.on("COMMIT", pgReply{err: "could not serialize"})

		err := RunMigrations(context.Background(), ownerDSN(addr), discardLogger())
		if err == nil || !strings.Contains(err.Error(), "committing migration 1") {
			t.Fatalf("RunMigrations error = %v, want the commit stage", err)
		}
		if last := normSQL(srv.queries()[len(srv.queries())-1]); last != "COMMIT" {
			t.Fatalf("last query = %q, want the refused commit", last)
		}
	})
}
