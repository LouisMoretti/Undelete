package media

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/LouisMoretti/Undelete/bot/internal/storage"
)

// This file covers repository.go, the SQL half of the package, WITHOUT a
// database: a scripted fake speaks enough of the PostgreSQL wire protocol for
// the real pgxpool to connect, and storage.DB.InTenant runs its real
// BEGIN / set_config / COMMIT sequence against it. What the fake then observes
// is exactly what the database would see: the statements, their order, and
// their inlined parameters. The semantic half (RLS actually filtering, upserts
// actually converging, the server clock, CHECK constraints firing) stays the
// integration tests'. What only a fake can pin is that every statement travels
// inside an InTenant transaction opened for THIS owner, that MarkStored
// validates its inputs before touching the database, that the single-row
// updates fail closed on "no row affected" (the RLS trap), and that every
// batched DELETE and cursor-paginated SELECT keeps its bound.
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
// message id batch as an array literal.
const (
	mediaInsertSQL  = "INSERT INTO MEDIA_FILES"
	countByOwnerSQL = "SELECT COUNT(*) FROM MEDIA_FILES"
	sumStoredSQL    = "SELECT COALESCE(SUM(BYTE_SIZE), 0) FROM MEDIA_FILES"
	markStoredSQL   = "UPDATE MEDIA_FILES SET STATUS = 'STORED'"
	markPurgedSQL   = "UPDATE MEDIA_FILES SET STATUS = 'PURGED'"
	markRetrySQL    = "UPDATE MEDIA_FILES SET STATUS = 'PENDING'"
	knownPathsSQL   = "SELECT RELATIVE_PATH FROM MEDIA_FILES"
	albumAnchorsSQL = "SELECT MEDIA_GROUP_ID, MIN(MESSAGE_ID)"

	deleteTenantSQL = "DELETE FROM MEDIA_FILES WHERE ID IN (SELECT ID FROM MEDIA_FILES ORDER BY ID"
	deleteStaleSQL  = "DELETE FROM MEDIA_FILES WHERE ID IN (SELECT ID FROM MEDIA_FILES WHERE STATUS = 'PENDING'"
	deletePurgedSQL = "DELETE FROM MEDIA_FILES WHERE ID IN (SELECT ID FROM MEDIA_FILES WHERE STATUS = 'PURGED'"

	// catalogueSQL is the prefix every catalogue read (selectColumns) starts
	// with -- short on purpose: the full twenty-column list is pinned by the
	// scan assertions, not by the rule key.
	catalogueSQL = "SELECT ID, BUSINESS_CONNECTION_ID, CHAT_ID, MESSAGE_ID, FILE_INDEX"
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
// the error surfaces through rows.Err() -- the path scanFiles reports.
type pgReply struct {
	columns      []pgColumn
	rows         [][]*string
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
// storage.DB.InTenant, plus the DB itself: SelectStoredTx and
// SelectAlbumAnchorsTx take the caller's transaction, exactly the way
// messages.MarkDeleted hands them its own. The pool is lazy: nothing connects
// until the first query, and the whole server is torn down with the test.
func newScriptedRepository(t *testing.T) (*Repository, *storage.DB, *scriptedPG) {
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
// how a nullable column (byte_size, width, ...) comes back unset.
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

// assertEveryQueryRanInsideInTenant walks the recorded stream and enforces the
// package's one structural rule: every statement reaches the database inside a
// transaction that opened with app.current_owner_user_id set to THIS owner,
// LOCAL to that transaction. A query sneaking past InTenant would run with no
// tenant context, which RLS answers with zero rows -- fail-closed, but
// silently wrong.
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

// catalogueColumns describes the row every catalogue read shares
// (media.selectColumns): twenty columns in the scan order media.scanFiles
// expects.
func catalogueColumns() []pgColumn {
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

// catalogueRow builds one full catalogue row: an attachment of tenant 11, chat
// 33, message 501, on disk, belonging to the given album ("" = lone media).
func catalogueRow(id string) []*string {
	return []*string{
		pgStr(id), pgStr("BC-1"), pgStr("33"), pgStr("501"), pgStr("0"),
		pgStr("tg-" + id), pgStr("uniq-" + id), pgStr("photo"),
		pgStr("image/jpeg"), pgStr("pic" + id + ".jpg"),
		pgStr("1024"), pgStr("800"), pgStr("600"), pgStr("30"),
		pgStr("11/a/pic" + id + ".jpg"), pgStr("11/a/pic" + id + "_thumb.jpg"),
		pgStr("sha-" + id), pgStr("stored"), pgStr("album-1"),
		pgStr("2026-09-01 10:00:00+00"),
	}
}

// fullRecord is one attachment with every optional field present.
func fullRecord() Record {
	byteSize := int64(1024)
	width, height, duration := 800, 600, 30
	return Record{
		BusinessConnectionID: "BC-1",
		ChatID:               33,
		MessageID:            501,
		FileIndex:            0,
		TelegramFileID:       "tg-1",
		TelegramFileUniqueID: "uniq-1",
		MediaType:            TypePhoto,
		MimeType:             "image/jpeg",
		FileName:             "pic.jpg",
		ByteSize:             &byteSize,
		Width:                &width,
		Height:               &height,
		DurationSec:          &duration,
		MediaGroupID:         "album-1",
	}
}

// TestSaveUpsertsInsideOneTenantTransaction pins Save on the wire: one InTenant
// transaction, the anti-collision key in the ON CONFLICT, and the upsert
// deliberately NOT resetting the on-disk facts (status, paths, hash) -- a
// redelivery must never orphan an already downloaded blob.
func TestSaveUpsertsInsideOneTenantTransaction(t *testing.T) {
	repo, _, srv := newScriptedRepository(t)
	srv.on(mediaInsertSQL, pgReply{
		columns: []pgColumn{{name: "id", oid: oidInt8}},
		rows:    [][]*string{pgRow("77")},
	})

	id, err := repo.Save(context.Background(), 11, fullRecord())
	if err != nil {
		t.Fatalf("Save: %v", err)
	}
	if id != 77 {
		t.Fatalf("Save returned id %d, want 77", id)
	}

	queries := srv.normalizedQueries()
	if len(queries) != 4 {
		t.Fatalf("queries = %v, want begin, context, upsert, commit", queries)
	}
	assertEveryQueryRanInsideInTenant(t, queries, 11)

	upsert := firstQueryWithPrefix(t, queries, mediaInsertSQL)
	for _, want := range []string{
		"VALUES ('11', 'BC-1', '33', '501', '0', 'TG-1', 'UNIQ-1', 'PHOTO', NULLIF('IMAGE/JPEG', ''), NULLIF('PIC.JPG', ''), '1024', '800', '600', '30', NULLIF('ALBUM-1', ''))",
		"ON CONFLICT (OWNER_USER_ID, BUSINESS_CONNECTION_ID, CHAT_ID, MESSAGE_ID, FILE_INDEX)",
		"TELEGRAM_FILE_ID = EXCLUDED.TELEGRAM_FILE_ID",
		"COALESCE(EXCLUDED.MIME_TYPE, MEDIA_FILES.MIME_TYPE)",
		"BYTE_SIZE = CASE WHEN MEDIA_FILES.STATUS = 'STORED' THEN MEDIA_FILES.BYTE_SIZE ELSE COALESCE(EXCLUDED.BYTE_SIZE, MEDIA_FILES.BYTE_SIZE) END",
	} {
		if !strings.Contains(upsert, want) {
			t.Fatalf("the upsert %q does not carry %q", upsert, want)
		}
	}
	// The orphan-blob protection: the upsert must not reset the disk facts to
	// what this delivery claims -- only the Telegram-side identity refreshes.
	for _, forbidden := range []string{
		"STATUS = EXCLUDED",
		"RELATIVE_PATH = EXCLUDED",
		"SHA256 = EXCLUDED",
		"THUMBNAIL_RELATIVE_PATH = EXCLUDED",
	} {
		if strings.Contains(upsert, forbidden) {
			t.Fatalf("the upsert resets %q, which describes the file on disk: %q", forbidden, upsert)
		}
	}
}

// TestSaveFailureSurfaces: a database that cannot record the attachment must
// produce an error naming the stage, and the transaction must roll back.
func TestSaveFailureSurfaces(t *testing.T) {
	repo, _, srv := newScriptedRepository(t)
	srv.on(mediaInsertSQL, pgReply{err: "disk full"})

	if _, err := repo.Save(context.Background(), 11, fullRecord()); err == nil {
		t.Fatal("Save returned no error although the upsert failed")
	} else if !strings.Contains(err.Error(), "media upsert") {
		t.Fatalf("error %q does not name its stage", err)
	}

	queries := srv.normalizedQueries()
	if len(queries) == 0 || queries[len(queries)-1] != "ROLLBACK" {
		t.Fatalf("a failed Save must end on ROLLBACK, got %v", queries)
	}
}

// TestQuotaCountsReadTheTenantCatalogue pins the two quota seeds: CountByOwner
// counts every row of the tenant, SumStoredBytes only the stored ones.
func TestQuotaCountsReadTheTenantCatalogue(t *testing.T) {
	t.Run("CountByOwner counts whatever the status", func(t *testing.T) {
		repo, _, srv := newScriptedRepository(t)
		srv.on(countByOwnerSQL, pgReply{
			columns: []pgColumn{{name: "count", oid: oidInt8}},
			rows:    [][]*string{pgRow("5")},
		})

		count, err := repo.CountByOwner(context.Background(), 11)
		if err != nil {
			t.Fatalf("CountByOwner: %v", err)
		}
		if count != 5 {
			t.Fatalf("CountByOwner = %d, want 5", count)
		}
		queries := srv.normalizedQueries()
		assertEveryQueryRanInsideInTenant(t, queries, 11)
		if got := firstQueryWithPrefix(t, queries, countByOwnerSQL); !strings.Contains(got, "WHERE OWNER_USER_ID = '11'") {
			t.Fatalf("CountByOwner %q is not scoped to the tenant", got)
		}
	})

	t.Run("SumStoredBytes sums the stored rows only", func(t *testing.T) {
		repo, _, srv := newScriptedRepository(t)
		srv.on(sumStoredSQL, pgReply{
			columns: []pgColumn{{name: "coalesce", oid: oidInt8}},
			rows:    [][]*string{pgRow("4096")},
		})

		total, err := repo.SumStoredBytes(context.Background(), 11)
		if err != nil {
			t.Fatalf("SumStoredBytes: %v", err)
		}
		if total != 4096 {
			t.Fatalf("SumStoredBytes = %d, want 4096", total)
		}
		queries := srv.normalizedQueries()
		assertEveryQueryRanInsideInTenant(t, queries, 11)
		got := firstQueryWithPrefix(t, queries, sumStoredSQL)
		for _, want := range []string{"WHERE OWNER_USER_ID = '11'", "STATUS = 'STORED'"} {
			if !strings.Contains(got, want) {
				t.Fatalf("SumStoredBytes %q does not carry %q", got, want)
			}
		}
	})
}

// TestGetByMessageScansTheCatalogueRow pins the twenty-column scan: COALESCE'd
// columns come back as "", NULL optionals as unset pointers, the timestamp
// parses, and the read is ordered by file_index so an album rebuilds in the
// order it was sent.
func TestGetByMessageScansTheCatalogueRow(t *testing.T) {
	repo, _, srv := newScriptedRepository(t)
	// A photo reports no duration: the NULL path of the optional columns.
	row := catalogueRow("9")
	row[13] = nil
	srv.on(catalogueSQL, pgReply{
		columns: catalogueColumns(),
		rows:    [][]*string{row},
	})

	files, err := repo.GetByMessage(context.Background(), 11, "BC-1", 33, 501)
	if err != nil {
		t.Fatalf("GetByMessage: %v", err)
	}
	if len(files) != 1 {
		t.Fatalf("GetByMessage returned %d files, want 1", len(files))
	}
	f := files[0]
	if f.ID != 9 || f.BusinessConnectionID != "BC-1" || f.ChatID != 33 || f.MessageID != 501 || f.FileIndex != 0 {
		t.Fatalf("identity fields scanned wrong: %+v", f)
	}
	if f.MediaType != TypePhoto || f.MimeType != "image/jpeg" || f.FileName != "pic9.jpg" {
		t.Fatalf("description fields scanned wrong: %+v", f)
	}
	if f.ByteSize == nil || *f.ByteSize != 1024 {
		t.Fatalf("byte_size scanned wrong: %v", f.ByteSize)
	}
	if f.Width == nil || *f.Width != 800 || f.Height == nil || *f.Height != 600 {
		t.Fatalf("dimensions scanned wrong: %+v", f)
	}
	if f.DurationSec != nil {
		t.Fatalf("a NULL duration must scan as an unset pointer: %+v", f)
	}
	if f.RelativePath != "11/a/pic9.jpg" || f.ThumbnailRelativePath != "11/a/pic9_thumb.jpg" {
		t.Fatalf("paths scanned wrong: %+v", f)
	}
	if f.SHA256 != "sha-9" || f.Status != StatusStored || f.MediaGroupID != "album-1" {
		t.Fatalf("status fields scanned wrong: %+v", f)
	}
	if f.CreatedAt.IsZero() || f.CreatedAt.UTC().Year() != 2026 {
		t.Fatalf("created_at scanned wrong: %v", f.CreatedAt)
	}

	queries := srv.normalizedQueries()
	assertEveryQueryRanInsideInTenant(t, queries, 11)
	got := firstQueryWithPrefix(t, queries, catalogueSQL)
	for _, want := range []string{
		"WHERE BUSINESS_CONNECTION_ID = 'BC-1' AND CHAT_ID = '33' AND MESSAGE_ID = '501'",
		"ORDER BY FILE_INDEX",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("GetByMessage %q does not carry %q", got, want)
		}
	}
}

// TestCatalogueReadBrokenStream: an error arriving AFTER the DataRows must
// surface through rows.Err() -- scanFiles reports it, the caller's error names
// the stage.
func TestCatalogueReadBrokenStream(t *testing.T) {
	repo, _, srv := newScriptedRepository(t)
	srv.on(catalogueSQL, pgReply{
		columns:      catalogueColumns(),
		rows:         [][]*string{catalogueRow("9")},
		errAfterRows: "connection reset",
	})

	if _, err := repo.GetByMessage(context.Background(), 11, "BC-1", 33, 501); err == nil {
		t.Fatal("GetByMessage returned no error although the stream broke after the rows")
	} else if !strings.Contains(err.Error(), "reading media of message") {
		t.Fatalf("error %q does not name its stage", err)
	}
}

// TestSelectStoredAndAlbumAnchorsRunOnTheCallerTransaction pins the contract
// the deletion alert depends on: the two catalogue reads take the caller's
// transaction, so they add NO BEGIN/COMMIT of their own -- the media entry is
// enqueued atomically with the deleted_at it belongs to. SelectStoredTx keeps
// only the rows that are on disk; the anchors are status-blind and batch-fixed
// so a redelivered deletion yields the same MIN.
func TestSelectStoredAndAlbumAnchorsRunOnTheCallerTransaction(t *testing.T) {
	_, db, srv := newScriptedRepository(t)
	srv.on(catalogueSQL, pgReply{
		columns: catalogueColumns(),
		rows:    [][]*string{catalogueRow("9")},
	})
	srv.on(albumAnchorsSQL, pgReply{
		columns: []pgColumn{{name: "media_group_id", oid: oidText}, {name: "min", oid: oidInt8}},
		rows:    [][]*string{pgRow("album-1", "501")},
	})

	var stored []File
	var anchors map[string]int64
	err := db.InTenant(context.Background(), 11, func(tx pgx.Tx) error {
		var err error
		stored, err = SelectStoredTx(context.Background(), tx, "BC-1", 33, []int64{501, 502})
		if err != nil {
			return err
		}
		anchors, err = SelectAlbumAnchorsTx(context.Background(), tx, "BC-1", 33, []int64{501, 502})
		return err
	})
	if err != nil {
		t.Fatalf("caller transaction: %v", err)
	}
	if len(stored) != 1 || stored[0].ID != 9 || stored[0].Status != StatusStored {
		t.Fatalf("SelectStoredTx = %+v, want the one stored row", stored)
	}
	if len(anchors) != 1 || anchors["album-1"] != 501 {
		t.Fatalf("SelectAlbumAnchorsTx = %v, want album-1 -> 501", anchors)
	}

	// One transaction for the whole callback: begin, context, the two reads,
	// commit -- nothing else.
	queries := srv.normalizedQueries()
	if len(queries) != 5 {
		t.Fatalf("queries = %v, want begin, context, stored read, anchor read, commit", queries)
	}
	assertEveryQueryRanInsideInTenant(t, queries, 11)

	storedRead := firstQueryWithPrefix(t, queries, catalogueSQL)
	for _, want := range []string{
		"WHERE BUSINESS_CONNECTION_ID = 'BC-1'",
		"MESSAGE_ID = ANY('{501,502}')",
		"STATUS = 'STORED'",
		"RELATIVE_PATH IS NOT NULL",
		"ORDER BY MESSAGE_ID, FILE_INDEX",
	} {
		if !strings.Contains(storedRead, want) {
			t.Fatalf("SelectStoredTx %q does not carry %q", storedRead, want)
		}
	}
	anchorRead := firstQueryWithPrefix(t, queries, albumAnchorsSQL)
	for _, want := range []string{
		"MESSAGE_ID = ANY('{501,502}')",
		"MEDIA_GROUP_ID IS NOT NULL",
		"GROUP BY MEDIA_GROUP_ID",
	} {
		if !strings.Contains(anchorRead, want) {
			t.Fatalf("SelectAlbumAnchorsTx %q does not carry %q", anchorRead, want)
		}
	}
}

// TestMarkStoredValidatesBeforeTouchingTheDatabase pins the ordering contract
// of MarkStored: the paths and the hash are validated BEFORE the query, so a
// bug in the generator is refused before anything reaches the wire.
func TestMarkStoredValidatesBeforeTouchingTheDatabase(t *testing.T) {
	validHash := strings.Repeat("ab", 32)
	for _, tc := range []struct {
		name string
		file StoredFile
	}{
		{"empty path", StoredFile{SHA256: validHash}},
		{"escaping path", StoredFile{RelativePath: "../escape.jpg", SHA256: validHash}},
		{"absolute path", StoredFile{RelativePath: "/etc/passwd", SHA256: validHash}},
		{"backslash in path", StoredFile{RelativePath: `11\a\pic.jpg`, SHA256: validHash}},
		{"escaping thumbnail", StoredFile{RelativePath: "11/a/pic.jpg", ThumbnailRelativePath: "../t.jpg", SHA256: validHash}},
		{"short hash", StoredFile{RelativePath: "11/a/pic.jpg", SHA256: "abcd"}},
		{"uppercase hash", StoredFile{RelativePath: "11/a/pic.jpg", SHA256: strings.Repeat("AB", 32)}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			repo, _, srv := newScriptedRepository(t)
			err := repo.MarkStored(context.Background(), 11, 9, tc.file)
			if err == nil {
				t.Fatal("MarkStored accepted an invalid input")
			}
			if queries := srv.normalizedQueries(); len(queries) != 0 {
				t.Fatalf("an invalid input must not reach the database, got %v", queries)
			}
		})
	}
}

// TestSingleRowUpdatesRunInsideTenantAndFailClosed pins the three status
// updates on the wire: they travel inside the tenant's transaction, and "no
// row affected" is ErrNotFound -- a row of ANOTHER tenant is invisible, and
// silently succeeding against it is the fail-closed trap.
func TestSingleRowUpdatesRunInsideTenantAndFailClosed(t *testing.T) {
	validHash := strings.Repeat("ab", 32)
	for _, tc := range []struct {
		name    string
		prefix  string
		call    func(*Repository) error
		wantSQL []string
	}{
		{
			name:   "MarkStored",
			prefix: markStoredSQL,
			call: func(r *Repository) error {
				return r.MarkStored(context.Background(), 11, 9, StoredFile{
					RelativePath: "11/a/pic.jpg", ThumbnailRelativePath: "11/a/t.jpg",
					SHA256: validHash, ByteSize: 1024,
				})
			},
			wantSQL: []string{
				"SET STATUS = 'STORED', RELATIVE_PATH = '11/A/PIC.JPG', THUMBNAIL_RELATIVE_PATH = NULLIF('11/A/T.JPG', ''), SHA256 = '" + strings.Repeat("AB", 32) + "', BYTE_SIZE = '1024'",
				"WHERE ID = '9'",
			},
		},
		{
			name:   "MarkPurged",
			prefix: markPurgedSQL,
			call: func(r *Repository) error {
				return r.MarkPurged(context.Background(), 11, 9)
			},
			wantSQL: []string{
				"SET STATUS = 'PURGED', RELATIVE_PATH = NULL, THUMBNAIL_RELATIVE_PATH = NULL",
				"WHERE ID = '9'",
			},
		},
		{
			name:   "MarkPendingRetry",
			prefix: markRetrySQL,
			call: func(r *Repository) error {
				return r.MarkPendingRetry(context.Background(), 11, 9)
			},
			wantSQL: []string{
				"SET STATUS = 'PENDING', RELATIVE_PATH = NULL, THUMBNAIL_RELATIVE_PATH = NULL, SHA256 = NULL",
				"WHERE ID = '9'",
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Run("happy path", func(t *testing.T) {
				repo, _, srv := newScriptedRepository(t)
				srv.on(tc.prefix, pgReply{tag: "UPDATE 1"})

				if err := tc.call(repo); err != nil {
					t.Fatalf("%s: %v", tc.name, err)
				}
				queries := srv.normalizedQueries()
				assertEveryQueryRanInsideInTenant(t, queries, 11)
				got := firstQueryWithPrefix(t, queries, tc.prefix)
				for _, want := range tc.wantSQL {
					if !strings.Contains(got, want) {
						t.Fatalf("%s %q does not carry %q", tc.name, got, want)
					}
				}
			})

			t.Run("no row affected fails closed", func(t *testing.T) {
				repo, _, srv := newScriptedRepository(t)
				srv.on(tc.prefix, pgReply{tag: "UPDATE 0"})

				err := tc.call(repo)
				if !errors.Is(err, ErrNotFound) {
					t.Fatalf("%s on UPDATE 0 returned %v, want ErrNotFound", tc.name, err)
				}
			})
		})
	}
}

// TestListQueriesPaginateByCursorAndStatus pins the four catalogue listings:
// the download queue is pending and oldest-first, the retention sweep is
// cursor-bounded on id and gated on retention elapsed, and the erasure page is
// status-blind.
func TestListQueriesPaginateByCursorAndStatus(t *testing.T) {
	listReply := pgReply{columns: catalogueColumns(), rows: [][]*string{catalogueRow("9")}}

	t.Run("ListPending is pending and oldest first", func(t *testing.T) {
		repo, _, srv := newScriptedRepository(t)
		srv.on(catalogueSQL, listReply)

		if _, err := repo.ListPending(context.Background(), 11, 10); err != nil {
			t.Fatalf("ListPending: %v", err)
		}
		queries := srv.normalizedQueries()
		assertEveryQueryRanInsideInTenant(t, queries, 11)
		got := firstQueryWithPrefix(t, queries, catalogueSQL)
		for _, want := range []string{"WHERE STATUS = 'PENDING'", "ORDER BY ID", "LIMIT '10'"} {
			if !strings.Contains(got, want) {
				t.Fatalf("ListPending %q does not carry %q", got, want)
			}
		}
	})

	t.Run("ListExpiredStored is cursor-bounded and retention-gated", func(t *testing.T) {
		repo, _, srv := newScriptedRepository(t)
		srv.on(catalogueSQL, listReply)

		if _, err := repo.ListExpiredStored(context.Background(), 11, 100, 30, 50); err != nil {
			t.Fatalf("ListExpiredStored: %v", err)
		}
		queries := srv.normalizedQueries()
		assertEveryQueryRanInsideInTenant(t, queries, 11)
		got := firstQueryWithPrefix(t, queries, catalogueSQL)
		for _, want := range []string{
			"WHERE STATUS = 'STORED' AND ID > '100'",
			"CREATED_AT < NOW() - MAKE_INTERVAL(DAYS => '30')",
			"ORDER BY ID",
			"LIMIT '50'",
		} {
			if !strings.Contains(got, want) {
				t.Fatalf("ListExpiredStored %q does not carry %q", got, want)
			}
		}
	})

	t.Run("ListStoredPage is cursor-paginated over stored rows", func(t *testing.T) {
		repo, _, srv := newScriptedRepository(t)
		srv.on(catalogueSQL, listReply)

		if _, err := repo.ListStoredPage(context.Background(), 11, 100, 50); err != nil {
			t.Fatalf("ListStoredPage: %v", err)
		}
		queries := srv.normalizedQueries()
		assertEveryQueryRanInsideInTenant(t, queries, 11)
		got := firstQueryWithPrefix(t, queries, catalogueSQL)
		for _, want := range []string{"WHERE STATUS = 'STORED' AND ID > '100' ORDER BY ID", "LIMIT '50'"} {
			if !strings.Contains(got, want) {
				t.Fatalf("ListStoredPage %q does not carry %q", got, want)
			}
		}
	})

	t.Run("ListTenantPage is status-blind", func(t *testing.T) {
		repo, _, srv := newScriptedRepository(t)
		srv.on(catalogueSQL, listReply)

		if _, err := repo.ListTenantPage(context.Background(), 11, 100, 50); err != nil {
			t.Fatalf("ListTenantPage: %v", err)
		}
		queries := srv.normalizedQueries()
		assertEveryQueryRanInsideInTenant(t, queries, 11)
		got := firstQueryWithPrefix(t, queries, catalogueSQL)
		if !strings.Contains(got, "WHERE ID > '100' ORDER BY ID") {
			t.Fatalf("ListTenantPage %q is not the plain keyset page", got)
		}
		if strings.Contains(got, "WHERE STATUS") || strings.Contains(got, "AND STATUS") {
			t.Fatalf("ListTenantPage %q filters on status, but an erasure must visit every row", got)
		}
	})
}

// TestKnownPathsUnionsFilesAndThumbnails: the reconciliation question "does
// anything still point at this path?" reads both columns, in one statement,
// inside the tenant.
func TestKnownPathsUnionsFilesAndThumbnails(t *testing.T) {
	repo, _, srv := newScriptedRepository(t)
	srv.on(knownPathsSQL, pgReply{
		columns: []pgColumn{{name: "relative_path", oid: oidText}},
		rows:    [][]*string{pgRow("11/a/pic9.jpg"), pgRow("11/a/pic9_thumb.jpg")},
	})

	known, err := repo.KnownPaths(context.Background(), 11, []string{"11/a/pic9.jpg", "11/a/pic9_thumb.jpg", "11/a/gone.jpg"})
	if err != nil {
		t.Fatalf("KnownPaths: %v", err)
	}
	if len(known) != 2 {
		t.Fatalf("KnownPaths = %v, want the two referenced paths", known)
	}
	if _, ok := known["11/a/pic9.jpg"]; !ok {
		t.Fatalf("KnownPaths = %v, want 11/a/pic9.jpg", known)
	}
	if _, ok := known["11/a/pic9_thumb.jpg"]; !ok {
		t.Fatalf("KnownPaths = %v, want the thumbnail too", known)
	}

	queries := srv.normalizedQueries()
	assertEveryQueryRanInsideInTenant(t, queries, 11)
	got := firstQueryWithPrefix(t, queries, knownPathsSQL)
	for _, want := range []string{
		"WHERE RELATIVE_PATH = ANY",
		"UNION",
		"SELECT THUMBNAIL_RELATIVE_PATH FROM MEDIA_FILES WHERE THUMBNAIL_RELATIVE_PATH = ANY",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("KnownPaths %q does not carry %q", got, want)
		}
	}
}

// TestKnownPathsEmptyShortCircuits: no batch, no query -- the caller's scan
// found nothing, and the reconciliation must not round-trip an empty IN.
func TestKnownPathsEmptyShortCircuits(t *testing.T) {
	repo, _, srv := newScriptedRepository(t)

	known, err := repo.KnownPaths(context.Background(), 11, nil)
	if err != nil {
		t.Fatalf("KnownPaths: %v", err)
	}
	if len(known) != 0 {
		t.Fatalf("KnownPaths = %v, want empty", known)
	}
	if queries := srv.normalizedQueries(); len(queries) != 0 {
		t.Fatalf("an empty batch must not reach the database, got %v", queries)
	}
}

// TestDeleteTenantBatchIsBounded pins the erasure row deletion: batched by a
// subselect with LIMIT, inside the tenant, returning how many went.
func TestDeleteTenantBatchIsBounded(t *testing.T) {
	repo, _, srv := newScriptedRepository(t)
	srv.on(deleteTenantSQL, pgReply{tag: "DELETE 3"})

	deleted, err := repo.DeleteTenantBatch(context.Background(), 11, 50)
	if err != nil {
		t.Fatalf("DeleteTenantBatch: %v", err)
	}
	if deleted != 3 {
		t.Fatalf("DeleteTenantBatch = %d, want 3", deleted)
	}
	queries := srv.normalizedQueries()
	assertEveryQueryRanInsideInTenant(t, queries, 11)
	got := firstQueryWithPrefix(t, queries, deleteTenantSQL)
	if !strings.Contains(got, "ORDER BY ID LIMIT '50'") {
		t.Fatalf("DeleteTenantBatch %q is not bounded", got)
	}
}

// TestDeleteStalePendingKeepsBothDeadlines pins the two independent deadlines
// of the stale-pending sweep: retention is absolute (created_at), staleness
// resets on a requeue (created_at AND updated_at).
func TestDeleteStalePendingKeepsBothDeadlines(t *testing.T) {
	repo, _, srv := newScriptedRepository(t)
	srv.on(deleteStaleSQL, pgReply{tag: "DELETE 2"})

	deleted, err := repo.DeleteStalePending(context.Background(), 11, 24*time.Hour, 14, 100)
	if err != nil {
		t.Fatalf("DeleteStalePending: %v", err)
	}
	if deleted != 2 {
		t.Fatalf("DeleteStalePending = %d, want 2", deleted)
	}
	queries := srv.normalizedQueries()
	assertEveryQueryRanInsideInTenant(t, queries, 11)
	got := firstQueryWithPrefix(t, queries, deleteStaleSQL)
	for _, want := range []string{
		"WHERE STATUS = 'PENDING'",
		"CREATED_AT < NOW() - MAKE_INTERVAL(DAYS => '14')",
		"CREATED_AT < NOW() - MAKE_INTERVAL(SECS => '86400')",
		"AND UPDATED_AT < NOW() - MAKE_INTERVAL(SECS => '86400')",
		"LIMIT '100'",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("DeleteStalePending %q does not carry %q", got, want)
		}
	}
}

// TestDeletePurgedGatesOnRetentionAndGrace pins the purged-row deletion: a
// purged row is the last trace of an attachment, so retention gates it and
// grace only adds a margin counted from updated_at.
func TestDeletePurgedGatesOnRetentionAndGrace(t *testing.T) {
	repo, _, srv := newScriptedRepository(t)
	srv.on(deletePurgedSQL, pgReply{tag: "DELETE 1"})

	deleted, err := repo.DeletePurged(context.Background(), 11, time.Hour, 14, 100)
	if err != nil {
		t.Fatalf("DeletePurged: %v", err)
	}
	if deleted != 1 {
		t.Fatalf("DeletePurged = %d, want 1", deleted)
	}
	queries := srv.normalizedQueries()
	assertEveryQueryRanInsideInTenant(t, queries, 11)
	got := firstQueryWithPrefix(t, queries, deletePurgedSQL)
	for _, want := range []string{
		"WHERE STATUS = 'PURGED'",
		"CREATED_AT < NOW() - MAKE_INTERVAL(DAYS => '14')",
		"UPDATED_AT < NOW() - MAKE_INTERVAL(SECS => '3600')",
		"LIMIT '100'",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("DeletePurged %q does not carry %q", got, want)
		}
	}
}
