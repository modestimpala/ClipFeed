package main

import (
	"context"
	"database/sql"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Dialect represents the SQL database backend in use.
type Dialect string

const (
	DialectSQLite   Dialect = "sqlite"
	DialectPostgres Dialect = "postgres"
)

// CompatDB wraps *sql.DB to provide transparent ? → $N placeholder
// conversion for Postgres while keeping SQLite queries unchanged.
type CompatDB struct {
	DB      *sql.DB
	Dialect Dialect
}

func NewCompatDB(db *sql.DB, dialect Dialect) *CompatDB {
	return &CompatDB{DB: db, Dialect: dialect}
}

func (d *CompatDB) Close() error                         { return d.DB.Close() }
func (d *CompatDB) SetMaxOpenConns(n int)                { d.DB.SetMaxOpenConns(n) }
func (d *CompatDB) SetMaxIdleConns(n int)                { d.DB.SetMaxIdleConns(n) }
func (d *CompatDB) SetConnMaxLifetime(dur time.Duration) { d.DB.SetConnMaxLifetime(dur) }
func (d *CompatDB) IsPostgres() bool                     { return d.Dialect == DialectPostgres }

func (d *CompatDB) rewrite(query string) string {
	if d.Dialect == DialectSQLite {
		return query
	}
	return rewritePlaceholders(query)
}

func (d *CompatDB) Exec(query string, args ...interface{}) (sql.Result, error) {
	return d.DB.Exec(d.rewrite(query), args...)
}

func (d *CompatDB) ExecContext(ctx context.Context, query string, args ...interface{}) (sql.Result, error) {
	return d.DB.ExecContext(ctx, d.rewrite(query), args...)
}

func (d *CompatDB) Query(query string, args ...interface{}) (*sql.Rows, error) {
	return d.DB.Query(d.rewrite(query), args...)
}

func (d *CompatDB) QueryContext(ctx context.Context, query string, args ...interface{}) (*sql.Rows, error) {
	return d.DB.QueryContext(ctx, d.rewrite(query), args...)
}

func (d *CompatDB) QueryRow(query string, args ...interface{}) *sql.Row {
	return d.DB.QueryRow(d.rewrite(query), args...)
}

func (d *CompatDB) QueryRowContext(ctx context.Context, query string, args ...interface{}) *sql.Row {
	return d.DB.QueryRowContext(ctx, d.rewrite(query), args...)
}

func (d *CompatDB) Conn(ctx context.Context) (*CompatConn, error) {
	conn, err := d.DB.Conn(ctx)
	if err != nil {
		return nil, err
	}
	return &CompatConn{Conn: conn, dialect: d.Dialect}, nil
}

// CompatConn wraps *sql.Conn with automatic placeholder conversion.
type CompatConn struct {
	Conn    *sql.Conn
	dialect Dialect
}

func (c *CompatConn) Close() error { return c.Conn.Close() }

func (c *CompatConn) rewrite(query string) string {
	if c.dialect == DialectSQLite {
		return query
	}
	return rewritePlaceholders(query)
}

func (c *CompatConn) ExecContext(ctx context.Context, query string, args ...interface{}) (sql.Result, error) {
	return c.Conn.ExecContext(ctx, c.rewrite(query), args...)
}

func (c *CompatConn) QueryContext(ctx context.Context, query string, args ...interface{}) (*sql.Rows, error) {
	return c.Conn.QueryContext(ctx, c.rewrite(query), args...)
}

func (c *CompatConn) QueryRowContext(ctx context.Context, query string, args ...interface{}) *sql.Row {
	return c.Conn.QueryRowContext(ctx, c.rewrite(query), args...)
}

// rewritePlaceholders converts ? to $1, $2, ... for Postgres.
// Respects single-quoted string literals and escaped quotes ('').
func rewritePlaceholders(query string) string {
	var buf strings.Builder
	buf.Grow(len(query) + 32)
	n := 1
	inStr := false
	for i := 0; i < len(query); i++ {
		c := query[i]
		if c == '\'' {
			if inStr && i+1 < len(query) && query[i+1] == '\'' {
				// Escaped quote ('') — stays inside the string literal.
				buf.WriteByte(c)
				buf.WriteByte(query[i+1])
				i++
				continue
			}
			inStr = !inStr
			buf.WriteByte(c)
		} else if c == '?' && !inStr {
			buf.WriteByte('$')
			buf.WriteString(strconv.Itoa(n))
			n++
		} else {
			buf.WriteByte(c)
		}
	}
	return buf.String()
}

// ---------------------------------------------------------------------------
// SQL dialect helpers — return SQL fragments appropriate for the dialect.
// ---------------------------------------------------------------------------

// NowUTC returns a SQL expression for the current UTC time as ISO 8601 text.
func (d *CompatDB) NowUTC() string {
	if d.IsPostgres() {
		return `to_char(now() AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS"Z"')`
	}
	return `strftime('%Y-%m-%dT%H:%M:%SZ', 'now')`
}

// AgeHoursExpr returns a SQL expression computing "hours since <col>".
func (d *CompatDB) AgeHoursExpr(col string) string {
	if d.IsPostgres() {
		return fmt.Sprintf("EXTRACT(EPOCH FROM (now() - %s::timestamptz)) / 3600.0", col)
	}
	return fmt.Sprintf("(julianday('now') - julianday(%s)) * 24.0", col)
}

// RandomFloat returns a SQL expression that evaluates to a float in [0, 1).
func (d *CompatDB) RandomFloat() string {
	if d.IsPostgres() {
		return "random()"
	}
	return "CAST(ABS(RANDOM()) AS REAL) / 9223372036854775807.0"
}

// DatetimeModifier returns a SQL expression for datetime(base, modifier).
// For Postgres, interprets the modifier (e.g. '-24 hours', '-7 days').
// The `base` should be 'now' and modifier should be like '-24 hours'.
func (d *CompatDB) DatetimeModifier(modifier string) string {
	if d.IsPostgres() {
		// Convert SQLite modifier like "-24 hours" → Postgres interval
		mod := strings.TrimPrefix(modifier, "-")
		return fmt.Sprintf("now() AT TIME ZONE 'UTC' - interval '%s'", mod)
	}
	return fmt.Sprintf("datetime('now', '%s')", modifier)
}

// DatetimeRecencyExpr returns a SQL expression for "created_at > now - N days".
// For filters where recency is parameterized with a negative day count.
func (d *CompatDB) DatetimeRecencyExpr() string {
	if d.IsPostgres() {
		// Parameter is negative days as integer, e.g. -7; multiply by interval to get offset.
		return "c.created_at > (now() AT TIME ZONE 'UTC' + ? * interval '1 day')"
	}
	return "c.created_at > datetime('now', ? || ' days')"
}

// DBSizeExpr returns a SQL expression for the database size in MB.
func (d *CompatDB) DBSizeExpr() string {
	if d.IsPostgres() {
		return "COALESCE(pg_database_size(current_database()) / 1024.0 / 1024.0, 0)"
	}
	return "COALESCE((SELECT page_count * page_size / 1024.0 / 1024.0 FROM pragma_page_count(), pragma_page_size()), 0)"
}

// DateExpr returns a SQL expression for date('now', modifier).
func (d *CompatDB) DateExpr(modifier string) string {
	if d.IsPostgres() {
		mod := strings.TrimPrefix(modifier, "-")
		return fmt.Sprintf("to_char((now() AT TIME ZONE 'UTC' - interval '%s')::date, 'YYYY-MM-DD')", mod)
	}
	return fmt.Sprintf("date('now', '%s')", modifier)
}

// DateOfExpr returns a SQL expression extracting the date from a column as text.
func (d *CompatDB) DateOfExpr(col string) string {
	if d.IsPostgres() {
		return fmt.Sprintf("LEFT(%s, 10)", col)
	}
	return fmt.Sprintf("date(%s)", col)
}

// PurgeDatetimeExpr returns the SQL fragment used in admin auto-purge.
// For SQLite: datetime(COALESCE(col, created_at)) <= datetime('now', modifier)
// For Postgres: equivalent cast+comparison.
func (d *CompatDB) PurgeDatetimeComparison(coalesced string, modifier string) string {
	if d.IsPostgres() {
		mod := strings.TrimPrefix(modifier, "-")
		return fmt.Sprintf("%s::timestamptz <= now() - interval '%s'", coalesced, mod)
	}
	return fmt.Sprintf("datetime(%s) <= datetime('now', '%s')", coalesced, modifier)
}

// BeginTxSQL returns the SQL statement to begin a write transaction.
func (d *CompatDB) BeginTxSQL() string {
	if d.IsPostgres() {
		return "BEGIN"
	}
	return "BEGIN IMMEDIATE"
}
