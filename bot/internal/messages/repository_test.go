package messages

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

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/LouisMoretti/Undelete/bot/internal/storage"
	"github.com/LouisMoretti/Undelete/bot/internal/users"
)

// This file covers repository.go, the SQL half of the package, WITHOUT a
// database: a scripted fake speaks enough of the PostgreSQL wire protocol for
// the real pgxpool to connect, and storage.DB.InTenant runs its real
// BEGIN / set_config / COMMIT sequence against it. What the fake then observes
// is exactly what the database would see: the statements, their order, and
// their inlined parameters -- including the parameters the repository hands to
// outbox.InsertTx / outbox.InsertMediaTx and to the media catalogue reads it
// runs on the caller's transaction. The semantic half (RLS actually filtering,
// upserts actually converging, the server clock) stays the integration tests'.
// What only a fake can pin is that every statement travels inside an InTenant
// transaction, that MarkDeleted writes text chunks, media entries and
// deleted_at in ONE transaction, in that order, and that each server failure
// maps to the stage-named error the caller branches on.
//
// The DSN forces default_query_exec_mode=simple_protocol, so every query
// arrives as one 'Q' message with its arguments inlined -- which is also what
// makes the parameters assertable. The fake answers anything unscripted with
// an error, so a query the test did not plan for fails loudly instead of
// silently succeeding.

// PostgreSQL type OIDs the scripted RowDescriptions need.
const (
	oidInt4        = 23
	oidInt8        = 20
	oidText        = 25
	oidTimestamptz = 1184
)

