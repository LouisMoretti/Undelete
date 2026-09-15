package users

import (
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"strings"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

// TestNewRepositoryReturnsHandle pins the constructor: users is an allowlisted
// pool holder (with storage only) and the handle must be usable without any
// tenant context -- the table carries no RLS and is reached without InTenant
// by construction.
func TestNewRepositoryReturnsHandle(t *testing.T) {
	if got := NewRepository(nil); got == nil {
		t.Fatal("NewRepository(nil) = nil, want a usable handle")
	}
}

// TestParseRetentionDaysRejectsUnrepresentableInteger covers the strconv.Atoi
// failure branch: a canonical digits-only token that no int can hold. Earlier
// guards (single token, no leading zero, digits only) all pass, so only the
// Atoi check can refuse it -- the same answerable error as any other invalid
// form, and the caller must change nothing.
func TestParseRetentionDaysRejectsUnrepresentableInteger(t *testing.T) {
	// All above MaxInt64 on 64-bit platforms: digits-only, canonical, but
	// unrepresentable, so strconv.Atoi fails after the digit loop passed.
	unrepresentable := []string{
		"9223372036854775808",
		"99999999999999999999",
		"10000000000000000000000",
	}
	for _, argument := range unrepresentable {
		t.Run("reject "+argument, func(t *testing.T) {
			if days, err := ParseRetentionDays(argument); err == nil {
				t.Fatalf("ParseRetentionDays(%q) = %d, want an error", argument, days)
			} else if !strings.Contains(err.Error(), "between") {
				t.Fatalf("ParseRetentionDays(%q) error = %q, want it to name the bounds", argument, err)
			}
		})
	}
}

// TestSetRetentionDaysRejectsOutOfBoundsWithoutDatabase pins that the Go
// validation runs before any database access: with a nil pool, out-of-bounds
// values must fail with the answerable validation error instead of panicking
// on the pool. Valid values are exercised against the scripted server below.
func TestSetRetentionDaysRejectsOutOfBoundsWithoutDatabase(t *testing.T) {
	repo := NewRepository(nil)
	ctx := context.Background()

	invalid := []int{
		MinRetentionDays - 1,
		MaxRetentionDays + 1,
		-1,
		-100,
		1000000,
	}
	for _, days := range invalid {
		t.Run(fmt.Sprintf("refuse %d", days), func(t *testing.T) {
			err := repo.SetRetentionDays(ctx, 42, days)
			if err == nil {
				t.Fatalf("SetRetentionDays(ctx, 42, %d) = nil, want an out-of-bounds error", days)
			}
			if !strings.Contains(err.Error(), "out of bounds") {
				t.Fatalf("SetRetentionDays(ctx, 42, %d) error = %q, want the validation error, not a database failure", days, err)
			}
		})
	}
}

// This second half covers the SQL half of the package, WITHOUT a database: a
// scripted fake speaks enough of the PostgreSQL wire protocol for the real
// pgxpool to connect, and the repository runs its real statements against it.
// What the fake then observes is exactly what the database would see: the
// statements, their order, and their inlined parameters. The semantic half
// (the actual upsert, the CHECK constraint backstop, multi-tenant isolation)
// stays the integration tests' -- what only a fake can pin is the opposite of
// the erasure/messages rule: users is the ROOT identity table, queried
// directly on the app pool WITHOUT storage.DB.InTenant. It cannot set
// app.current_owner_user_id before knowing which owner_user_id maps to a
// telegram_user_id -- doing so would be circular -- so every statement must
// travel in autocommit, with no tenant context and no transaction wrapper.
//
// The DSN forces default_query_exec_mode=simple_protocol, so every query
// arrives as one 'Q' message with its arguments inlined -- which is also what
// makes the parameters assertable. The fake answers anything unscripted with
// an error, so a query the test did not plan for fails loudly instead of
// silently succeeding.

// PostgreSQL type OIDs the scripted RowDescriptions need.
const (
	oidInt8 = 20
	oidInt4 = 23
)

// The normalised statements the repository issues, recognised by prefix. In
// simple protocol every argument arrives quoted and space-padded (pgx's
// comment-injection mitigation), which normSQL folds away.
const (
	upsertSQL       = "INSERT INTO USERS"
	getRetentionSQL = "SELECT RETENTION_DAYS FROM USERS"
	setRetentionSQL = "UPDATE USERS SET RETENTION_DAYS"
	listTenantsSQL  = "SELECT ID, RETENTION_DAYS FROM USERS"
)

type pgColumn struct {
	name string
	oid  uint32
}

// pgReply is one scripted server answer: an optional RowDescription with its
// DataRows, the CommandComplete tag, or an ErrorResponse when err is set.
// errAfterRows breaks the stream AFTER the DataRows (no CommandComplete): the
// rows the client already read stand, and the error surfaces through
// rows.Err().
type pgReply struct {
	columns      []pgColumn
	rows         [][]string
	tag          string
	err          string
	errAfterRows string
}

// scriptedPG is the fake server and its script. Each rule holds a FIFO of
// replies for one statement prefix.
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

func (s *scriptedPG) normalizedQueries() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]string, len(s.log))
	for i, q := range s.log {
		out[i] = normSQL(q)
	}
	return out
}

