package outbox

import (
	"context"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/LouisMoretti/Undelete/bot/internal/storage"
	"github.com/LouisMoretti/Undelete/bot/internal/users"
)

// This file covers repository.go, the SQL half of the package, WITHOUT a
// database: a scripted fake speaks enough of the PostgreSQL wire protocol for
// the real pgxpool to connect, and storage.DB.InTenant runs its real
// BEGIN / set_config / COMMIT sequence against it. What the fake then observes
// is exactly what the database would see: the statements, their order, and
// their inlined parameters -- the lease seconds, the retry delay, the 6h
// resweep delay, the error classes. The semantic half (RLS actually filtering,
// SKIP LOCKED actually excluding, the server clock) stays the integration
// tests'. What only a fake can pin is that every statement travels inside an
// InTenant transaction, that Claim writes the lease it will return, and that
// each server failure maps to the stage-named error the worker branches on.
//
// The DSN forces default_query_exec_mode=simple_protocol, so every query
// arrives as one 'Q' message with its arguments inlined -- which is also what
// makes the parameters assertable. The fake answers anything unscripted with
// an error, so a query the test did not plan for fails loudly instead of
// silently succeeding.

// PostgreSQL type OIDs the scripted RowDescriptions need.
const (
	oidInt4  = 23
	oidInt8  = 20
	oidText  = 25
	oidJSONB = 3802
)

// The normalised prefixes the repository's statements are recognised by. In
// simple protocol every argument arrives quoted (integers included: pgx encodes
// every parameter to text, then quotes it), nil as the bare NULL -- which
// normSQL folds into the single form these prefixes and the assertions below
// are written against.
const (
	outboxInsertSQL = "INSERT INTO NOTIFICATION_OUTBOX"
	claimSQL        = "WITH CANDIDATE AS (SELECT"
	markSentSQL     = "UPDATE NOTIFICATION_OUTBOX SET STATUS = 'SENT'"
	markRetrySQL    = "UPDATE NOTIFICATION_OUTBOX SET STATUS = 'PENDING'"
	markFailedSQL   = "UPDATE NOTIFICATION_OUTBOX SET STATUS = 'FAILED'"

	// CountBacklog's per-tenant count, keyed on the tenant the tests use.
	backlogTenant11SQL = "SELECT COUNT(*) FROM NOTIFICATION_OUTBOX WHERE OWNER_USER_ID = '11'"
	backlogTenant12SQL = "SELECT COUNT(*) FROM NOTIFICATION_OUTBOX WHERE OWNER_USER_ID = '12'"

	// DeleteTenant's unconditional DELETE, distinguished from PurgeExpired's
	// by its lack of a status predicate.
	deleteTenant11SQL = "DELETE FROM NOTIFICATION_OUTBOX WHERE OWNER_USER_ID = '11'"

	// PurgeExpired's per-tenant DELETE, with its retention predicate.
	purgeTenant11SQL = "DELETE FROM NOTIFICATION_OUTBOX WHERE OWNER_USER_ID = '11' AND STATUS IN"
	purgeTenant12SQL = "DELETE FROM NOTIFICATION_OUTBOX WHERE OWNER_USER_ID = '12' AND STATUS IN"
)

// tenantContextSQL is the normalised InTenant set_config of one owner: the
// context every statement of that tenant's transaction must have seen.
func tenantContextSQL(ownerUserID int64) string {
	return "SELECT SET_CONFIG('APP.CURRENT_OWNER_USER_ID', '" +
		strconv.FormatInt(ownerUserID, 10) + "', TRUE)"
}

type pgColumn struct {
	name string
	oid  uint32
}

