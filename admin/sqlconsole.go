package main

import (
	"bufio"
	"compress/gzip"
	"context"
	"database/sql"
	"encoding/hex"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

// The SQL console (GET/POST /sql) and the database dump (GET /sql/dump).
//
// This is the one admin surface that can step outside the admin/viewer write
// split (see ../CLAUDE.md): with "allow writes" ticked it will happily UPDATE
// users.plan or payment_invoices, which nothing else in this service may touch.
// So writes are opt-in per statement, and the default is a READ ONLY
// transaction — MySQL itself refuses the write, rather than a keyword filter
// here that a cleverly written statement could slip past.

const (
	// sqlQueryTimeout bounds one console statement. The dump has no such bound:
	// it runs as long as the download does.
	sqlQueryTimeout = 60 * time.Second
	// sqlMaxRows caps how many result rows the console renders.
	sqlMaxRows = 1000
	// sqlMaxCellRunes clips long text cells for display (the dump never clips).
	sqlMaxCellRunes = 300
	// dumpBatchBytes is roughly how large one multi-row INSERT grows before it
	// is terminated, far under MySQL's default 64 MB max_allowed_packet.
	dumpBatchBytes = 1 << 20
)

// sqlTable is one row of the console's table sidebar and the dump's picker.
// Rows is information_schema's estimate, not a COUNT(*).
type sqlTable struct {
	Name  string
	Rows  int64
	Bytes int64
}

type sqlCell struct {
	Text    string
	Null    bool
	Blob    bool // Text is a size label, not the value
	Clipped bool
}

type sqlResult struct {
	Columns   []string
	Rows      [][]sqlCell
	Truncated bool
	IsExec    bool // a statement without a result set: show Affected/LastID
	Affected  int64
	LastID    int64
	Committed bool
	Elapsed   string
	Err       string
}

type sqlPage struct {
	Database string
	Tables   []sqlTable
	SQL      string
	Write    bool
	MaxRows  int
	Result   *sqlResult
	Error    string // failure to load the page itself (the table list)
}

func (s *Server) handleSQLPage(w http.ResponseWriter, r *http.Request) {
	render(w, "sql.html", s.loadSQLPage(r.Context()))
}

func (s *Server) handleSQLQuery(w http.ResponseWriter, r *http.Request) {
	if !sameOriginPost(r) {
		http.Error(w, "cross-origin request refused", http.StatusForbidden)
		return
	}
	page := s.loadSQLPage(r.Context())
	page.SQL = r.FormValue("sql")
	page.Write = r.FormValue("write") == "1"
	if strings.TrimSpace(page.SQL) != "" {
		page.Result = s.runSQL(r.Context(), page.SQL, page.Write)
	}
	render(w, "sql.html", page)
}

// sameOriginPost guards the console's POST against cross-site form
// submission. Basic-auth credentials are replayed by the browser on any
// request to this origin, including a form another site auto-submits, which
// with "allow writes" ticked would be arbitrary SQL. Browsers send
// Sec-Fetch-Site on every request (and Origin on every POST); a non-browser
// client such as curl sends neither and has no ambient credentials to abuse.
func sameOriginPost(r *http.Request) bool {
	if site := r.Header.Get("Sec-Fetch-Site"); site != "" && site != "same-origin" && site != "none" {
		return false
	}
	if origin := r.Header.Get("Origin"); origin != "" {
		u, err := url.Parse(origin)
		if err != nil || u.Host != r.Host {
			return false
		}
	}
	return true
}

func (s *Server) loadSQLPage(ctx context.Context) sqlPage {
	page := sqlPage{MaxRows: sqlMaxRows}
	if err := s.store.db.QueryRowContext(ctx, `SELECT COALESCE(DATABASE(), '')`).Scan(&page.Database); err != nil {
		page.Error = err.Error()
		return page
	}
	tables, err := s.listSQLTables(ctx)
	if err != nil {
		page.Error = err.Error()
	}
	page.Tables = tables
	return page
}

func (s *Server) listSQLTables(ctx context.Context) ([]sqlTable, error) {
	rows, err := s.store.db.QueryContext(ctx, `
		SELECT TABLE_NAME, COALESCE(TABLE_ROWS, 0), COALESCE(DATA_LENGTH, 0) + COALESCE(INDEX_LENGTH, 0)
		FROM information_schema.TABLES
		WHERE TABLE_SCHEMA = DATABASE() AND TABLE_TYPE = 'BASE TABLE'
		ORDER BY TABLE_NAME`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []sqlTable
	for rows.Next() {
		var t sqlTable
		if err := rows.Scan(&t.Name, &t.Rows, &t.Bytes); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// runSQL executes one statement inside a transaction: READ ONLY and rolled
// back unless write is set, in which case it is committed. (DDL commits
// implicitly in MySQL regardless — but in a READ ONLY transaction it is
// refused before it gets that far.)
func (s *Server) runSQL(ctx context.Context, query string, write bool) *sqlResult {
	mode := "read"
	if write {
		mode = "WRITE"
	}
	log.Printf("sql console [%s]: %s", mode, strings.Join(strings.Fields(query), " "))

	ctx, cancel := context.WithTimeout(ctx, sqlQueryTimeout)
	defer cancel()
	start := time.Now()
	res := &sqlResult{}
	defer func() { res.Elapsed = time.Since(start).Round(time.Millisecond).String() }()

	tx, err := s.store.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: !write})
	if err != nil {
		res.Err = err.Error()
		return res
	}
	defer tx.Rollback() // no-op after a commit

	if returnsRows(query) {
		err = scanResult(ctx, tx, query, res)
	} else {
		res.IsExec = true
		var r sql.Result
		if r, err = tx.ExecContext(ctx, query); err == nil {
			res.Affected, _ = r.RowsAffected()
			res.LastID, _ = r.LastInsertId()
		}
	}
	if err == nil && write {
		if err = tx.Commit(); err == nil {
			res.Committed = true
		}
	}
	if err != nil {
		res.Err = err.Error()
	}
	return res
}

// returnsRows reports whether a statement produces a result set, from its
// first keyword once leading comments are skipped. It only chooses between
// Query and Exec; it is not a safety check — the READ ONLY transaction is.
func returnsRows(query string) bool {
	q := query
	for {
		q = strings.TrimLeftFunc(q, unicode.IsSpace)
		switch {
		case strings.HasPrefix(q, "--"), strings.HasPrefix(q, "#"):
			if i := strings.IndexByte(q, '\n'); i >= 0 {
				q = q[i+1:]
				continue
			}
			return false
		case strings.HasPrefix(q, "/*"):
			if i := strings.Index(q, "*/"); i >= 0 {
				q = q[i+2:]
				continue
			}
			return false
		}
		break
	}
	if strings.HasPrefix(q, "(") { // (SELECT …) UNION (SELECT …)
		return true
	}
	end := strings.IndexFunc(q, func(r rune) bool { return !unicode.IsLetter(r) })
	if end < 0 {
		end = len(q)
	}
	switch strings.ToUpper(q[:end]) {
	case "SELECT", "SHOW", "DESC", "DESCRIBE", "EXPLAIN", "WITH", "TABLE", "VALUES":
		return true
	}
	return false
}

func scanResult(ctx context.Context, tx *sql.Tx, query string, res *sqlResult) error {
	rows, err := tx.QueryContext(ctx, query)
	if err != nil {
		return err
	}
	defer rows.Close()
	types, err := rows.ColumnTypes()
	if err != nil {
		return err
	}
	for _, t := range types {
		res.Columns = append(res.Columns, t.Name())
	}
	vals, ptrs := scanTargets(len(types))
	for rows.Next() {
		if len(res.Rows) == sqlMaxRows {
			res.Truncated = true
			break
		}
		if err := rows.Scan(ptrs...); err != nil {
			return err
		}
		row := make([]sqlCell, len(vals))
		for i, v := range vals {
			row[i] = displayCell(v, types[i].DatabaseTypeName())
		}
		res.Rows = append(res.Rows, row)
	}
	return rows.Err()
}

// scanTargets returns n `any` slots and the pointers rows.Scan fills them
// through. database/sql copies []byte into an *any, so values survive Next.
func scanTargets(n int) ([]any, []any) {
	vals := make([]any, n)
	ptrs := make([]any, n)
	for i := range vals {
		ptrs[i] = &vals[i]
	}
	return vals, ptrs
}

func displayCell(v any, dbType string) sqlCell {
	switch x := v.(type) {
	case nil:
		return sqlCell{Text: "NULL", Null: true}
	case []byte:
		if isBinaryType(dbType) {
			return sqlCell{Text: "BLOB " + filesize(int64(len(x))), Blob: true}
		}
		return clipCell(string(x))
	case time.Time:
		return sqlCell{Text: formatSQLTime(x, dbType)}
	default:
		return clipCell(fmt.Sprint(x))
	}
}

func clipCell(s string) sqlCell {
	if utf8.RuneCountInString(s) <= sqlMaxCellRunes {
		return sqlCell{Text: s}
	}
	r := []rune(s)
	return sqlCell{Text: string(r[:sqlMaxCellRunes]) + "…", Clipped: true}
}

// isBinaryType reports whether a column holds raw bytes rather than text, by
// the go-sql-driver type name (which already tells BLOB from TEXT by charset).
// JSON is deliberately not binary: MySQL refuses a binary string for a JSON
// column, so the dump must write it as quoted text.
func isBinaryType(dbType string) bool {
	switch dbType {
	case "BLOB", "TINYBLOB", "MEDIUMBLOB", "LONGBLOB", "BINARY", "VARBINARY", "BIT", "GEOMETRY":
		return true
	}
	return false
}

// formatSQLTime renders a scanned DATE/DATETIME/TIMESTAMP as MySQL literal
// text. The DSN parses times with loc=Local, so the time's own location holds
// the wall clock MySQL returned — format it as-is, never convert.
func formatSQLTime(t time.Time, dbType string) string {
	if dbType == "DATE" {
		if t.IsZero() {
			return "0000-00-00"
		}
		return t.Format("2006-01-02")
	}
	if t.IsZero() {
		return "0000-00-00 00:00:00"
	}
	return t.Format("2006-01-02 15:04:05.999999")
}

// handleSQLDump streams a mysqldump-style backup of the selected tables
// (?table=… repeated; all when none given). blobs=0 writes NULL for binary
// columns — in practice the torrent_file LONGBLOBs, which dominate the size —
// and gzip=0 disables compression (see flagParam). Restore with `mysql <db> < dump.sql`.
func (s *Server) handleSQLDump(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	all, err := s.listSQLTables(ctx)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	// Only names that exist are ever interpolated (backtick-quoted) below.
	tables := make([]string, 0, len(all))
	if want := r.URL.Query()["table"]; len(want) > 0 {
		known := make(map[string]bool, len(all))
		for _, t := range all {
			known[t.Name] = true
		}
		for _, name := range want {
			if !known[name] {
				http.Error(w, "unknown table: "+name, http.StatusBadRequest)
				return
			}
			tables = append(tables, name)
		}
	} else {
		for _, t := range all {
			tables = append(tables, t.Name)
		}
	}
	blobs := flagParam(r.URL.Query(), "blobs")
	gz := flagParam(r.URL.Query(), "gzip")

	// One READ ONLY repeatable-read transaction for the whole dump, so every
	// table comes from the same InnoDB snapshot.
	tx, err := s.store.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelRepeatableRead, ReadOnly: true})
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	defer tx.Rollback()
	var dbName, timeZone string
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE(DATABASE(), ''), @@session.time_zone`).Scan(&dbName, &timeZone); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	now := time.Now()
	filename := fmt.Sprintf("%s-%s.sql", dbName, now.Format("20060102-150405"))
	if gz {
		filename += ".gz"
		w.Header().Set("Content-Type", "application/gzip")
	} else {
		w.Header().Set("Content-Type", "application/sql; charset=utf-8")
	}
	w.Header().Set("Content-Disposition", `attachment; filename="`+filename+`"`)

	var out io.Writer = w
	if gz {
		zw := gzip.NewWriter(w)
		defer zw.Close()
		out = zw
	}
	bw := bufio.NewWriterSize(out, 256<<10)
	defer bw.Flush()

	log.Printf("sql dump: %d tables from %s (blobs=%v gzip=%v)", len(tables), dbName, blobs, gz)
	if err := writeDump(ctx, tx, bw, dbName, timeZone, tables, blobs, now); err != nil {
		// Headers are long gone; leave the failure where a restore will show it.
		log.Printf("sql dump failed: %v", err)
		fmt.Fprintf(bw, "\n-- ERROR: dump incomplete: %s\n", strings.ReplaceAll(err.Error(), "\n", " "))
	}
}

// flagParam reads an on-by-default flag. The form pairs each checkbox with a
// hidden "0" so an unticked box still says so, which means a ticked one sends
// both values — any "1" wins.
func flagParam(q url.Values, name string) bool {
	vals, ok := q[name]
	if !ok {
		return true
	}
	for _, v := range vals {
		if v == "1" {
			return true
		}
	}
	return false
}

func writeDump(ctx context.Context, tx *sql.Tx, w *bufio.Writer, dbName, timeZone string, tables []string, blobs bool, now time.Time) error {
	fmt.Fprintf(w, "-- phimtor2 admin dump of `%s`, %s\n", dbName, now.Format(time.RFC3339))
	if !blobs {
		w.WriteString("-- Binary (BLOB) columns were omitted and written as NULL.\n")
	}
	w.WriteString("\nSET NAMES utf8mb4;\n")
	fmt.Fprintf(w, "SET time_zone = %s;\n", quoteSQL(timeZone))
	w.WriteString("SET FOREIGN_KEY_CHECKS = 0;\nSET UNIQUE_CHECKS = 0;\nSET SQL_MODE = 'NO_AUTO_VALUE_ON_ZERO';\n")

	for _, table := range tables {
		if err := dumpTable(ctx, tx, w, table, blobs); err != nil {
			return fmt.Errorf("%s: %w", table, err)
		}
	}

	w.WriteString("\nSET FOREIGN_KEY_CHECKS = 1;\nSET UNIQUE_CHECKS = 1;\n-- dump complete\n")
	return nil
}

func dumpTable(ctx context.Context, tx *sql.Tx, w *bufio.Writer, table string, blobs bool) error {
	qt := quoteIdent(table)
	var name, create string
	if err := tx.QueryRowContext(ctx, "SHOW CREATE TABLE "+qt).Scan(&name, &create); err != nil {
		return err
	}
	fmt.Fprintf(w, "\n--\n-- Table %s\n--\n\nDROP TABLE IF EXISTS %s;\n%s;\n\n", qt, qt, create)

	rows, err := tx.QueryContext(ctx, "SELECT * FROM "+qt)
	if err != nil {
		return err
	}
	defer rows.Close()
	types, err := rows.ColumnTypes()
	if err != nil {
		return err
	}
	cols := make([]string, len(types))
	binary := make([]bool, len(types))
	for i, t := range types {
		cols[i] = quoteIdent(t.Name())
		binary[i] = isBinaryType(t.DatabaseTypeName())
	}
	insert := "INSERT INTO " + qt + " (" + strings.Join(cols, ", ") + ") VALUES\n"

	vals, ptrs := scanTargets(len(types))
	var sb strings.Builder
	batch := 0 // bytes in the open INSERT; 0 = none open
	for rows.Next() {
		if err := rows.Scan(ptrs...); err != nil {
			return err
		}
		sb.Reset()
		sb.WriteByte('(')
		for i, v := range vals {
			if i > 0 {
				sb.WriteByte(',')
			}
			if binary[i] && !blobs {
				v = nil
			}
			writeSQLValue(&sb, v, binary[i], types[i].DatabaseTypeName())
		}
		sb.WriteByte(')')

		if batch == 0 {
			w.WriteString(insert)
		} else {
			w.WriteString(",\n")
		}
		w.WriteString(sb.String())
		batch += sb.Len()
		if batch >= dumpBatchBytes {
			w.WriteString(";\n")
			batch = 0
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if batch > 0 {
		w.WriteString(";\n")
	}
	return nil
}

func writeSQLValue(sb *strings.Builder, v any, binary bool, dbType string) {
	switch x := v.(type) {
	case nil:
		sb.WriteString("NULL")
	case int64:
		sb.WriteString(strconv.FormatInt(x, 10))
	case uint64:
		sb.WriteString(strconv.FormatUint(x, 10))
	case float64:
		sb.WriteString(strconv.FormatFloat(x, 'g', -1, 64))
	case float32:
		sb.WriteString(strconv.FormatFloat(float64(x), 'g', -1, 32))
	case bool:
		if x {
			sb.WriteByte('1')
		} else {
			sb.WriteByte('0')
		}
	case time.Time:
		sb.WriteString(quoteSQL(formatSQLTime(x, dbType)))
	case []byte:
		if binary {
			if len(x) == 0 {
				sb.WriteString("''")
				return
			}
			sb.WriteString("0x")
			sb.WriteString(hex.EncodeToString(x))
			return
		}
		sb.WriteString(quoteSQL(string(x)))
	default:
		sb.WriteString(quoteSQL(fmt.Sprint(x)))
	}
}

// quoteSQL renders s as a single-quoted MySQL string literal, escaping the
// same characters mysqldump does.
func quoteSQL(s string) string {
	var b strings.Builder
	b.Grow(len(s) + 2)
	b.WriteByte('\'')
	for i := 0; i < len(s); i++ {
		switch c := s[i]; c {
		case 0:
			b.WriteString(`\0`)
		case '\n':
			b.WriteString(`\n`)
		case '\r':
			b.WriteString(`\r`)
		case '\\':
			b.WriteString(`\\`)
		case '\'':
			b.WriteString(`\'`)
		case '"':
			b.WriteString(`\"`)
		case 0x1a:
			b.WriteString(`\Z`)
		default:
			b.WriteByte(c)
		}
	}
	b.WriteByte('\'')
	return b.String()
}

func quoteIdent(name string) string {
	return "`" + strings.ReplaceAll(name, "`", "``") + "`"
}