// respond answers one simple-protocol Query. There is deliberately no
// BEGIN/COMMIT boilerplate here, unlike the erasure and messages fakes: users
// must never open a transaction nor set tenant context, so such a statement is
// as unscripted -- and as loud -- as a query nobody planned for.
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
	return pgReply{err: "unscripted query: " + n}
}

// newScriptedRepository starts the fake server on a loopback port and returns
// a Repository backed by a real pgxpool connected to it -- the pool passed
// directly, exactly as cmd/bot wires it, with no storage.DB in between. The
// pool is lazy: nothing connects until the first query, and the whole server
// is torn down with the test.
func newScriptedRepository(t *testing.T) (*Repository, *scriptedPG) {
	t.Helper()
	srv := &scriptedPG{}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	go srv.serve(ln)
	pool, err := pgxpool.New(context.Background(), fmt.Sprintf(
		"postgres://undelete_app@%s/undelete?sslmode=disable&default_query_exec_mode=simple_protocol",
		ln.Addr()))
	if err != nil {
		t.Fatalf("pgxpool.New: %v", err)
	}
	t.Cleanup(func() {
		pool.Close()
		ln.Close()
	})
	return NewRepository(pool), srv
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
			if !s.answer(conn, s.respond(sql)) {
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
// needs to consider the connection usable: AuthenticationOk, the two parameter
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

// answer writes one reply. The transaction status is always 'I': the
// repository must never open one, so the fake never claims otherwise.
func (s *scriptedPG) answer(conn net.Conn, reply pgReply) bool {
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
		if reply.errAfterRows != "" {
			if !writeError(conn, reply.errAfterRows) {
				return false
			}
		} else if !writeCommandComplete(conn, reply.tag) {
			return false
		}
	}
	return writeReadyForQuery(conn, 'I')
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

// normSQL folds a statement into the single form the prefixes above and the
// assertions below are written against: upper case, whitespace collapsed, and
// the space padding pgx wraps around every simple-protocol argument removed.
func normSQL(sql string) string {
	folded := strings.Join(strings.Fields(strings.ToUpper(sql)), " ")
	folded = strings.ReplaceAll(folded, "( ", "(")
	folded = strings.ReplaceAll(folded, " ,", ",")
	folded = strings.ReplaceAll(folded, " )", ")")
	return folded
}

// assertRanWithoutTenantContextOrTransaction walks the recorded stream and
// enforces the package's one structural rule, the mirror image of the
// erasure/messages one: users is the root identity table, so no statement may
// open a transaction or set app.current_owner_user_id -- the owner id is what
// this very table is queried to establish, and wrapping its reads in a tenant
// context would be circular. Everything runs in autocommit.
func assertRanWithoutTenantContextOrTransaction(t *testing.T, queries []string) {
	t.Helper()
	for i, q := range queries {
		switch {
		case q == "BEGIN" || q == "COMMIT" || q == "ROLLBACK":
			t.Fatalf("query %d: %q -- users must never open a transaction", i, q)
		case strings.HasPrefix(q, "SELECT SET_CONFIG("):
			t.Fatalf("query %d: %q -- users must never set a tenant context", i, q)
		}
	}
}

// TestUpsertByTelegramID pins the onboarding write on the wire: one single
// idempotent statement (a Business connection may be notified several times
// for the same account holder, and each notification must land the same way),
// in autocommit, without tenant context -- the row is what establishes the
// tenant, not the other way around.
func TestUpsertByTelegramID(t *testing.T) {
	t.Run("returns the existing-or-created row", func(t *testing.T) {
		repo, srv := newScriptedRepository(t)
		srv.on(upsertSQL, pgReply{
			columns: []pgColumn{
				{name: "id", oid: oidInt8},
				{name: "telegram_user_id", oid: oidInt8},
				{name: "retention_days", oid: oidInt4},
			},
			rows: [][]string{{"11", "700001", "7"}},
			tag:  "INSERT 0 1",
		})

		u, err := repo.UpsertByTelegramID(context.Background(), 700001)
		if err != nil {
			t.Fatalf("UpsertByTelegramID: %v", err)
		}
		if u == nil || u.ID != 11 || u.TelegramUserID != 700001 || u.RetentionDays != 7 {
			t.Fatalf("UpsertByTelegramID = %+v, want the row the database returned", u)
		}

		queries := srv.normalizedQueries()
		if len(queries) != 1 {
			t.Fatalf("queries = %v, want the upsert alone: no transaction wrapper, no tenant context", queries)
		}
		assertRanWithoutTenantContextOrTransaction(t, queries)
		for _, want := range []string{
			"INSERT INTO USERS (TELEGRAM_USER_ID)",
			"VALUES ('700001')",
			"ON CONFLICT (TELEGRAM_USER_ID) DO UPDATE", // idempotent: create OR return the existing row
			"RETURNING ID, TELEGRAM_USER_ID, RETENTION_DAYS",
		} {
			if !strings.Contains(queries[0], want) {
				t.Fatalf("the upsert %q does not carry %q", queries[0], want)
			}
		}
	})

	t.Run("a second notification re-runs the same idempotent statement", func(t *testing.T) {
		repo, srv := newScriptedRepository(t)
		reply := pgReply{
			columns: []pgColumn{
				{name: "id", oid: oidInt8},
				{name: "telegram_user_id", oid: oidInt8},
				{name: "retention_days", oid: oidInt4},
			},
			rows: [][]string{{"11", "700001", "7"}},
			tag:  "INSERT 0 1",
		}
		srv.on(upsertSQL, reply, reply)

		for i := 0; i < 2; i++ {
			if _, err := repo.UpsertByTelegramID(context.Background(), 700001); err != nil {
				t.Fatalf("UpsertByTelegramID call %d: %v", i+1, err)
			}
		}
		queries := srv.normalizedQueries()
		if len(queries) != 2 {
			t.Fatalf("queries = %v, want one statement per notification", queries)
		}
		if queries[0] != queries[1] {
			t.Fatalf("the two notifications ran different statements:\n%q\n%q", queries[0], queries[1])
		}
		assertRanWithoutTenantContextOrTransaction(t, queries)
	})

	t.Run("store failure surfaces", func(t *testing.T) {
		repo, srv := newScriptedRepository(t)
		srv.on(upsertSQL, pgReply{err: "disk full"})

		_, err := repo.UpsertByTelegramID(context.Background(), 700001)
		if err == nil {
			t.Fatal("UpsertByTelegramID returned no error although the store failed")
		}
		if !strings.Contains(err.Error(), "upsert user 700001") {
			t.Fatalf("error %q does not name its stage and tenant", err)
		}
	})
}

// TestGetRetentionDays pins the read half of /retention: one SELECT by the
// owner id the caller resolved from the Business connection -- never derived
// from anything else -- in autocommit and without tenant context. An unknown
// tenant is an error, not a period: the caller must answer nothing rather
// than lie with a default.
func TestGetRetentionDays(t *testing.T) {
	t.Run("returns the tenant's period", func(t *testing.T) {
		repo, srv := newScriptedRepository(t)
		srv.on(getRetentionSQL, pgReply{
			columns: []pgColumn{{name: "retention_days", oid: oidInt4}},
			rows:    [][]string{{"30"}},
			tag:     "SELECT 1",
		})

		days, err := repo.GetRetentionDays(context.Background(), 11)
		if err != nil {
			t.Fatalf("GetRetentionDays: %v", err)
		}
		if days != 30 {
			t.Fatalf("GetRetentionDays = %d, want the 30 the database returned", days)
		}

		queries := srv.normalizedQueries()
		if len(queries) != 1 {
			t.Fatalf("queries = %v, want the read alone", queries)
		}
		assertRanWithoutTenantContextOrTransaction(t, queries)
		if want := "SELECT RETENTION_DAYS FROM USERS WHERE ID = '11'"; queries[0] != want {
			t.Fatalf("the read is %q, want %q", queries[0], want)
		}
	})

	t.Run("an unknown tenant is an error", func(t *testing.T) {
		repo, srv := newScriptedRepository(t)
		srv.on(getRetentionSQL, pgReply{
			columns: []pgColumn{{name: "retention_days", oid: oidInt4}},
			tag:     "SELECT 0",
		})

		_, err := repo.GetRetentionDays(context.Background(), 11)
		if err == nil {
			t.Fatal("GetRetentionDays on an unknown tenant = nil, want an error")
		}
		if !strings.Contains(err.Error(), "reading retention of tenant 11") {
			t.Fatalf("error %q does not name its stage and tenant", err)
		}
	})

	t.Run("store failure surfaces", func(t *testing.T) {
		repo, srv := newScriptedRepository(t)
		srv.on(getRetentionSQL, pgReply{err: "connection reset"})

		_, err := repo.GetRetentionDays(context.Background(), 11)
		if err == nil {
			t.Fatal("GetRetentionDays returned no error although the store failed")
		}
		if !strings.Contains(err.Error(), "reading retention of tenant 11") {
			t.Fatalf("error %q does not name its stage and tenant", err)
		}
	})
}

// TestSetRetentionDays pins the write half of /retention: one UPDATE by owner
// id, the value already validated in Go so the CHECK constraint stays a
// backstop. The bounds are inclusive on the wire: MinRetentionDays and
// MaxRetentionDays pass the Go gate and must reach the database. A tenant
// that does not exist is an error naming it -- the command must not claim a
// success that changed nothing.
func TestSetRetentionDays(t *testing.T) {
	t.Run("updates the period", func(t *testing.T) {
		repo, srv := newScriptedRepository(t)
		srv.on(setRetentionSQL, pgReply{tag: "UPDATE 1"})

		if err := repo.SetRetentionDays(context.Background(), 11, 30); err != nil {
			t.Fatalf("SetRetentionDays: %v", err)
		}

		queries := srv.normalizedQueries()
		if len(queries) != 1 {
			t.Fatalf("queries = %v, want the update alone", queries)
		}
		assertRanWithoutTenantContextOrTransaction(t, queries)
		if want := "UPDATE USERS SET RETENTION_DAYS = '30' WHERE ID = '11'"; queries[0] != want {
			t.Fatalf("the update is %q, want %q", queries[0], want)
		}
	})

	t.Run("the bounds are inclusive on the wire", func(t *testing.T) {
		repo, srv := newScriptedRepository(t)
		srv.on(setRetentionSQL, pgReply{tag: "UPDATE 1"}, pgReply{tag: "UPDATE 1"})

		for _, days := range []int{MinRetentionDays, MaxRetentionDays} {
			if err := repo.SetRetentionDays(context.Background(), 11, days); err != nil {
				t.Fatalf("SetRetentionDays(ctx, 11, %d) = %v, want nil: the bound is inclusive", days, err)
			}
		}
		queries := srv.normalizedQueries()
		if len(queries) != 2 {
			t.Fatalf("queries = %v, want one update per bound", queries)
		}
		assertRanWithoutTenantContextOrTransaction(t, queries)
		if want := "UPDATE USERS SET RETENTION_DAYS = '365' WHERE ID = '11'"; queries[1] != want {
			t.Fatalf("the second update is %q, want %q", queries[1], want)
		}
	})

	t.Run("an unknown tenant is an error", func(t *testing.T) {
		repo, srv := newScriptedRepository(t)
		srv.on(setRetentionSQL, pgReply{tag: "UPDATE 0"})

		err := repo.SetRetentionDays(context.Background(), 11, 30)
		if err == nil {
			t.Fatal("SetRetentionDays on an unknown tenant = nil, want an error")
		}
		if !strings.Contains(err.Error(), "no such tenant") {
			t.Fatalf("error %q does not say there is no such tenant", err)
		}
	})

	t.Run("store failure surfaces", func(t *testing.T) {
		repo, srv := newScriptedRepository(t)
		srv.on(setRetentionSQL, pgReply{err: "lock timeout"})

		if err := repo.SetRetentionDays(context.Background(), 11, 30); err == nil {
			t.Fatal("SetRetentionDays returned no error although the store failed")
		} else if !strings.Contains(err.Error(), "setting retention of tenant 11") {
			t.Fatalf("error %q does not name its stage and tenant", err)
		}
	})
}

// TestListTenantsForRetention pins the feed of the retention purge: ONE
// unscoped SELECT over the whole table -- the per-tenant loop (and the RLS
// scoping it depends on) lives in messages.Repository.PurgeExpired, never in
// a global DELETE here. Every way the read can fail must surface: a broken
// result set or an unreadable row is an error, never a silently short list
// the purge would mistake for the complete tenant set.
func TestListTenantsForRetention(t *testing.T) {
	t.Run("lists every tenant in one statement", func(t *testing.T) {
		repo, srv := newScriptedRepository(t)
		srv.on(listTenantsSQL, pgReply{
			columns: []pgColumn{
				{name: "id", oid: oidInt8},
				{name: "retention_days", oid: oidInt4},
			},
			rows: [][]string{{"11", "7"}, {"12", "30"}},
			tag:  "SELECT 2",
		})

		tenants, err := repo.ListTenantsForRetention(context.Background())
		if err != nil {
			t.Fatalf("ListTenantsForRetention: %v", err)
		}
		want := []TenantRetention{{OwnerUserID: 11, RetentionDays: 7}, {OwnerUserID: 12, RetentionDays: 30}}
		if len(tenants) != len(want) {
			t.Fatalf("ListTenantsForRetention = %v, want %v", tenants, want)
		}
		for i := range want {
			if tenants[i] != want[i] {
				t.Fatalf("tenants[%d] = %+v, want %+v", i, tenants[i], want[i])
			}
		}

		queries := srv.normalizedQueries()
		if len(queries) != 1 {
			t.Fatalf("queries = %v, want the listing alone", queries)
		}
		assertRanWithoutTenantContextOrTransaction(t, queries)
		if queries[0] != listTenantsSQL {
			t.Fatalf("the listing is %q, want %q (every tenant, no WHERE)", queries[0], listTenantsSQL)
		}
	})

	t.Run("an empty table lists no tenant", func(t *testing.T) {
		repo, srv := newScriptedRepository(t)
		srv.on(listTenantsSQL, pgReply{
			columns: []pgColumn{
				{name: "id", oid: oidInt8},
				{name: "retention_days", oid: oidInt4},
			},
			tag: "SELECT 0",
		})

		tenants, err := repo.ListTenantsForRetention(context.Background())
		if err != nil {
			t.Fatalf("ListTenantsForRetention on an empty table: %v", err)
		}
		if len(tenants) != 0 {
			t.Fatalf("ListTenantsForRetention = %v, want no tenant", tenants)
		}
	})

	t.Run("a row that does not scan fails loudly", func(t *testing.T) {
		repo, srv := newScriptedRepository(t)
		srv.on(listTenantsSQL, pgReply{
			columns: []pgColumn{
				{name: "id", oid: oidInt8},
				{name: "retention_days", oid: oidInt4},
			},
			rows: [][]string{{"not-a-number", "7"}},
			tag:  "SELECT 1",
		})

		_, err := repo.ListTenantsForRetention(context.Background())
		if err == nil {
			t.Fatal("ListTenantsForRetention returned no error although a row did not scan")
		}
		if !strings.Contains(err.Error(), "reading tenant") {
			t.Fatalf("error %q does not name its stage", err)
		}
	})

	t.Run("a broken result set surfaces its error", func(t *testing.T) {
		repo, srv := newScriptedRepository(t)
		srv.on(listTenantsSQL, pgReply{
			columns: []pgColumn{
				{name: "id", oid: oidInt8},
				{name: "retention_days", oid: oidInt4},
			},
			rows:         [][]string{{"11", "7"}},
			errAfterRows: "server died mid-result",
		})

		tenants, err := repo.ListTenantsForRetention(context.Background())
		if err == nil {
			t.Fatalf("ListTenantsForRetention = %v with no error, want the broken result set to surface", tenants)
		}
	})

	t.Run("store failure surfaces", func(t *testing.T) {
		repo, srv := newScriptedRepository(t)
		srv.on(listTenantsSQL, pgReply{err: "disk full"})

		_, err := repo.ListTenantsForRetention(context.Background())
		if err == nil {
			t.Fatal("ListTenantsForRetention returned no error although the store failed")
		}
		if !strings.Contains(err.Error(), "listing tenants") {
			t.Fatalf("error %q does not name its stage", err)
		}
	})
}