// The normalised prefixes the repository's statements are recognised by. In
// simple protocol every argument arrives quoted (integers included: pgx encodes
// every parameter to text, then quotes it), nil as the bare NULL, and the
// message id batch as an array literal -- which normSQL folds into the single
// form these prefixes and the assertions below are written against.
const (
	chatsInsertSQL    = "INSERT INTO CHATS"
	messagesInsertSQL = "INSERT INTO MESSAGES"
	chatLabelReadSQL  = "SELECT TITLE, USERNAME FROM CHATS"
	markDeletedSQL    = "UPDATE MESSAGES SET DELETED_AT = COALESCE"
	outboxInsertSQL   = "INSERT INTO NOTIFICATION_OUTBOX"
	mediaFilesSQL     = "SELECT ID, BUSINESS_CONNECTION_ID, CHAT_ID, MESSAGE_ID, FILE_INDEX"
	albumAnchorsSQL   = "SELECT MEDIA_GROUP_ID, MIN(MESSAGE_ID)"
	countByOwnerSQL   = "SELECT COUNT(*) FROM MESSAGES"

	// DeleteTenant's two statements, keyed on the tenant the tests use.
	deleteTenantMessagesSQL = "DELETE FROM MESSAGES WHERE OWNER_USER_ID = '11'"
	deleteTenantChatsSQL    = "DELETE FROM CHATS WHERE OWNER_USER_ID = '11'"

	// PurgeExpired's per-tenant DELETE, distinguished from DeleteTenant's by
	// its retention predicate.
	purgeTenant11SQL = "DELETE FROM MESSAGES WHERE OWNER_USER_ID = '11' AND SAVED_AT"
	purgeTenant12SQL = "DELETE FROM MESSAGES WHERE OWNER_USER_ID = '12' AND SAVED_AT"
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
// nil entry in a row is a SQL NULL. errAfterRows breaks the stream AFTER the
// DataRows (no CommandComplete): the rows the client already read stand, and
// the error surfaces through rows.Err().
type pgReply struct {
	columns      []pgColumn
	rows         [][]*string
	tag          string
	err          string
	errAfterRows string
}

// scriptedPG is the fake server and its script. Each rule holds a FIFO of
// replies for one statement prefix: the outbox inserts of one MarkDeleted call
// (text chunks then the media entry) share a rule and get one reply each.
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
		if reply.errAfterRows != "" {
			if !writeError(conn, reply.errAfterRows) {
				return false
			}
		} else if !writeCommandComplete(conn, reply.tag) {
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
// how a nullable column (from_user_id, byte_size, ...) comes back unset.
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
// stray spaces before closing punctuation removed.
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

func queriesWithPrefix(queries []string, prefix string) []string {
	var out []string
	for _, q := range queries {
		if strings.HasPrefix(q, prefix) {
			out = append(out, q)
		}
	}
	return out
}

// assertEveryQueryRanInsideInTenant walks the recorded stream and enforces the
// package's one structural rule: every statement reaches the database inside a
// transaction that opened with app.current_owner_user_id set to THIS owner,
// LOCAL to that transaction. A query sneaking past InTenant would run with no
// tenant context, which RLS answers with zero rows -- fail-closed, but
// silently wrong (cf. the PurgeExpired trap in repository.go).
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

// deletedReturningColumns describes the RETURNING row of MarkDeleted's UPDATE:
// seven columns, from_user_id nullable.
func deletedReturningColumns() []pgColumn {
	return []pgColumn{
		{name: "chat_id", oid: oidInt8},
		{name: "message_id", oid: oidInt8},
		{name: "from_user_id", oid: oidInt8},
		{name: "from_display", oid: oidText},
		{name: "message_type", oid: oidText},
		{name: "text_content", oid: oidText},
		{name: "telegram_date", oid: oidInt8},
	}
}

// mediaCatalogueColumns describes the row of media.SelectStoredTx /
// SelectAlbumAnchorsTx's shared column list (media.selectColumns): twenty
// columns in the scan order media.scanFiles expects.
func mediaCatalogueColumns() []pgColumn {
	return []pgColumn{
		{name: "id", oid: oidInt8},
		{name: "business_connection_id", oid: oidText},
		{name: "chat_id", oid: oidInt8},
		{name: "message_id", oid: oidInt8},
		{name: "file_index", oid: oidInt4},
		{name: "telegram_file_id", oid: oidText},
		{name: "telegram_file_unique_id", oid: oidText},
		{name: "media_type", oid: oidText},
		{name: "mime_type", oid: oidText},
		{name: "file_name", oid: oidText},
		{name: "byte_size", oid: oidInt8},
		{name: "width", oid: oidInt4},
		{name: "height", oid: oidInt4},
		{name: "duration_sec", oid: oidInt4},
		{name: "relative_path", oid: oidText},
		{name: "thumbnail_relative_path", oid: oidText},
		{name: "sha256", oid: oidText},
		{name: "status", oid: oidText},
		{name: "media_group_id", oid: oidText},
		{name: "created_at", oid: oidTimestamptz},
	}
}

// storedMediaRow builds one 'stored' catalogue row for SelectStoredTx: an
// attachment of chat 33, on disk, belonging to the given album ("" = lone
// media). byte_size/width/height set, duration NULL.
func storedMediaRow(id, messageID, fileIndex, groupID string) []*string {
	return []*string{
		pgStr(id), pgStr("BC-1"), pgStr("33"), pgStr(messageID), pgStr(fileIndex),
		pgStr("tg-" + id), pgStr("uniq-" + id), pgStr("photo"),
		pgStr("image/jpeg"), pgStr("pic" + id + ".jpg"),
		pgStr("1024"), pgStr("800"), pgStr("600"), nil,
		pgStr("11/a/pic" + id + ".jpg"), pgStr(""),
		pgStr("sha-" + id), pgStr("stored"), pgStr(groupID),
		pgStr("2026-09-01 10:00:00+00"),
	}
}

func pgStr(v string) *string { return &v }

// TestSaveUpsertsChatLabelThenMessageAtomically pins Save on the wire: the
// chat label upsert and the message upsert travel in ONE InTenant transaction,
// label first (the only moment the full Chat is visible), with every field of
// the record inlined and the edited flag driving the server-side edited_at
// decision -- now() on an edit, untouched on a double delivery.
func TestSaveUpsertsChatLabelThenMessageAtomically(t *testing.T) {
	for _, tc := range []struct {
		name      string
		edited    bool
		fromUser  *int64
		wantCase  string
		wantFrom  string
		wantExtra string
	}{
		{
			name:     "new message",
			edited:   false,
			fromUser: nil,
			// $10 inlined twice: VALUES writes NULL, ON CONFLICT preserves.
			// (pgx inlines a bool parameter as PostgreSQL's own 't'/'f'.)
			wantCase:  "CASE WHEN 'F' THEN NOW() ELSE NULL END",
			wantFrom:  "NULL",
			wantExtra: "CASE WHEN 'F' THEN NOW() ELSE MESSAGES.EDITED_AT END",
		},
		{
			name:      "edited message",
			edited:    true,
			fromUser:  pgInt64(700002),
			wantCase:  "CASE WHEN 'T' THEN NOW() ELSE NULL END",
			wantFrom:  "'700002'",
			wantExtra: "CASE WHEN 'T' THEN NOW() ELSE MESSAGES.EDITED_AT END",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			repo, srv := newScriptedRepository(t)
			srv.on(chatsInsertSQL, pgReply{tag: "INSERT 0 1"})
			srv.on(messagesInsertSQL, pgReply{tag: "INSERT 0 1"})

			rec := Record{
				BusinessConnectionID: "BC-1",
				ChatID:               33,
				MessageID:            501,
				FromUserID:           tc.fromUser,
				FromDisplay:          "Alice",
				MessageType:          "text",
				TextContent:          "hello",
				TelegramDate:         1726000000,
				ChatTitle:            "Project Chat",
				ChatUsername:         "projectchat",
				ChatType:             "private",
			}
			if err := repo.Save(context.Background(), 11, rec, tc.edited); err != nil {
				t.Fatalf("Save: %v", err)
			}

			queries := srv.normalizedQueries()
			if len(queries) != 5 {
				t.Fatalf("queries = %v, want begin, context, chat upsert, message upsert, commit", queries)
			}
			assertEveryQueryRanInsideInTenant(t, queries, 11)

			chatUpsert := firstQueryWithPrefix(t, queries, chatsInsertSQL)
			for _, want := range []string{
				"VALUES ('11', 'BC-1', '33', 'PROJECT CHAT', 'PROJECTCHAT', 'PRIVATE')",
				// Constraint #8: the label upsert carries no chat selection.
				"ON CONFLICT (OWNER_USER_ID, BUSINESS_CONNECTION_ID, CHAT_ID)",
				// An update without a label must not wipe the known one.
				"EXCLUDED.TITLE <> ''",
				"EXCLUDED.USERNAME <> ''",
				"LAST_SEEN_AT = NOW()",
			} {
				if !strings.Contains(chatUpsert, want) {
					t.Fatalf("the chat upsert %q does not carry %q", chatUpsert, want)
				}
			}

			msgUpsert := firstQueryWithPrefix(t, queries, messagesInsertSQL)
			wantValues := "VALUES ('11', 'BC-1', '33', '501', " + tc.wantFrom +
				", 'ALICE', 'TEXT', 'HELLO', '1726000000', " + tc.wantCase
			for _, want := range []string{
				wantValues,
				tc.wantExtra,
				"ON CONFLICT (OWNER_USER_ID, BUSINESS_CONNECTION_ID, CHAT_ID, MESSAGE_ID)",
				"TEXT_CONTENT = EXCLUDED.TEXT_CONTENT",
			} {
				if !strings.Contains(msgUpsert, want) {
					t.Fatalf("the message upsert %q does not carry %q", msgUpsert, want)
				}
			}
		})
	}
}

// TestSaveFailuresSurface: a database that cannot record the chat label or the
// message must produce an error naming the stage, and the message upsert must
// not run after a failed label upsert -- the two describe each other and are
// written together or not at all.
func TestSaveFailuresSurface(t *testing.T) {
	t.Run("the chat label upsert fails", func(t *testing.T) {
		repo, srv := newScriptedRepository(t)
		srv.on(chatsInsertSQL, pgReply{err: "disk full"})

		err := repo.Save(context.Background(), 11, Record{BusinessConnectionID: "BC-1"}, false)
		if err == nil {
			t.Fatal("Save returned no error although the label upsert failed")
		}
		if !strings.Contains(err.Error(), "chat label upsert") {
			t.Fatalf("error %q does not name its stage", err)
		}
		if got := countQueriesWithPrefix(srv.normalizedQueries(), messagesInsertSQL); got != 0 {
			t.Fatalf("the message upsert ran %d times after a failed label upsert", got)
		}
	})

	t.Run("the message upsert fails", func(t *testing.T) {
		repo, srv := newScriptedRepository(t)
		srv.on(chatsInsertSQL, pgReply{tag: "INSERT 0 1"})
		srv.on(messagesInsertSQL, pgReply{err: "disk full"})

		err := repo.Save(context.Background(), 11, Record{BusinessConnectionID: "BC-1"}, false)
		if err == nil {
			t.Fatal("Save returned no error although the message upsert failed")
		}
		if !strings.Contains(err.Error(), "message upsert") {
			t.Fatalf("error %q does not name its stage", err)
		}
	})
}

// longDeletedContent is long enough that its deletion alert needs two Telegram
// messages: the chunk chaining (text chunks first, media entry after them) is
// exactly what that second chunk makes observable.
const longDeletedContent = "five thousand characters of restored text " +
	"xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx" +
	"xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx" +
	"xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx" +
	"xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx" +
	"xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx" +
	"xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx" +
	"xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx" +
	"xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx" +
	"xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx" +
	"xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx" +
	"xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx" +
	"xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx" +
	"xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx" +
	"xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx" +
	"xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx" +
	"xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx" +
	"xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx" +
	"xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx" +
	"xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx" +
	"xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx" +
	"xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx" +
	"xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx" +
	"xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx" +
	"xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx" +
	"xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx" +
	"xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx" +
	"xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx" +
	"xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx" +
	"xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx" +
	"xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx" +
	"xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx" +
	"xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx" +
	"xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx" +
	"xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx" +
	"xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx" +
	"xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx" +
	"xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx" +
	"xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx" +
	"xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx" +
	"xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx" +
	"xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx" +
	"xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx" +
	"xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx" +
	"xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx" +
	"xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx" +
	"xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx" +
	"xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx" +
	"xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx" +
	"xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx" +
	"xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx" +
	"xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx" +
	"xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx" +
	"xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx" +
	"xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx" +
	"xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx" +
	"xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx" +
	"xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx" +
	"xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx"

// TestMarkDeletedNotifiesTextThenMediaInOneTransaction pins the whole MarkDeleted
// contract on the wire: the chat label is read first in the SAME transaction,
// the UPDATE carries the whole batch via ANY(...), the found messages come
// back sorted by message_id whatever order the server returned (an album must
// reach the owner in send order), the text chunks of each message are written
// in that order, and the media entry LAST -- keyed on the album's catalogued
// anchor, at the chunk index right after the text chunks of that message.
func TestMarkDeletedNotifiesTextThenMediaInOneTransaction(t *testing.T) {
	repo, srv := newScriptedRepository(t)
	srv.on(chatLabelReadSQL, pgReply{
		columns: []pgColumn{{name: "title", oid: oidText}, {name: "username", oid: oidText}},
		rows:    [][]*string{pgRow("Team", "team")},
		tag:     "SELECT 1",
	})
	// The server returns the batch OUT of order: the sort under test is the
	// repository's, not PostgreSQL's goodwill.
	srv.on(markDeletedSQL, pgReply{
		columns: deletedReturningColumns(),
		rows: [][]*string{
			{pgStr("33"), pgStr("102"), nil, pgStr("Bob"), pgStr("photo"), pgStr("second pic"), pgStr("1726000012")},
			{pgStr("33"), pgStr("101"), pgStr("700002"), pgStr("Alice"), pgStr("text"), pgStr(longDeletedContent), pgStr("1726000005")},
		},
		tag: "UPDATE 2",
	})
	// Four outbox writes in delivery order: chunks 0 and 1 of message 101,
	// chunk 0 of message 102, then the album's media entry.
	srv.on(outboxInsertSQL,
		pgReply{tag: "INSERT 0 1"},
		pgReply{tag: "INSERT 0 1"},
		pgReply{tag: "INSERT 0 1"},
		pgReply{tag: "INSERT 0 1"},
	)
	srv.on(mediaFilesSQL, pgReply{
		columns: mediaCatalogueColumns(),
		rows: [][]*string{
			storedMediaRow("9001", "101", "0", "G"),
			storedMediaRow("9002", "102", "0", "G"),
		},
		tag: "SELECT 2",
	})
	srv.on(albumAnchorsSQL, pgReply{
		columns: []pgColumn{{name: "media_group_id", oid: oidText}, {name: "min", oid: oidInt8}},
		rows:    [][]*string{pgRow("G", "101")},
		tag:     "SELECT 1",
	})

	found, err := repo.MarkDeleted(context.Background(), 11, 700001, "BC-1", 33, []int64{101, 102})
	if err != nil {
		t.Fatalf("MarkDeleted: %v", err)
	}

	// The batch came back sorted by message_id, with each message's own row.
	if len(found) != 2 {
		t.Fatalf("%d messages found, want 2: %+v", len(found), found)
	}
	if found[0].MessageID != 101 || found[1].MessageID != 102 {
		t.Fatalf("found = [%d %d], want sorted [101 102]", found[0].MessageID, found[1].MessageID)
	}
	if found[0].FromUserID == nil || *found[0].FromUserID != 700002 {
		t.Fatalf("message 101 from_user_id = %v, want 700002", found[0].FromUserID)
	}
	if found[1].FromUserID != nil {
		t.Fatalf("message 102 from_user_id = %v, want NULL", found[1].FromUserID)
	}
	if found[0].FromDisplay != "Alice" || found[1].FromDisplay != "Bob" {
		t.Fatalf("from_display = [%q %q], want [Alice Bob]", found[0].FromDisplay, found[1].FromDisplay)
	}
	if found[0].TextContent != longDeletedContent || found[1].TextContent != "second pic" {
		t.Fatalf("text_content mismatch: [%d %d] chars, want [%d %d]",
			len(found[0].TextContent), len(found[1].TextContent), len(longDeletedContent), len("second pic"))
	}

	queries := srv.normalizedQueries()
	wantSequence := []string{
		"BEGIN",
		tenantContextSQL(11),
		chatLabelReadSQL,
		markDeletedSQL,
		outboxInsertSQL, // 101 chunk 0
		outboxInsertSQL, // 101 chunk 1
		outboxInsertSQL, // 102 chunk 0
		mediaFilesSQL,   // media read AFTER every text chunk of the batch
		albumAnchorsSQL, // anchors from the FULL catalogue, not the stored set
		outboxInsertSQL, // the album's media entry, last
		"COMMIT",
	}
	if len(queries) != len(wantSequence) {
		t.Fatalf("queries = %v, want the sequence %v", queries, wantSequence)
	}
	for i, want := range wantSequence {
		if !strings.HasPrefix(queries[i], want) {
			t.Fatalf("query %d is %q, want it to start with %q", i, queries[i], want)
		}
	}
	assertEveryQueryRanInsideInTenant(t, queries, 11)

	// The UPDATE is one statement for the whole batch, idempotent on redelivery.
	update := firstQueryWithPrefix(t, queries, markDeletedSQL)
	for _, want := range []string{
		"SET DELETED_AT = COALESCE(DELETED_AT, NOW())",
		"BUSINESS_CONNECTION_ID = 'BC-1'",
		"CHAT_ID = '33'",
		"MESSAGE_ID = ANY('{101,102}')",
		"COALESCE(FROM_DISPLAY, '')",
		"COALESCE(TEXT_CONTENT, '')",
	} {
		if !strings.Contains(update, want) {
			t.Fatalf("the update %q does not carry %q", update, want)
		}
	}

	// The media catalogue read only sees stored files, ordered as the sender
	// composed them; the anchors read is status-independent by design.
	files := firstQueryWithPrefix(t, queries, mediaFilesSQL)
	for _, want := range []string{
		"STATUS = 'STORED'",
		"RELATIVE_PATH IS NOT NULL",
		"ORDER BY MESSAGE_ID, FILE_INDEX",
		"ANY('{101,102}')",
	} {
		if !strings.Contains(files, want) {
			t.Fatalf("the media read %q does not carry %q", files, want)
		}
	}
	anchors := firstQueryWithPrefix(t, queries, albumAnchorsSQL)
	if !strings.Contains(anchors, "GROUP BY MEDIA_GROUP_ID") {
		t.Fatalf("the anchors read %q does not group by media_group_id", anchors)
	}

	// Every outbox row carries the tenant identity, and the chunk chaining is
	// exactly the delivery contract: 101/0, 101/1, 102/0, then the album on
	// its anchor 101 at the index right after that message's text chunks.
	outbox := queriesWithPrefix(queries, outboxInsertSQL)
	if len(outbox) != 4 {
		t.Fatalf("%d outbox writes, want 4", len(outbox))
	}
	for i, insert := range outbox {
		if !strings.Contains(insert, "'11', '700001', 'BC-1', '33'") {
			t.Fatalf("outbox write %d (%q) does not carry the tenant identity", i, insert)
		}
	}
	wantChunks := []string{
		"'101', 'DELETED_MESSAGE', '0'",
		"'101', 'DELETED_MESSAGE', '1'",
		"'102', 'DELETED_MESSAGE', '0'",
		"'101', 'DELETED_MESSAGE', '2'",
	}
	for i, want := range wantChunks {
		if !strings.Contains(outbox[i], want) {
			t.Fatalf("outbox write %d (%q) does not carry %q", i, outbox[i], want)
		}
	}
	// The alert itself is built by telegram.BuildDeletionMessageRequests: the
	// identity header proves the text chunks are real alerts, not placeholders.
	if !strings.Contains(outbox[0], "RECOVERED DELETED MESSAGE") || !strings.Contains(outbox[0], "TEAM (@TEAM)") {
		t.Fatalf("the first chunk %q is not a deletion alert with the chat label", outbox[0])
	}
	// Text chunks are text-kind with no media payload; the media entry is
	// media-kind with the JSON payload inlined.
	if !strings.Contains(outbox[0], "'TEXT', NULL") {
		t.Fatalf("a text chunk carries something else than (text, NULL): %q", outbox[0])
	}
	if !strings.Contains(outbox[3], "'MEDIA', '\\X") {
		t.Fatalf("the media entry does not carry a media payload: %q", outbox[3])
	}
}

// TestMarkDeletedWithoutFoundMessagesEnqueuesNothing pins the expected-loss
// path: message ids never seen (prior to the connection) or already purged
// produce no outbox row and no catalogue read -- the decision to log-and-
// continue belongs to the caller, which gets an empty batch, not an error.
func TestMarkDeletedWithoutFoundMessagesEnqueuesNothing(t *testing.T) {
	repo, srv := newScriptedRepository(t)
	srv.on(chatLabelReadSQL, pgReply{
		columns: []pgColumn{{name: "title", oid: oidText}, {name: "username", oid: oidText}},
		tag:     "SELECT 0",
	})
	srv.on(markDeletedSQL, pgReply{columns: deletedReturningColumns(), tag: "UPDATE 0"})

	found, err := repo.MarkDeleted(context.Background(), 11, 700001, "BC-1", 33, []int64{999})
	if err != nil {
		t.Fatalf("MarkDeleted: %v", err)
	}
	if len(found) != 0 {
		t.Fatalf("found = %+v, want empty", found)
	}

	queries := srv.normalizedQueries()
	if len(queries) != 5 || queries[0] != "BEGIN" || queries[4] != "COMMIT" {
		t.Fatalf("queries = %v, want begin, context, label read, update, commit", queries)
	}
	assertEveryQueryRanInsideInTenant(t, queries, 11)
	if got := countQueriesWithPrefix(queries, outboxInsertSQL); got != 0 {
		t.Fatalf("%d outbox writes for a batch that found nothing", got)
	}
	if got := countQueriesWithPrefix(queries, mediaFilesSQL); got != 0 {
		t.Fatalf("%d media reads for a batch that found nothing", got)
	}
}

// TestMarkDeletedMissingChatLabelFallsBackToChatID pins the degraded label
// read: a chat unseen since migration 0003 has no row, the alert then names
// the chat by its id -- "chat 33" -- and the deletion is still notified.
func TestMarkDeletedMissingChatLabelFallsBackToChatID(t *testing.T) {
	repo, srv := newScriptedRepository(t)
	srv.on(chatLabelReadSQL, pgReply{
		columns: []pgColumn{{name: "title", oid: oidText}, {name: "username", oid: oidText}},
		tag:     "SELECT 0", // ErrNoRows: the chat label was never captured
	})
	srv.on(markDeletedSQL, pgReply{
		columns: deletedReturningColumns(),
		rows:    [][]*string{{pgStr("33"), pgStr("105"), nil, pgStr("Bob"), pgStr("text"), pgStr("hi"), pgStr("1726000020")}},
		tag:     "UPDATE 1",
	})
	srv.on(outboxInsertSQL, pgReply{tag: "INSERT 0 1"})
	// The catalogue reads run whatever the message type: no row here means
	// the deletion is notified as text only.
	srv.on(mediaFilesSQL, pgReply{columns: mediaCatalogueColumns(), tag: "SELECT 0"})
	srv.on(albumAnchorsSQL, pgReply{
		columns: []pgColumn{{name: "media_group_id", oid: oidText}, {name: "min", oid: oidInt8}},
		tag:     "SELECT 0",
	})

	found, err := repo.MarkDeleted(context.Background(), 11, 700001, "BC-1", 33, []int64{105})
	if err != nil {
		t.Fatalf("MarkDeleted: %v", err)
	}
	if len(found) != 1 {
		t.Fatalf("found = %+v, want the one message", found)
	}

	insert := firstQueryWithPrefix(t, srv.normalizedQueries(), outboxInsertSQL)
	if !strings.Contains(insert, "CHAT: CHAT 33") {
		t.Fatalf("the alert %q does not fall back to the chat id", insert)
	}
}

// TestMarkDeletedFailuresSurface: every statement of the deletion transaction
// can fail, and each failure must surface as an error naming its stage -- a
// deletion whose alert was lost would silently break the product's one
// promise. A failure after the first write must roll the whole transaction
// back: deleted_at and its alerts are written together or not at all.
func TestMarkDeletedFailuresSurface(t *testing.T) {
	t.Run("the chat label read fails", func(t *testing.T) {
		repo, srv := newScriptedRepository(t)
		srv.on(chatLabelReadSQL, pgReply{err: "connection reset"})

		_, err := repo.MarkDeleted(context.Background(), 11, 700001, "BC-1", 33, []int64{101})
		if err == nil {
			t.Fatal("MarkDeleted returned no error although the label read failed")
		}
		if !strings.Contains(err.Error(), "reading chat label") {
			t.Fatalf("error %q does not name its stage", err)
		}
		if got := countQueriesWithPrefix(srv.normalizedQueries(), markDeletedSQL); got != 0 {
			t.Fatalf("the update ran %d times after a failed label read", got)
		}
	})

	t.Run("the update fails", func(t *testing.T) {
		repo, srv := newScriptedRepository(t)
		srv.on(chatLabelReadSQL, pgReply{
			columns: []pgColumn{{name: "title", oid: oidText}, {name: "username", oid: oidText}},
			tag:     "SELECT 0",
		})
		srv.on(markDeletedSQL, pgReply{err: "lock timeout"})

		_, err := repo.MarkDeleted(context.Background(), 11, 700001, "BC-1", 33, []int64{101})
		if err == nil {
			t.Fatal("MarkDeleted returned no error although the update failed")
		}
		if !strings.Contains(err.Error(), "updating deleted_at") {
			t.Fatalf("error %q does not name its stage", err)
		}
	})

	t.Run("a returned row cannot be scanned", func(t *testing.T) {
		repo, srv := newScriptedRepository(t)
		srv.on(chatLabelReadSQL, pgReply{
			columns: []pgColumn{{name: "title", oid: oidText}, {name: "username", oid: oidText}},
			tag:     "SELECT 0",
		})
		// An int8 column carrying garbage: the scan fails, the batch is not
		// silently truncated to the rows that happened to scan.
		srv.on(markDeletedSQL, pgReply{
			columns: deletedReturningColumns(),
			rows:    [][]*string{{pgStr("33"), pgStr("not-a-number"), nil, pgStr("Bob"), pgStr("text"), pgStr("hi"), pgStr("1726000020")}},
			tag:     "UPDATE 1",
		})

		_, err := repo.MarkDeleted(context.Background(), 11, 700001, "BC-1", 33, []int64{101})
		if err == nil {
			t.Fatal("MarkDeleted returned no error although a row did not scan")
		}
		if !strings.Contains(err.Error(), "reading deleted message") {
			t.Fatalf("error %q does not name its stage", err)
		}
	})

	t.Run("an outbox write fails", func(t *testing.T) {
		repo, srv := newScriptedRepository(t)
		srv.on(chatLabelReadSQL, pgReply{
			columns: []pgColumn{{name: "title", oid: oidText}, {name: "username", oid: oidText}},
			tag:     "SELECT 0",
		})
		srv.on(markDeletedSQL, pgReply{
			columns: deletedReturningColumns(),
			rows:    [][]*string{{pgStr("33"), pgStr("105"), nil, pgStr("Bob"), pgStr("text"), pgStr("hi"), pgStr("1726000020")}},
			tag:     "UPDATE 1",
		})
		srv.on(outboxInsertSQL, pgReply{err: "outbox down"})

		_, err := repo.MarkDeleted(context.Background(), 11, 700001, "BC-1", 33, []int64{105})
		if err == nil {
			t.Fatal("MarkDeleted returned no error although the outbox write failed")
		}
		queries := srv.normalizedQueries()
		if got := countQueriesWithPrefix(queries, mediaFilesSQL); got != 0 {
			t.Fatalf("the media read ran %d times after a failed outbox write", got)
		}
		// deleted_at was set in this transaction, its alert could not be
		// written: nothing may have committed.
		if !containsExact(queries, "ROLLBACK") {
			t.Fatalf("the failed deletion committed: %v", queries)
		}
	})

	t.Run("the media catalogue read fails", func(t *testing.T) {
		repo, srv := newScriptedRepository(t)
		srv.on(chatLabelReadSQL, pgReply{
			columns: []pgColumn{{name: "title", oid: oidText}, {name: "username", oid: oidText}},
			tag:     "SELECT 0",
		})
		srv.on(markDeletedSQL, pgReply{
			columns: deletedReturningColumns(),
			rows:    [][]*string{{pgStr("33"), pgStr("105"), nil, pgStr("Bob"), pgStr("photo"), pgStr("hi"), pgStr("1726000020")}},
			tag:     "UPDATE 1",
		})
		srv.on(outboxInsertSQL, pgReply{tag: "INSERT 0 1"})
		srv.on(mediaFilesSQL, pgReply{err: "media catalogue unreadable"})

		_, err := repo.MarkDeleted(context.Background(), 11, 700001, "BC-1", 33, []int64{105})
		if err == nil {
			t.Fatal("MarkDeleted returned no error although the media read failed")
		}
		if !strings.Contains(err.Error(), "reading stored media") {
			t.Fatalf("error %q does not name its stage", err)
		}
		if !containsExact(srv.normalizedQueries(), "ROLLBACK") {
			t.Fatal("a deletion whose media could not be read must not commit")
		}
	})

	t.Run("the media outbox write fails", func(t *testing.T) {
		repo, srv := newScriptedRepository(t)
		srv.on(chatLabelReadSQL, pgReply{
			columns: []pgColumn{{name: "title", oid: oidText}, {name: "username", oid: oidText}},
			tag:     "SELECT 0",
		})
		srv.on(markDeletedSQL, pgReply{
			columns: deletedReturningColumns(),
			rows:    [][]*string{{pgStr("33"), pgStr("105"), nil, pgStr("Bob"), pgStr("photo"), pgStr("hi"), pgStr("1726000020")}},
			tag:     "UPDATE 1",
		})
		// The text chunk goes out, the media entry does not: deleted_at must
		// not commit with only half of its alert.
		srv.on(outboxInsertSQL,
			pgReply{tag: "INSERT 0 1"},
			pgReply{err: "outbox down"},
		)
		srv.on(mediaFilesSQL, pgReply{
			columns: mediaCatalogueColumns(),
			rows:    [][]*string{storedMediaRow("9001", "105", "0", "G")},
			tag:     "SELECT 1",
		})
		srv.on(albumAnchorsSQL, pgReply{
			columns: []pgColumn{{name: "media_group_id", oid: oidText}, {name: "min", oid: oidInt8}},
			rows:    [][]*string{pgRow("G", "105")},
			tag:     "SELECT 1",
		})

		_, err := repo.MarkDeleted(context.Background(), 11, 700001, "BC-1", 33, []int64{105})
		if err == nil {
			t.Fatal("MarkDeleted returned no error although the media entry could not be written")
		}
		if !containsExact(srv.normalizedQueries(), "ROLLBACK") {
			t.Fatal("a deletion whose media alert could not be written must not commit")
		}
	})

	t.Run("the rows stream breaks mid-batch", func(t *testing.T) {
		repo, srv := newScriptedRepository(t)
		srv.on(chatLabelReadSQL, pgReply{
			columns: []pgColumn{{name: "title", oid: oidText}, {name: "username", oid: oidText}},
			tag:     "SELECT 0",
		})
		// One deliverable row, then the stream dies: the batch must not be
		// reported as found-and-notified on a partial read.
		srv.on(markDeletedSQL, pgReply{
			columns:      deletedReturningColumns(),
			rows:         [][]*string{{pgStr("33"), pgStr("105"), nil, pgStr("Bob"), pgStr("text"), pgStr("hi"), pgStr("1726000020")}},
			errAfterRows: "server crashed mid-result",
		})

		_, err := repo.MarkDeleted(context.Background(), 11, 700001, "BC-1", 33, []int64{105})
		if err == nil {
			t.Fatal("MarkDeleted returned no error although the result stream broke")
		}
		if got := countQueriesWithPrefix(srv.normalizedQueries(), outboxInsertSQL); got != 0 {
			t.Fatalf("%d outbox writes despite a broken result stream", got)
		}
	})

	t.Run("the album anchors read fails", func(t *testing.T) {
		repo, srv := newScriptedRepository(t)
		srv.on(chatLabelReadSQL, pgReply{
			columns: []pgColumn{{name: "title", oid: oidText}, {name: "username", oid: oidText}},
			tag:     "SELECT 0",
		})
		srv.on(markDeletedSQL, pgReply{
			columns: deletedReturningColumns(),
			rows:    [][]*string{{pgStr("33"), pgStr("105"), nil, pgStr("Bob"), pgStr("photo"), pgStr("hi"), pgStr("1726000020")}},
			tag:     "UPDATE 1",
		})
		srv.on(outboxInsertSQL, pgReply{tag: "INSERT 0 1"})
		srv.on(mediaFilesSQL, pgReply{
			columns: mediaCatalogueColumns(),
			rows:    [][]*string{storedMediaRow("9001", "105", "0", "G")},
			tag:     "SELECT 1",
		})
		srv.on(albumAnchorsSQL, pgReply{err: "anchors unreadable"})

		_, err := repo.MarkDeleted(context.Background(), 11, 700001, "BC-1", 33, []int64{105})
		if err == nil {
			t.Fatal("MarkDeleted returned no error although the anchors read failed")
		}
		if !strings.Contains(err.Error(), "reading album anchors") {
			t.Fatalf("error %q does not name its stage", err)
		}
	})
}

// TestCountByOwnerReturnsTheDatabaseTruth pins the quota source: one COUNT
// inside an InTenant transaction, its value returned verbatim, and a store
// failure surfaced with the tenant named rather than as a zero count.
func TestCountByOwnerReturnsTheDatabaseTruth(t *testing.T) {
	t.Run("the count comes back", func(t *testing.T) {
		repo, srv := newScriptedRepository(t)
		srv.on(countByOwnerSQL, pgReply{
			columns: []pgColumn{{name: "count", oid: oidInt8}},
			rows:    [][]*string{pgRow("42")},
			tag:     "SELECT 1",
		})

		count, err := repo.CountByOwner(context.Background(), 11)
		if err != nil {
			t.Fatalf("CountByOwner: %v", err)
		}
		if count != 42 {
			t.Fatalf("CountByOwner = %d, want the 42 the database returned", count)
		}
		queries := srv.normalizedQueries()
		if len(queries) != 4 || queries[0] != "BEGIN" || queries[3] != "COMMIT" {
			t.Fatalf("queries = %v, want begin, context, count, commit", queries)
		}
		assertEveryQueryRanInsideInTenant(t, queries, 11)
		countQuery := firstQueryWithPrefix(t, queries, countByOwnerSQL)
		if !strings.Contains(countQuery, "OWNER_USER_ID = '11'") {
			t.Fatalf("the count %q is not scoped to the tenant", countQuery)
		}
	})

	t.Run("the store failure surfaces with the tenant named", func(t *testing.T) {
		repo, srv := newScriptedRepository(t)
		srv.on(countByOwnerSQL, pgReply{err: "disk full"})

		_, err := repo.CountByOwner(context.Background(), 11)
		if err == nil {
			t.Fatal("CountByOwner returned no error although the store failed")
		}
		if !strings.Contains(err.Error(), "counting messages of tenant 11") {
			t.Fatalf("error %q does not name its stage and tenant", err)
		}
	})
}

// TestDeleteTenantRemovesMessagesAndChatsInOneTransaction pins the erasure
// half: both tables go in ONE transaction (a crash between two deletes would
// leave chat labels describing conversations whose messages are gone), messages
// first, and each count is the one the database reports.
func TestDeleteTenantRemovesMessagesAndChatsInOneTransaction(t *testing.T) {
	t.Run("both counts come back in one transaction", func(t *testing.T) {
		repo, srv := newScriptedRepository(t)
		srv.on(deleteTenantMessagesSQL, pgReply{tag: "DELETE 5"})
		srv.on(deleteTenantChatsSQL, pgReply{tag: "DELETE 2"})

		deletedMessages, deletedChats, err := repo.DeleteTenant(context.Background(), 11)
		if err != nil {
			t.Fatalf("DeleteTenant: %v", err)
		}
		if deletedMessages != 5 || deletedChats != 2 {
			t.Fatalf("DeleteTenant = (%d, %d), want (5, 2)", deletedMessages, deletedChats)
		}

		queries := srv.normalizedQueries()
		if len(queries) != 5 || queries[0] != "BEGIN" || queries[4] != "COMMIT" {
			t.Fatalf("queries = %v, want one transaction around both deletes", queries)
		}
		assertEveryQueryRanInsideInTenant(t, queries, 11)
		// Messages first: the labels describe the messages, never the other
		// way round.
		messagesAt, chatsAt := -1, -1
		for i, q := range queries {
			switch {
			case strings.HasPrefix(q, deleteTenantMessagesSQL):
				messagesAt = i
			case strings.HasPrefix(q, deleteTenantChatsSQL):
				chatsAt = i
			}
		}
		if messagesAt < 0 || chatsAt < 0 || messagesAt > chatsAt {
			t.Fatalf("deletes ran at (%d, %d), want messages before chats", messagesAt, chatsAt)
		}
	})

	t.Run("the messages delete fails", func(t *testing.T) {
		repo, srv := newScriptedRepository(t)
		srv.on(deleteTenantMessagesSQL, pgReply{err: "disk full"})

		_, _, err := repo.DeleteTenant(context.Background(), 11)
		if err == nil {
			t.Fatal("DeleteTenant returned no error although the messages delete failed")
		}
		if !strings.Contains(err.Error(), "deleting messages of tenant 11") {
			t.Fatalf("error %q does not name its stage and tenant", err)
		}
		if got := countQueriesWithPrefix(srv.normalizedQueries(), deleteTenantChatsSQL); got != 0 {
			t.Fatalf("the chats delete ran %d times after a failed messages delete", got)
		}
	})

	t.Run("the chats delete fails", func(t *testing.T) {
		repo, srv := newScriptedRepository(t)
		srv.on(deleteTenantMessagesSQL, pgReply{tag: "DELETE 5"})
		srv.on(deleteTenantChatsSQL, pgReply{err: "disk full"})

		_, _, err := repo.DeleteTenant(context.Background(), 11)
		if err == nil {
			t.Fatal("DeleteTenant returned no error although the chats delete failed")
		}
		if !strings.Contains(err.Error(), "deleting chat labels of tenant 11") {
			t.Fatalf("error %q does not name its stage and tenant", err)
		}
	})
}

// TestPurgeExpiredLoopsTenantByTenant pins the non-negotiable trap of
// repository.go: the retention purge is one InTenant transaction PER tenant
// with that tenant's own context and retention window, never one global
// DELETE. A tenant's failure returns what the tenants before it already
// purged, plus the error -- the caller decides what to log, but the count
// must not silently drop the work already done.
func TestPurgeExpiredLoopsTenantByTenant(t *testing.T) {
	t.Run("each tenant gets its own transaction and window", func(t *testing.T) {
		repo, srv := newScriptedRepository(t)
		srv.on(purgeTenant11SQL, pgReply{tag: "DELETE 3"})
		srv.on(purgeTenant12SQL, pgReply{tag: "DELETE 4"})

		tenants := []users.TenantRetention{
			{OwnerUserID: 11, RetentionDays: 30},
			{OwnerUserID: 12, RetentionDays: 365},
		}
		purged, err := repo.PurgeExpired(context.Background(), tenants)
		if err != nil {
			t.Fatalf("PurgeExpired: %v", err)
		}
		if purged != 7 {
			t.Fatalf("PurgeExpired = %d, want the 3+4 rows the database reports", purged)
		}

		queries := srv.normalizedQueries()
		wantSequence := []string{
			"BEGIN",
			tenantContextSQL(11),
			purgeTenant11SQL,
			"COMMIT",
			"BEGIN",
			tenantContextSQL(12),
			purgeTenant12SQL,
			"COMMIT",
		}
		if len(queries) != len(wantSequence) {
			t.Fatalf("queries = %v, want the sequence %v", queries, wantSequence)
		}
		for i, want := range wantSequence {
			if !strings.HasPrefix(queries[i], want) {
				t.Fatalf("query %d is %q, want it to start with %q", i, queries[i], want)
			}
		}
		// The retention window travels to the server clock per tenant.
		purge11 := firstQueryWithPrefix(t, queries, purgeTenant11SQL)
		if !strings.Contains(purge11, "MAKE_INTERVAL(DAYS => '30')") {
			t.Fatalf("tenant 11's purge %q does not carry its 30-day window", purge11)
		}
		purge12 := firstQueryWithPrefix(t, queries, purgeTenant12SQL)
		if !strings.Contains(purge12, "MAKE_INTERVAL(DAYS => '365')") {
			t.Fatalf("tenant 12's purge %q does not carry its 365-day window", purge12)
		}
	})

	t.Run("a tenant's failure keeps the work already done", func(t *testing.T) {
		repo, srv := newScriptedRepository(t)
		srv.on(purgeTenant11SQL, pgReply{tag: "DELETE 3"})
		srv.on(purgeTenant12SQL, pgReply{err: "lock timeout"})

		tenants := []users.TenantRetention{
			{OwnerUserID: 11, RetentionDays: 30},
			{OwnerUserID: 12, RetentionDays: 365},
		}
		purged, err := repo.PurgeExpired(context.Background(), tenants)
		if err == nil {
			t.Fatal("PurgeExpired returned no error although tenant 12 failed")
		}
		if purged != 3 {
			t.Fatalf("PurgeExpired = %d on failure, want the 3 rows tenant 11 already purged", purged)
		}
		if !strings.Contains(err.Error(), "purge tenant 12") {
			t.Fatalf("error %q does not name the failing tenant", err)
		}
	})

	t.Run("no tenants means no query at all", func(t *testing.T) {
		repo, srv := newScriptedRepository(t)

		purged, err := repo.PurgeExpired(context.Background(), nil)
		if err != nil {
			t.Fatalf("PurgeExpired(nil): %v", err)
		}
		if purged != 0 {
			t.Fatalf("PurgeExpired(nil) = %d, want 0", purged)
		}
		if queries := srv.normalizedQueries(); len(queries) != 0 {
			t.Fatalf("PurgeExpired(nil) ran %v", queries)
		}
	})
}

func pgInt64(v int64) *int64 { return &v }

func containsExact(queries []string, want string) bool {
	for _, q := range queries {
		if q == want {
			return true
		}
	}
	return false
}