// pgReply is one scripted server answer: an optional RowDescription with its
// DataRows, the CommandComplete tag, or an ErrorResponse when err is set. A
// nil entry in a row is a SQL NULL.
type pgReply struct {
	columns []pgColumn
	rows    [][]*string
	tag     string
	err     string
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

// newScriptedOutbox starts the fake server on a loopback port and returns a
// Repository backed by a real pgxpool connected to it, through the real
// storage.DB.InTenant, plus the DB itself: the insert entry points take the
// caller's transaction, exactly the way messages.MarkDeleted hands them its
// own. The pool is lazy: nothing connects until the first query, and the whole
// server is torn down with the test.
func newScriptedOutbox(t *testing.T) (*Repository, *storage.DB, *scriptedPG) {
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
	db := &storage.DB{Pool: pool}
	return NewRepository(db), db, srv
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

// writeDataRow writes one row; a nil value is a SQL NULL (length -1), which is
// how media_payload comes back for a text chunk.
func writeDataRow(conn net.Conn, values []*string) bool {
	var p []byte
	p = binary.BigEndian.AppendUint16(p, uint16(len(values)))
	for _, v := range values {
		if v == nil {
			p = binary.BigEndian.AppendUint32(p, 0xffffffff)
			continue
		}
		p = binary.BigEndian.AppendUint32(p, uint32(len(*v)))
		p = append(p, *v...)
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
// assertions below are written against: upper case, whitespace collapsed (the
// simple-protocol sanitizer pads every inlined argument with spaces), and the
// stray spaces before opening parens and closing punctuation removed.
func normSQL(sql string) string {
	folded := strings.Join(strings.Fields(strings.ToUpper(sql)), " ")
	folded = strings.ReplaceAll(folded, "( ", "(")
	folded = strings.ReplaceAll(folded, " ,", ",")
	folded = strings.ReplaceAll(folded, " )", ")")
	return folded
}

// pgRow builds a DataRow of non-NULL text values.
func pgRow(values ...string) []*string {
	row := make([]*string, len(values))
	for i, v := range values {
		row[i] = &v
	}
	return row
}

func pgStr(v string) *string { return &v }

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

func countQueriesWithPrefix(queries []string, prefix string) int {
	n := 0
	for _, q := range queries {
		if strings.HasPrefix(q, prefix) {
			n++
		}
	}
	return n
}

func containsExact(queries []string, want string) bool {
	for _, q := range queries {
		if q == want {
			return true
		}
	}
	return false
}

// assertEveryQueryRanInsideInTenant walks the recorded stream and enforces the
// package's one structural rule: every statement reaches the database inside a
// transaction that opened with app.current_owner_user_id set to THIS owner,
// LOCAL to that transaction. A query sneaking past InTenant would run with no
// tenant context, which RLS answers with zero rows -- fail-closed, but
// silently wrong (cf. the CountBacklog trap in repository.go).
func assertEveryQueryRanInsideInTenant(t *testing.T, queries []string, ownerUserID int64) {
	t.Helper()
	wantContext := tenantContextSQL(ownerUserID)
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

// claimReturningColumns describes the RETURNING row of Claim's UPDATE: twelve
// columns in the scan order repository.Claim expects, media_payload nullable.
func claimReturningColumns() []pgColumn {
	return []pgColumn{
		{name: "id", oid: oidInt8},
		{name: "owner_user_id", oid: oidInt8},
		{name: "owner_telegram_user_id", oid: oidInt8},
		{name: "business_connection_id", oid: oidText},
		{name: "chat_id", oid: oidInt8},
		{name: "message_id", oid: oidInt8},
		{name: "event_type", oid: oidText},
		{name: "payload_text", oid: oidText},
		{name: "attempts", oid: oidInt4},
		{name: "lease_token", oid: oidText},
		{name: "payload_kind", oid: oidText},
		{name: "media_payload", oid: oidJSONB},
	}
}

// claimRow builds the RETURNING DataRow of a claimed job; a nil media is a
// SQL NULL, which is how a text chunk comes back.
func claimRow(id, owner, ownerTelegram, bcID, chatID, msgID, eventType, text, attempts, token, kind string, media *string) []*string {
	return []*string{pgStr(id), pgStr(owner), pgStr(ownerTelegram), pgStr(bcID),
		pgStr(chatID), pgStr(msgID), pgStr(eventType), pgStr(text),
		pgStr(attempts), pgStr(token), pgStr(kind), media}
}

// TestInsertTxWritesTheTextChunkInTheCallersTransaction pins the write half of
// the atomicity contract: InsertTx has no transaction of its own, it joins the
// caller's -- here InTenant's, exactly the way messages.MarkDeleted hands it
// the transaction that sets deleted_at. A text chunk is (text, NULL): the
// payload kind and the absent media payload are inlined for the database to
// check, and the idempotency key must be the full natural key.
func TestInsertTxWritesTheTextChunkInTheCallersTransaction(t *testing.T) {
	_, db, srv := newScriptedOutbox(t)
	srv.on(outboxInsertSQL, pgReply{tag: "INSERT 0 1"})

	err := db.InTenant(context.Background(), 11, func(tx pgx.Tx) error {
		return InsertTx(context.Background(), tx, 11, 700001, "BC-1", 33, 501, EventDeletedMessage, 0, "alert text")
	})
	if err != nil {
		t.Fatalf("InsertTx: %v", err)
	}

	queries := srv.normalizedQueries()
	if len(queries) != 4 || queries[0] != "BEGIN" || queries[3] != "COMMIT" {
		t.Fatalf("queries = %v, want begin, context, insert, commit", queries)
	}
	assertEveryQueryRanInsideInTenant(t, queries, 11)
	insert := firstQueryWithPrefix(t, queries, outboxInsertSQL)
	for _, want := range []string{
		"VALUES ('11', '700001', 'BC-1', '33', '501', 'DELETED_MESSAGE', '0', 'ALERT TEXT', 'TEXT', NULL)",
		"ON CONFLICT (OWNER_USER_ID, BUSINESS_CONNECTION_ID, CHAT_ID, MESSAGE_ID, EVENT_TYPE, CHUNK_INDEX) DO NOTHING",
	} {
		if !strings.Contains(insert, want) {
			t.Fatalf("the insert %q does not carry %q", insert, want)
		}
	}
}

// TestInsertMediaTxFreezesThePayloadWithItsFallbackText pins the media entry:
// the fallback text travels in payload_text, the kind is 'media', and the
// payload is frozen as the JSON encodeMediaPayload produces -- bytea-hex on
// the wire, decodable back to the very same struct.
func TestInsertMediaTxFreezesThePayloadWithItsFallbackText(t *testing.T) {
	_, db, srv := newScriptedOutbox(t)
	srv.on(outboxInsertSQL, pgReply{tag: "INSERT 0 1"})

	payload := MediaPayload{
		MediaGroupID: "album-1",
		Items: []MediaItem{{
			MediaFileID: 9001, MessageID: 101, FileIndex: 0,
			MediaType: "photo", RelativePath: "11/a/1.jpg",
			FileName: "1.jpg", Caption: "first",
		}},
	}
	err := db.InTenant(context.Background(), 11, func(tx pgx.Tx) error {
		return InsertMediaTx(context.Background(), tx, 11, 700001, "BC-1", 33, 501, EventDeletedMessage, 2, "fallback text", payload)
	})
	if err != nil {
		t.Fatalf("InsertMediaTx: %v", err)
	}

	insert := firstQueryWithPrefix(t, srv.normalizedQueries(), outboxInsertSQL)
	if !strings.Contains(insert, "VALUES ('11', '700001', 'BC-1', '33', '501', 'DELETED_MESSAGE', '2', 'FALLBACK TEXT', 'MEDIA', ") {
		t.Fatalf("the media insert does not carry its fallback text and kind: %q", insert)
	}
	raw, err := encodeMediaPayload(payload)
	if err != nil {
		t.Fatal(err)
	}
	// normSQL upper-cases the whole statement, the bytea literal included.
	wantPayload := `'\X` + strings.ToUpper(hex.EncodeToString(raw)) + `'`
	if !strings.Contains(insert, wantPayload) {
		t.Fatalf("the media insert does not carry the frozen payload: %q", insert)
	}
}

// TestInsertFailuresRollBackTheTransaction: an outbox write that fails inside
// the caller's transaction must surface naming its stage, and nothing may
// commit -- deleted_at and its alerts are written together or not at all.
func TestInsertFailuresRollBackTheTransaction(t *testing.T) {
	_, db, srv := newScriptedOutbox(t)
	srv.on(outboxInsertSQL, pgReply{err: "outbox down"})

	err := db.InTenant(context.Background(), 11, func(tx pgx.Tx) error {
		return InsertTx(context.Background(), tx, 11, 700001, "BC-1", 33, 501, EventDeletedMessage, 0, "alert text")
	})
	if err == nil {
		t.Fatal("InsertTx returned no error although the insert failed")
	}
	if !strings.Contains(err.Error(), "outbox insert") {
		t.Fatalf("error %q does not name its stage", err)
	}
	if !containsExact(srv.normalizedQueries(), "ROLLBACK") {
		t.Fatalf("the failed insert committed: %v", srv.normalizedQueries())
	}
}

// TestClaimReservesOneJobWithAFreshLeaseToken pins the reservation on the
// wire: the eligibility window (pending/processing/failed past their
// deadlines), the head-of-line rule (a prior chunk may only be sent or failed),
// the FOR UPDATE SKIP LOCKED that lets workers share the queue, the fresh
// lease token written with the lease duration, the attempt budget reset for a
// reswept failure, and the RETURNING row handed back as the Job.
func TestClaimReservesOneJobWithAFreshLeaseToken(t *testing.T) {
	repo, _, srv := newScriptedOutbox(t)
	srv.on(claimSQL, pgReply{
		columns: claimReturningColumns(),
		rows: [][]*string{claimRow("7", "11", "700001", "BC-1", "33", "501",
			"deleted_message", "alert text", "3", "stored-token", "text", nil)},
		tag: "UPDATE 1",
	})

	job, err := repo.Claim(context.Background(), 11, 2*time.Minute)
	if err != nil {
		t.Fatalf("Claim: %v", err)
	}
	if job == nil {
		t.Fatal("Claim returned no job although the server had one")
	}
	if job.ID != 7 || job.OwnerUserID != 11 || job.OwnerTelegramUserID != 700001 ||
		job.BusinessConnectionID != "BC-1" || job.ChatID != 33 || job.MessageID != 501 ||
		job.EventType != EventDeletedMessage || job.Text != "alert text" ||
		job.Attempts != 3 || job.LeaseToken != "stored-token" ||
		job.PayloadKind != PayloadKindText || job.Media != nil {
		t.Fatalf("job = %#v, want the row the server returned", job)
	}

	queries := srv.normalizedQueries()
	if len(queries) != 4 || queries[0] != "BEGIN" || queries[3] != "COMMIT" {
		t.Fatalf("queries = %v, want begin, context, claim, commit", queries)
	}
	assertEveryQueryRanInsideInTenant(t, queries, 11)
	claim := firstQueryWithPrefix(t, queries, claimSQL)
	for _, want := range []string{
		// Every non-terminal status is claimable, failed included: this is
		// the resweep.
		"STATUS IN ('PENDING', 'PROCESSING', 'FAILED')",
		"NEXT_ATTEMPT_AT <= CLOCK_TIMESTAMP()",
		"(CURRENT_JOB.LOCKED_UNTIL IS NULL OR CURRENT_JOB.LOCKED_UNTIL <= CLOCK_TIMESTAMP())",
		// A prior chunk blocks only while pending or processing: a failed
		// chunk must not retain the rest of the message.
		"PRIOR.CHUNK_INDEX < CURRENT_JOB.CHUNK_INDEX",
		"PRIOR.STATUS NOT IN ('SENT', 'FAILED')",
		"ORDER BY CURRENT_JOB.NEXT_ATTEMPT_AT, CURRENT_JOB.ID LIMIT 1 FOR UPDATE SKIP LOCKED",
		// A reclaimed failure re-enters with a fresh attempt budget.
		"ATTEMPTS = CASE WHEN O.STATUS = 'FAILED' THEN 0 ELSE O.ATTEMPTS END",
		// The lease is stamped from the server clock, for the requested
		// duration.
		"LOCKED_UNTIL = CLOCK_TIMESTAMP() + MAKE_INTERVAL(SECS => '120')",
	} {
		if !strings.Contains(claim, want) {
			t.Fatalf("the claim %q does not carry %q", claim, want)
		}
	}
	// The lease token written is a fresh 16-byte hex value: the fencing token
	// the acknowledgement will have to present.
	m := regexp.MustCompile(`LEASE_TOKEN = '([0-9A-F]{32})'`).FindStringSubmatch(claim)
	if m == nil {
		t.Fatalf("the claim does not write a fresh hex lease token: %q", claim)
	}
	if _, err := hex.DecodeString(m[1]); err != nil {
		t.Fatalf("the lease token %q is not hex: %v", m[1], err)
	}
}

// TestClaimOnAnEmptyQueueReturnsNilJob pins the idle case: UPDATE 0, no row,
// no error -- the caller distinguishes "nothing to do" from "something broke".
func TestClaimOnAnEmptyQueueReturnsNilJob(t *testing.T) {
	repo, _, srv := newScriptedOutbox(t)
	srv.on(claimSQL, pgReply{columns: claimReturningColumns(), tag: "UPDATE 0"})

	job, err := repo.Claim(context.Background(), 11, 2*time.Minute)
	if err != nil {
		t.Fatalf("Claim on an empty queue: %v", err)
	}
	if job != nil {
		t.Fatalf("Claim = %#v, want nil", job)
	}
	assertEveryQueryRanInsideInTenant(t, srv.normalizedQueries(), 11)
}

// TestClaimScanFailureNamesTheStage: a row the repository cannot scan must
// surface as an error, never as a silently dropped or half-filled job.
func TestClaimScanFailureNamesTheStage(t *testing.T) {
	repo, _, srv := newScriptedOutbox(t)
	// owner_user_id (int8) carrying garbage: the scan fails.
	srv.on(claimSQL, pgReply{
		columns: claimReturningColumns(),
		rows: [][]*string{claimRow("7", "not-a-number", "700001", "BC-1", "33", "501",
			"deleted_message", "alert text", "3", "stored-token", "text", nil)},
		tag: "UPDATE 1",
	})

	job, err := repo.Claim(context.Background(), 11, 2*time.Minute)
	if err == nil {
		t.Fatal("Claim returned no error although the row did not scan")
	}
	if !strings.Contains(err.Error(), "outbox claim") {
		t.Fatalf("error %q does not name its stage", err)
	}
	if job != nil {
		t.Fatalf("a job came back from a failed scan: %#v", job)
	}
	if !containsExact(srv.normalizedQueries(), "ROLLBACK") {
		t.Fatal("a failed claim must not commit")
	}
}

// TestClaimDecodesTheMediaPayloadOfAMediaJob pins the read half of the media
// contract: the frozen payload is decoded back into the Job, ready for the
// worker's upload path.
func TestClaimDecodesTheMediaPayloadOfAMediaJob(t *testing.T) {
	repo, _, srv := newScriptedOutbox(t)
	payload := MediaPayload{
		MediaGroupID: "album-1",
		Items: []MediaItem{{
			MediaFileID: 9001, MessageID: 101, FileIndex: 0,
			MediaType: "photo", RelativePath: "11/a/1.jpg",
			FileName: "1.jpg", Caption: "first",
		}},
	}
	raw, err := encodeMediaPayload(payload)
	if err != nil {
		t.Fatal(err)
	}
	srv.on(claimSQL, pgReply{
		columns: claimReturningColumns(),
		rows: [][]*string{claimRow("7", "11", "700001", "BC-1", "33", "501",
			"deleted_message", "fallback text", "0", "stored-token", "media", pgStr(string(raw)))},
		tag: "UPDATE 1",
	})

	job, err := repo.Claim(context.Background(), 11, 2*time.Minute)
	if err != nil {
		t.Fatalf("Claim: %v", err)
	}
	if job == nil || job.Media == nil {
		t.Fatalf("job = %#v, want its decoded media payload", job)
	}
	if job.PayloadKind != PayloadKindMedia {
		t.Fatalf("payload kind = %q, want media", job.PayloadKind)
	}
	if job.Media.MediaGroupID != "album-1" || len(job.Media.Items) != 1 {
		t.Fatalf("media payload = %#v", job.Media)
	}
	item := job.Media.Items[0]
	if item.RelativePath != "11/a/1.jpg" || item.FileName != "1.jpg" || item.Caption != "first" ||
		item.MediaFileID != 9001 || item.MessageID != 101 || item.FileIndex != 0 {
		t.Fatalf("media item = %#v, want the frozen one", item)
	}
}

// TestClaimTreatsAnUndecodableMediaPayloadAsNoMedia pins the resilience rule:
// a row whose JSON cannot be decoded must not strand the alert -- the job then
// simply carries no media, and the worker delivers the fallback text.
func TestClaimTreatsAnUndecodableMediaPayloadAsNoMedia(t *testing.T) {
	repo, _, srv := newScriptedOutbox(t)
	srv.on(claimSQL, pgReply{
		columns: claimReturningColumns(),
		rows: [][]*string{claimRow("7", "11", "700001", "BC-1", "33", "501",
			"deleted_message", "fallback text", "0", "stored-token", "media", pgStr(`{"items":`))},
		tag: "UPDATE 1",
	})

	job, err := repo.Claim(context.Background(), 11, 2*time.Minute)
	if err != nil {
		t.Fatalf("Claim with an undecodable payload: %v", err)
	}
	if job == nil {
		t.Fatal("Claim returned no job")
	}
	if job.Media != nil {
		t.Fatalf("an undecodable payload became media: %#v", job.Media)
	}
	if job.Text != "fallback text" {
		t.Fatalf("the fallback text did not survive: %q", job.Text)
	}
}

// TestMarkAcknowledgePathsHonourTheLease pins the fencing contract of the
// three acknowledgements: each UPDATE is scoped to the row AND the lease token
// the worker holds, stamps its timestamps from the server clock, and writes
// its own delay -- the retry delay for MarkRetry, the 6h resweep for
// MarkFailed. A row that no longer matches (lease expired, row erased by
// DeleteTenant, token superseded) updates nothing and must surface
// ErrLeaseLost, never a silent success.
func TestMarkAcknowledgePathsHonourTheLease(t *testing.T) {
	markers := []struct {
		name      string
		prefix    string
		call      func(*Repository, context.Context) error
		fragments []string
	}{
		{
			name:   "MarkSent",
			prefix: markSentSQL,
			call: func(r *Repository, ctx context.Context) error {
				return r.MarkSent(ctx, 11, 7, "tok-1")
			},
			fragments: []string{
				"SET STATUS = 'SENT', SENT_AT = CLOCK_TIMESTAMP()",
				"LOCKED_UNTIL = NULL",
				"LEASE_TOKEN = NULL",
				"LAST_ERROR_CLASS = NULL",
				"WHERE ID = '7' AND STATUS = 'PROCESSING' AND LEASE_TOKEN = 'TOK-1'",
			},
		},
		{
			name:   "MarkRetry",
			prefix: markRetrySQL,
			call: func(r *Repository, ctx context.Context) error {
				return r.MarkRetry(ctx, 11, 7, "tok-1", 17*time.Second, "telegram_503")
			},
			fragments: []string{
				"SET STATUS = 'PENDING', ATTEMPTS = ATTEMPTS + 1",
				"NEXT_ATTEMPT_AT = CLOCK_TIMESTAMP() + MAKE_INTERVAL(SECS => '17')",
				"LAST_ERROR_CLASS = 'TELEGRAM_503'",
				"WHERE ID = '7' AND STATUS = 'PROCESSING' AND LEASE_TOKEN = 'TOK-1'",
			},
		},
		{
			name:   "MarkFailed",
			prefix: markFailedSQL,
			call: func(r *Repository, ctx context.Context) error {
				return r.MarkFailed(ctx, 11, 7, "tok-1", "telegram_400")
			},
			fragments: []string{
				"SET STATUS = 'FAILED', ATTEMPTS = ATTEMPTS + 1",
				// 6h: the slow-lane resweep delay, stored not slept.
				fmt.Sprintf("NEXT_ATTEMPT_AT = CLOCK_TIMESTAMP() + MAKE_INTERVAL(SECS => '%d')",
					int(failedResweepDelay.Seconds())),
				"LAST_ERROR_CLASS = 'TELEGRAM_400'",
				"WHERE ID = '7' AND STATUS = 'PROCESSING' AND LEASE_TOKEN = 'TOK-1'",
			},
		},
	}
	for _, m := range markers {
		t.Run(m.name, func(t *testing.T) {
			t.Run("acknowledged under the lease", func(t *testing.T) {
				repo, _, srv := newScriptedOutbox(t)
				srv.on(m.prefix, pgReply{tag: "UPDATE 1"})

				if err := m.call(repo, context.Background()); err != nil {
					t.Fatalf("%s: %v", m.name, err)
				}
				queries := srv.normalizedQueries()
				if len(queries) != 4 || queries[0] != "BEGIN" || queries[3] != "COMMIT" {
					t.Fatalf("queries = %v, want begin, context, update, commit", queries)
				}
				assertEveryQueryRanInsideInTenant(t, queries, 11)
				stmt := firstQueryWithPrefix(t, queries, m.prefix)
				for _, want := range m.fragments {
					if !strings.Contains(stmt, want) {
						t.Fatalf("the %s statement %q does not carry %q", m.name, stmt, want)
					}
				}
			})
			t.Run("a lost lease surfaces ErrLeaseLost", func(t *testing.T) {
				repo, _, srv := newScriptedOutbox(t)
				srv.on(m.prefix, pgReply{tag: "UPDATE 0"})

				err := m.call(repo, context.Background())
				if !errors.Is(err, ErrLeaseLost) {
					t.Fatalf("%s on a lost lease = %v, want ErrLeaseLost", m.name, err)
				}
			})
			t.Run("a database failure names the update stage", func(t *testing.T) {
				repo, _, srv := newScriptedOutbox(t)
				srv.on(m.prefix, pgReply{err: "disk full"})

				err := m.call(repo, context.Background())
				if err == nil {
					t.Fatalf("%s returned no error although the update failed", m.name)
				}
				if !strings.Contains(err.Error(), "outbox update") {
					t.Fatalf("error %q does not name its stage", err)
				}
				if !containsExact(srv.normalizedQueries(), "ROLLBACK") {
					t.Fatal("a failed acknowledgement must not commit")
				}
			})
		})
	}
}

// TestCountBacklogSumsEveryTenantInsideItsOwnContext pins the gauge source:
// one count PER tenant, each inside that tenant's own InTenant transaction
// (the FORCE RLS trap: a single global count would silently return 0), failed
// rows included, and a tenant's failure surfacing with the tenant named while
// keeping the tenants already counted.
func TestCountBacklogSumsEveryTenantInsideItsOwnContext(t *testing.T) {
	countColumns := []pgColumn{{name: "count", oid: oidInt8}}
	t.Run("each tenant is counted in its own transaction", func(t *testing.T) {
		repo, _, srv := newScriptedOutbox(t)
		srv.on(backlogTenant11SQL, pgReply{columns: countColumns, rows: [][]*string{pgRow("2")}, tag: "SELECT 1"})
		srv.on(backlogTenant12SQL, pgReply{columns: countColumns, rows: [][]*string{pgRow("3")}, tag: "SELECT 1"})

		total, err := repo.CountBacklog(context.Background(), []users.TenantRetention{
			{OwnerUserID: 11, RetentionDays: 30},
			{OwnerUserID: 12, RetentionDays: 365},
		})
		if err != nil {
			t.Fatalf("CountBacklog: %v", err)
		}
		if total != 5 {
			t.Fatalf("CountBacklog = %d, want the 2+3 rows the database reports", total)
		}

		queries := srv.normalizedQueries()
		wantSequence := []string{
			"BEGIN", tenantContextSQL(11), backlogTenant11SQL, "COMMIT",
			"BEGIN", tenantContextSQL(12), backlogTenant12SQL, "COMMIT",
		}
		if len(queries) != len(wantSequence) {
			t.Fatalf("queries = %v, want the sequence %v", queries, wantSequence)
		}
		for i, want := range wantSequence {
			if !strings.HasPrefix(queries[i], want) {
				t.Fatalf("query %d is %q, want it to start with %q", i, queries[i], want)
			}
		}
		// failed is included on purpose: the gauge must not drop to zero
		// precisely when every alert is stuck in the slow lane.
		if !strings.Contains(firstQueryWithPrefix(t, queries, "SELECT COUNT(*) FROM NOTIFICATION_OUTBOX"),
			"STATUS IN ('PENDING', 'PROCESSING', 'FAILED')") {
			t.Fatal("the backlog count does not include the failed rows")
		}
	})

	t.Run("a tenant's failure keeps the counts already made", func(t *testing.T) {
		repo, _, srv := newScriptedOutbox(t)
		srv.on(backlogTenant11SQL, pgReply{columns: countColumns, rows: [][]*string{pgRow("2")}, tag: "SELECT 1"})
		srv.on(backlogTenant12SQL, pgReply{err: "disk full"})

		total, err := repo.CountBacklog(context.Background(), []users.TenantRetention{
			{OwnerUserID: 11, RetentionDays: 30},
			{OwnerUserID: 12, RetentionDays: 365},
		})
		if err == nil {
			t.Fatal("CountBacklog returned no error although tenant 12 failed")
		}
		if total != 2 {
			t.Fatalf("CountBacklog = %d on failure, want the 2 rows tenant 11 already counted", total)
		}
		if !strings.Contains(err.Error(), "outbox backlog count for tenant 12") {
			t.Fatalf("error %q does not name its stage and tenant", err)
		}
	})

	t.Run("no tenants means no query at all", func(t *testing.T) {
		repo, _, srv := newScriptedOutbox(t)
		total, err := repo.CountBacklog(context.Background(), nil)
		if err != nil || total != 0 {
			t.Fatalf("CountBacklog(nil) = (%d, %v), want (0, nil)", total, err)
		}
		if queries := srv.normalizedQueries(); len(queries) != 0 {
			t.Fatalf("CountBacklog(nil) ran %v", queries)
		}
	})
}

// TestDeleteTenantRemovesEveryQueuedAlertWhateverItsStatus pins the erasure
// half: the DELETE carries no status predicate -- a leased 'processing' row is
// an alert about to be sent for an owner whose data must be gone, so it goes
// with the rest, and the worker's next acknowledgement fails with
// ErrLeaseLost, which it already knows how to survive.
func TestDeleteTenantRemovesEveryQueuedAlertWhateverItsStatus(t *testing.T) {
	t.Run("every status goes, processing included", func(t *testing.T) {
		repo, _, srv := newScriptedOutbox(t)
		srv.on(deleteTenant11SQL, pgReply{tag: "DELETE 7"})

		deleted, err := repo.DeleteTenant(context.Background(), 11)
		if err != nil {
			t.Fatalf("DeleteTenant: %v", err)
		}
		if deleted != 7 {
			t.Fatalf("DeleteTenant = %d, want the 7 rows the database reports", deleted)
		}
		queries := srv.normalizedQueries()
		if len(queries) != 4 || queries[0] != "BEGIN" || queries[3] != "COMMIT" {
			t.Fatalf("queries = %v, want begin, context, delete, commit", queries)
		}
		assertEveryQueryRanInsideInTenant(t, queries, 11)
		del := firstQueryWithPrefix(t, queries, deleteTenant11SQL)
		if strings.Contains(del, "STATUS") {
			t.Fatalf("the erasure delete filters on status: %q", del)
		}
	})

	t.Run("the delete failure names the tenant", func(t *testing.T) {
		repo, _, srv := newScriptedOutbox(t)
		srv.on(deleteTenant11SQL, pgReply{err: "disk full"})

		_, err := repo.DeleteTenant(context.Background(), 11)
		if err == nil {
			t.Fatal("DeleteTenant returned no error although the delete failed")
		}
		if !strings.Contains(err.Error(), "deleting queued alerts of tenant 11") {
			t.Fatalf("error %q does not name its stage and tenant", err)
		}
		if !containsExact(srv.normalizedQueries(), "ROLLBACK") {
			t.Fatal("a failed erasure must not commit")
		}
	})
}

// TestPurgeExpiredLoopsTenantByTenantWithEachWindow pins the retention cycle:
// one InTenant transaction PER tenant with that tenant's own retention window,
// only sent and failed rows eligible (pending work never dies by retention),
// and a tenant's failure keeping the work already done.
func TestPurgeExpiredLoopsTenantByTenantWithEachWindow(t *testing.T) {
	t.Run("each tenant gets its own transaction and window", func(t *testing.T) {
		repo, _, srv := newScriptedOutbox(t)
		srv.on(purgeTenant11SQL, pgReply{tag: "DELETE 3"})
		srv.on(purgeTenant12SQL, pgReply{tag: "DELETE 4"})

		purged, err := repo.PurgeExpired(context.Background(), []users.TenantRetention{
			{OwnerUserID: 11, RetentionDays: 30},
			{OwnerUserID: 12, RetentionDays: 365},
		})
		if err != nil {
			t.Fatalf("PurgeExpired: %v", err)
		}
		if purged != 7 {
			t.Fatalf("PurgeExpired = %d, want the 3+4 rows the database reports", purged)
		}

		queries := srv.normalizedQueries()
		wantSequence := []string{
			"BEGIN", tenantContextSQL(11), purgeTenant11SQL, "COMMIT",
			"BEGIN", tenantContextSQL(12), purgeTenant12SQL, "COMMIT",
		}
		if len(queries) != len(wantSequence) {
			t.Fatalf("queries = %v, want the sequence %v", queries, wantSequence)
		}
		for i, want := range wantSequence {
			if !strings.HasPrefix(queries[i], want) {
				t.Fatalf("query %d is %q, want it to start with %q", i, queries[i], want)
			}
		}
		// The retention window travels to the server clock per tenant, and
		// only terminal rows are eligible.
		purge11 := firstQueryWithPrefix(t, queries, purgeTenant11SQL)
		if !strings.Contains(purge11, "STATUS IN ('SENT', 'FAILED')") ||
			!strings.Contains(purge11, "MAKE_INTERVAL(DAYS => '30')") {
			t.Fatalf("tenant 11's purge %q does not carry its window and statuses", purge11)
		}
		purge12 := firstQueryWithPrefix(t, queries, purgeTenant12SQL)
		if !strings.Contains(purge12, "MAKE_INTERVAL(DAYS => '365')") {
			t.Fatalf("tenant 12's purge %q does not carry its 365-day window", purge12)
		}
	})

	t.Run("a tenant's failure keeps the work already done", func(t *testing.T) {
		repo, _, srv := newScriptedOutbox(t)
		srv.on(purgeTenant11SQL, pgReply{tag: "DELETE 3"})
		srv.on(purgeTenant12SQL, pgReply{err: "lock timeout"})

		purged, err := repo.PurgeExpired(context.Background(), []users.TenantRetention{
			{OwnerUserID: 11, RetentionDays: 30},
			{OwnerUserID: 12, RetentionDays: 365},
		})
		if err == nil {
			t.Fatal("PurgeExpired returned no error although tenant 12 failed")
		}
		if purged != 3 {
			t.Fatalf("PurgeExpired = %d on failure, want the 3 rows tenant 11 already purged", purged)
		}
		if !strings.Contains(err.Error(), "outbox purge for tenant 12") {
			t.Fatalf("error %q does not name its stage and tenant", err)
		}
	})

	t.Run("no tenants means no query at all", func(t *testing.T) {
		repo, _, srv := newScriptedOutbox(t)
		purged, err := repo.PurgeExpired(context.Background(), nil)
		if err != nil || purged != 0 {
			t.Fatalf("PurgeExpired(nil) = (%d, %v), want (0, nil)", purged, err)
		}
		if queries := srv.normalizedQueries(); len(queries) != 0 {
			t.Fatalf("PurgeExpired(nil) ran %v", queries)
		}
	})
}
