package erasure

import (
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/LouisMoretti/Undelete/bot/internal/storage"
)

// This file covers repository.go, the SQL half of the package, WITHOUT a
// database: a scripted fake speaks enough of the PostgreSQL wire protocol for
// the real pgxpool to connect, and storage.DB.InTenant runs its real
// BEGIN / set_config / COMMIT sequence against it. What the fake then observes
// is exactly what the database would see: the statements, their order, and
// their inlined parameters. The semantic half (RLS, row serialisation, the
// server clock) stays the integration tests' -- what only a fake can pin is
// that every statement travels inside an InTenant transaction, that Claim is
// one atomic UPDATE carrying its own expiry predicate, and that each server
// answer maps to the ClaimState the service branches on.
//
// The DSN forces default_query_exec_mode=simple_protocol, so every query
// arrives as one 'Q' message with its arguments inlined -- which is also what
// makes the parameters assertable. The fake answers anything unscripted with
// an error, so a query the test did not plan for fails loudly instead of
// silently succeeding.

// PostgreSQL type OIDs the scripted RowDescriptions need.
const (
	oidInt8        = 20
	oidText        = 25
	oidTimestamptz = 1184
)

// The normalised prefixes the repository's statements are recognised by. The
// two UPDATEs are distinguished by their SET clauses. In simple protocol every
// argument arrives quoted and space-padded (pgx's comment-injection
// mitigation), which normSQL folds away.
const (
	insertRequestSQL = "INSERT INTO DATA_ERASURE_REQUESTS"
	spendSQL         = "UPDATE DATA_ERASURE_REQUESTS SET STATUS = 'CONSUMED'"
	classifySQL      = "SELECT STATUS FROM DATA_ERASURE_REQUESTS"
	completeSQL      = "UPDATE DATA_ERASURE_REQUESTS SET STATUS = 'COMPLETED'"
	deleteOthersSQL  = "DELETE FROM DATA_ERASURE_REQUESTS WHERE OWNER_USER_ID = '11' AND CODE_SHA256 <>"
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
// replies for one statement prefix: a second Confirm replaying a code gets a
// different answer for the same UPDATE than the first one did.
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

// respond answers one simple-protocol Query. BEGIN/COMMIT/ROLLBACK and the
// InTenant set_config are boilerplate every statement depends on; everything
// else must have been scripted, in the order it is expected.
func (s *scriptedPG) respond(sql string) pgReply {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := normSQL(sql)
	switch n {
	case "BEGIN", "COMMIT", "ROLLBACK":
		return pgReply{tag: n}
	}
	if strings.HasPrefix(n, "SELECT SET_CONFIG(") {
		return pgReply{tag: "SELECT 1"}
	}
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
// a Repository backed by a real pgxpool connected to it, through the real
// storage.DB.InTenant. The pool is lazy: nothing connects until the first
// query, and the whole server is torn down with the test.
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
	return NewRepository(&storage.DB{Pool: pool}), srv
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

// firstQueryWithPrefix returns the first recorded statement matching prefix.
func firstQueryWithPrefix(t *testing.T, queries []string, prefix string) string {
	t.Helper()
	for _, q := range queries {
		if strings.HasPrefix(q, prefix) {
			return q
		}
	}
	t.Fatalf("no recorded query starts with %q: %v", prefix, queries)
	return ""
}

// countQueriesWithPrefix counts the recorded statements matching prefix.
func countQueriesWithPrefix(queries []string, prefix string) int {
	n := 0
	for _, q := range queries {
		if strings.HasPrefix(q, prefix) {
			n++
		}
	}
	return n
}

// assertEveryQueryRanInsideInTenant walks the recorded stream and enforces the
// package's one structural rule: every statement reaches the database inside a
// transaction that opened with app.current_owner_user_id set to THIS owner,
// LOCAL to that transaction. A query sneaking past InTenant would run with no
// tenant context, which RLS answers with zero rows -- fail-closed, but
// silently wrong.
func assertEveryQueryRanInsideInTenant(t *testing.T, queries []string, ownerUserID int64) {
	t.Helper()
	wantContext := "SELECT SET_CONFIG('APP.CURRENT_OWNER_USER_ID', '" +
		strconv.FormatInt(ownerUserID, 10) + "', TRUE)"
	inTx, haveContext := false, false
	for i, q := range queries {
		switch {
		case q == "BEGIN":
			if inTx {
				t.Fatalf("query %d: BEGIN inside a transaction", i)
			}
			inTx, haveContext = true, false
		case q == "COMMIT" || q == "ROLLBACK":
			inTx = false
		case strings.HasPrefix(q, "SELECT SET_CONFIG("):
			if !inTx {
				t.Fatalf("query %d: tenant context set outside a transaction: %q", i, q)
			}
			if q != wantContext {
				t.Fatalf("query %d: tenant context is %q, want %q (this owner, LOCAL)", i, q, wantContext)
			}
			haveContext = true
		default:
			if !inTx || !haveContext {
				t.Fatalf("query %d ran outside an InTenant transaction: %q", i, q)
			}
		}
	}
	if inTx {
		t.Fatalf("the recorded stream ends inside an open transaction: %v", queries)
	}
}

// TestRepositoryIssueReplacesThePendingCodeInOneStatement pins the Issue
// contract on the wire: ONE upsert inside the tenant transaction, arbitrated
// by the one-pending-per-owner index (migration 0008), so two racing Requests
// can never leave two live codes. Only the 'pending' row is matched (a
// consumed row may still need resuming, a completed one is the replay
// receipt), every identifying column is overwritten with the new request's,
// and the expiry is computed by the SERVER's clock (now() + make_interval),
// never by the bot's.
func TestRepositoryIssueReplacesThePendingCodeInOneStatement(t *testing.T) {
	repo, srv := newScriptedRepository(t)
	srv.on(insertRequestSQL, pgReply{
		columns: []pgColumn{{name: "expires_at", oid: oidTimestamptz}},
		rows:    [][]string{{"2026-09-15 12:34:56.789567+00"}},
		tag:     "INSERT 0 1",
	})

	expiresAt, err := repo.Issue(context.Background(), testTenant, "cafebabe", ChallengeTTL)
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	want := time.Date(2026, time.September, 15, 12, 34, 56, 789567000, time.UTC)
	if !expiresAt.Equal(want) {
		t.Fatalf("expiresAt = %v, want the timestamp the database returned: %v", expiresAt, want)
	}

	queries := srv.normalizedQueries()
	if len(queries) != 4 || queries[0] != "BEGIN" || queries[3] != "COMMIT" {
		t.Fatalf("queries = %v, want begin, context, upsert, commit", queries)
	}
	assertEveryQueryRanInsideInTenant(t, queries, testTenant.OwnerUserID)

	upsert := firstQueryWithPrefix(t, queries, insertRequestSQL)
	for _, want := range []string{
		"'CAFEBABE'",                   // the hash, never the code
		"'700001'",                     // the owner's Telegram id
		"'BC-1'",                       // the traceability connection id
		"RETURNING EXPIRES_AT",         // the deadline is read back, not assumed
		"MAKE_INTERVAL(SECS => '600')", // the TTL travels to the server clock, ChallengeTTL
		"ON CONFLICT (OWNER_USER_ID) WHERE STATUS = 'PENDING'",
		"CODE_SHA256 = EXCLUDED.CODE_SHA256",
		"EXPIRES_AT = EXCLUDED.EXPIRES_AT",
		"OWNER_TELEGRAM_USER_ID = EXCLUDED.OWNER_TELEGRAM_USER_ID",
		"BUSINESS_CONNECTION_ID = EXCLUDED.BUSINESS_CONNECTION_ID",
	} {
		if !strings.Contains(upsert, want) {
			t.Fatalf("the upsert %q does not carry %q", upsert, want)
		}
	}
}

// TestRepositoryClaimClassifiesEveryServerAnswer drives the real Claim against
// each answer the database can give. The single-use and expiry decisions are
// the UPDATE's own predicates (status = 'pending' AND expires_at > now()),
// decided atomically by the server; the classification SELECT only runs when
// nothing was spendable, which is what keeps "expired", "already running" and
// "never existed" three different answers to the owner.
func TestRepositoryClaimClassifiesEveryServerAnswer(t *testing.T) {
	for _, tt := range []struct {
		name          string
		updateMatched bool
		// statusRow is the status the classification SELECT reports; empty
		// means the row does not exist at all.
		statusRow string
		want      ClaimState
	}{
		{name: "granted", updateMatched: true, want: ClaimGranted},
		{name: "expired", statusRow: "pending", want: ClaimExpired},
		{name: "resumable", statusRow: "consumed", want: ClaimResumable},
		{name: "completed", statusRow: "completed", want: ClaimCompleted},
		{name: "no row at all", want: ClaimUnknown},
		{name: "a status the switch does not know", statusRow: "garbage", want: ClaimUnknown},
	} {
		t.Run(tt.name, func(t *testing.T) {
			repo, srv := newScriptedRepository(t)
			update := pgReply{tag: "UPDATE 0"}
			if tt.updateMatched {
				update = pgReply{
					columns: []pgColumn{{name: "id", oid: oidInt8}},
					rows:    [][]string{{"771"}},
					tag:     "UPDATE 1",
				}
			}
			srv.on(spendSQL, update)
			// A classification with no row (the code never existed for this
			// tenant) must come back as ErrNoRows, and a status the switch
			// does not name must not be guessed at either.
			selectReply := pgReply{
				columns: []pgColumn{{name: "status", oid: oidText}},
				tag:     "SELECT 1",
			}
			if tt.statusRow != "" {
				selectReply.rows = [][]string{{tt.statusRow}}
			}
			srv.on(classifySQL, selectReply)

			state, err := repo.Claim(context.Background(), testTenant.OwnerUserID, "cafebabe")
			if err != nil {
				t.Fatalf("Claim: %v", err)
			}
			if state != tt.want {
				t.Fatalf("Claim state = %v, want %v", state, tt.want)
			}

			queries := srv.normalizedQueries()
			assertEveryQueryRanInsideInTenant(t, queries, testTenant.OwnerUserID)
			spending := firstQueryWithPrefix(t, queries, spendSQL)
			for _, predicate := range []string{
				"STATUS = 'PENDING'",
				"EXPIRES_AT > NOW()",
				"RETURNING ID",
			} {
				if !strings.Contains(spending, predicate) {
					t.Fatalf("the spending UPDATE %q does not carry its %q predicate", spending, predicate)
				}
			}
			// The row is re-read only when the UPDATE matched nothing: a
			// granted claim is classified by the UPDATE alone.
			classifications := countQueriesWithPrefix(queries, classifySQL)
			if tt.updateMatched && classifications != 0 {
				t.Fatalf("a granted claim re-read the row %d times", classifications)
			}
			if !tt.updateMatched && classifications != 1 {
				t.Fatalf("an unspendable claim classified %d times, want 1", classifications)
			}
		})
	}
}

// TestRepositoryIssueFailuresSurface: a database that cannot record the
// challenge must produce an error naming the stage, never a silent success --
// an owner handed a code the store never wrote would hold an unspendable
// authorisation.
func TestRepositoryIssueFailuresSurface(t *testing.T) {
	t.Run("the upsert fails", func(t *testing.T) {
		repo, srv := newScriptedRepository(t)
		srv.on(insertRequestSQL, pgReply{err: "disk full"})

		_, err := repo.Issue(context.Background(), testTenant, "cafebabe", ChallengeTTL)
		if err == nil {
			t.Fatal("Issue returned no error although the upsert failed")
		}
		if !strings.Contains(err.Error(), "issuing erasure request") {
			t.Fatalf("error %q does not name its stage", err)
		}
	})

	t.Run("the insert returns no row", func(t *testing.T) {
		repo, srv := newScriptedRepository(t)
		srv.on(insertRequestSQL, pgReply{tag: "INSERT 0 1"})

		_, err := repo.Issue(context.Background(), testTenant, "cafebabe", ChallengeTTL)
		if err == nil {
			t.Fatal("Issue returned no error although no row came back")
		}
		if !strings.Contains(err.Error(), "issuing erasure request") {
			t.Fatalf("error %q does not name its stage", err)
		}
	})
}

// TestRepositoryClaimFailuresSurface: both statements of the claim can fail,
// and each failure must surface as an error rather than as a state -- a
// ClaimUnknown returned for a broken connection would read to the owner as
// "no such code" while the row may be pending and spendable.
func TestRepositoryClaimFailuresSurface(t *testing.T) {
	t.Run("the spending UPDATE fails", func(t *testing.T) {
		repo, srv := newScriptedRepository(t)
		srv.on(spendSQL, pgReply{err: "lock timeout"})

		_, err := repo.Claim(context.Background(), testTenant.OwnerUserID, "cafebabe")
		if err == nil {
			t.Fatal("Claim returned no error although the UPDATE failed")
		}
		if !strings.Contains(err.Error(), "claiming erasure request") {
			t.Fatalf("error %q does not name its stage", err)
		}
	})

	t.Run("the classification read fails", func(t *testing.T) {
		repo, srv := newScriptedRepository(t)
		srv.on(spendSQL, pgReply{tag: "UPDATE 0"})
		srv.on(classifySQL, pgReply{err: "connection reset"})

		_, err := repo.Claim(context.Background(), testTenant.OwnerUserID, "cafebabe")
		if err == nil {
			t.Fatal("Claim returned no error although the read failed")
		}
		if !strings.Contains(err.Error(), "reading erasure request") {
			t.Fatalf("error %q does not name its stage", err)
		}
	})
}

// TestRepositoryCompleteMarksOnlyConsumedRowsAndScrubsIdentifiers pins the
// completion on the wire: it can only ever move a CONSUMED row (never
// resurrect one the erasure deleted), and the same statement scrubs the
// Telegram and connection identifiers -- the receipt the erasure keeps holds
// the tenant key, the hash and the timestamps, and no identifier a replay
// could do without. A row already gone completes silently: replaying a
// completion must not be an error.
func TestRepositoryCompleteMarksOnlyConsumedRowsAndScrubsIdentifiers(t *testing.T) {
	t.Run("the statement restricts and scrubs", func(t *testing.T) {
		repo, srv := newScriptedRepository(t)
		srv.on(completeSQL, pgReply{tag: "UPDATE 1"})

		if err := repo.Complete(context.Background(), testTenant.OwnerUserID, "cafebabe"); err != nil {
			t.Fatalf("Complete: %v", err)
		}
		queries := srv.normalizedQueries()
		assertEveryQueryRanInsideInTenant(t, queries, testTenant.OwnerUserID)
		complete := firstQueryWithPrefix(t, queries, completeSQL)
		for _, want := range []string{
			"STATUS = 'COMPLETED'",
			"OWNER_TELEGRAM_USER_ID = NULL",
			"BUSINESS_CONNECTION_ID = NULL",
			"STATUS = 'CONSUMED'", // the WHERE: only a consumed row can complete
			"CAFEBABE",            // this row, not any other
		} {
			if !strings.Contains(complete, want) {
				t.Fatalf("the completion %q does not carry %q", complete, want)
			}
		}
	})

	t.Run("a row already gone completes silently", func(t *testing.T) {
		repo, srv := newScriptedRepository(t)
		srv.on(completeSQL, pgReply{tag: "UPDATE 0"})

		if err := repo.Complete(context.Background(), testTenant.OwnerUserID, "cafebabe"); err != nil {
			t.Fatalf("Complete on an absent row = %v, want nil: a replayed completion is not an error", err)
		}
	})

	t.Run("the store failure surfaces", func(t *testing.T) {
		repo, srv := newScriptedRepository(t)
		srv.on(completeSQL, pgReply{err: "disk full"})

		if err := repo.Complete(context.Background(), testTenant.OwnerUserID, "cafebabe"); err == nil {
			t.Fatal("Complete returned no error although the store failed")
		} else if !strings.Contains(err.Error(), "completing erasure request") {
			t.Fatalf("error %q does not name its stage", err)
		}
	})
}

// TestRepositoryDeleteOthersCountsRowsAndKeepsTheReceipt: the last deletion
// step removes every request of the tenant EXCEPT the hash being spent (the
// replay receipt), and returns how many went.
func TestRepositoryDeleteOthersCountsRowsAndKeepsTheReceipt(t *testing.T) {
	t.Run("the count comes back", func(t *testing.T) {
		repo, srv := newScriptedRepository(t)
		srv.on(deleteOthersSQL, pgReply{tag: "DELETE 3"})

		deleted, err := repo.DeleteOthers(context.Background(), testTenant.OwnerUserID, "cafebabe")
		if err != nil {
			t.Fatalf("DeleteOthers: %v", err)
		}
		if deleted != 3 {
			t.Fatalf("DeleteOthers = %d, want the 3 rows the database reports", deleted)
		}
		queries := srv.normalizedQueries()
		assertEveryQueryRanInsideInTenant(t, queries, testTenant.OwnerUserID)
		del := firstQueryWithPrefix(t, queries, deleteOthersSQL)
		if !strings.Contains(del, "CODE_SHA256 <> 'CAFEBABE'") {
			t.Fatalf("the deletion %q does not keep the receipt hash", del)
		}
	})

	t.Run("the store failure surfaces", func(t *testing.T) {
		repo, srv := newScriptedRepository(t)
		srv.on(deleteOthersSQL, pgReply{err: "disk full"})

		if _, err := repo.DeleteOthers(context.Background(), testTenant.OwnerUserID, "cafebabe"); err == nil {
			t.Fatal("DeleteOthers returned no error although the store failed")
		} else if !strings.Contains(err.Error(), "deleting erasure requests of tenant 11") {
			t.Fatalf("error %q does not name its stage and tenant", err)
		}
	})
}

// TestServiceRunsTheRealRepositoryThroughItsStates wires the real Repository
// behind the real Service -- the seam production uses -- and replays a code
// end to end against scripted database answers: first Confirm erases, the
// replay classifies the completed row and erases nothing. The whole recorded
// stream must satisfy the InTenant rule, which is the dynamic counterpart of
// the static audit in bot/internal/storage.
func TestServiceRunsTheRealRepositoryThroughItsStates(t *testing.T) {
	repo, srv := newScriptedRepository(t)
	steps := &fakeSteps{}
	service := newService(t, repo, steps)

	srv.on(insertRequestSQL, pgReply{
		columns: []pgColumn{{name: "expires_at", oid: oidTimestamptz}},
		rows:    [][]string{{"2026-09-15 12:34:56.789567+00"}},
		tag:     "INSERT 0 1",
	})
	srv.on(spendSQL,
		pgReply{ // first Confirm: the code is spendable
			columns: []pgColumn{{name: "id", oid: oidInt8}},
			rows:    [][]string{{"771"}},
			tag:     "UPDATE 1",
		},
		pgReply{tag: "UPDATE 0"}, // replay: nothing left to spend
	)
	srv.on(classifySQL, pgReply{
		columns: []pgColumn{{name: "status", oid: oidText}},
		rows:    [][]string{{"completed"}},
		tag:     "SELECT 1",
	})
	srv.on(deleteOthersSQL, pgReply{tag: "DELETE 2"})
	srv.on(completeSQL, pgReply{tag: "UPDATE 1"})

	challenge, err := service.Request(context.Background(), testTenant)
	if err != nil {
		t.Fatalf("Request: %v", err)
	}
	outcome, err := service.Confirm(context.Background(), testTenant, challenge.Code)
	if err != nil {
		t.Fatalf("Confirm: %v", err)
	}
	if outcome != OutcomeErased {
		t.Fatalf("outcome = %v, want OutcomeErased", outcome)
	}
	if got := strings.Join(steps.calls, ","); got != "connections,outbox,media,messages" {
		t.Fatalf("steps ran as %q", got)
	}

	outcome, err = service.Confirm(context.Background(), testTenant, challenge.Code)
	if err != nil {
		t.Fatalf("replayed Confirm: %v", err)
	}
	if outcome != OutcomeAlreadyErased {
		t.Fatalf("replayed outcome = %v, want OutcomeAlreadyErased", outcome)
	}
	if len(steps.calls) != 4 {
		t.Fatalf("the replay ran %d extra steps, want 0: %v", len(steps.calls)-4, steps.calls)
	}

	assertEveryQueryRanInsideInTenant(t, srv.normalizedQueries(), testTenant.OwnerUserID)
}
