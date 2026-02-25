package db

import (
	"strings"
	"testing"
)

// ---------------------------------------------------------------------------
// rewritePlaceholders
// ---------------------------------------------------------------------------

func TestRewritePlaceholders_Empty(t *testing.T) {
	if got := rewritePlaceholders(""); got != "" {
		t.Errorf("got %q, want empty", got)
	}
}

func TestRewritePlaceholders_NoPlaceholders(t *testing.T) {
	in := "SELECT 1"
	if got := rewritePlaceholders(in); got != in {
		t.Errorf("got %q, want %q", got, in)
	}
}

func TestRewritePlaceholders_Single(t *testing.T) {
	got := rewritePlaceholders("SELECT * FROM t WHERE id = ?")
	want := "SELECT * FROM t WHERE id = $1"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestRewritePlaceholders_Multiple(t *testing.T) {
	got := rewritePlaceholders("INSERT INTO t (a, b, c) VALUES (?, ?, ?)")
	want := "INSERT INTO t (a, b, c) VALUES ($1, $2, $3)"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestRewritePlaceholders_QuestionInStringLiteral(t *testing.T) {
	// ? inside a quoted string must not be rewritten.
	got := rewritePlaceholders("SELECT '?' AS q FROM t WHERE id = ?")
	want := "SELECT '?' AS q FROM t WHERE id = $1"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestRewritePlaceholders_EscapedQuote(t *testing.T) {
	// '' inside a string is an escaped single-quote; the ? after closing ' is a placeholder.
	got := rewritePlaceholders("SELECT 'it''s' WHERE x = ?")
	want := "SELECT 'it''s' WHERE x = $1"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestRewritePlaceholders_MultipleStringsAndPlaceholders(t *testing.T) {
	got := rewritePlaceholders("SELECT 'a?b' WHERE c = ? AND d = ?")
	want := "SELECT 'a?b' WHERE c = $1 AND d = $2"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

// ---------------------------------------------------------------------------
// Dialect helpers -- CompatDB with nil DB is safe; these methods only inspect
// d.Dialect and build SQL strings.
// ---------------------------------------------------------------------------

func sqliteDB() *CompatDB { return &CompatDB{Dialect: DialectSQLite} }
func pgDB() *CompatDB     { return &CompatDB{Dialect: DialectPostgres} }

func TestIsPostgres(t *testing.T) {
	if sqliteDB().IsPostgres() {
		t.Error("SQLite CompatDB.IsPostgres() should be false")
	}
	if !pgDB().IsPostgres() {
		t.Error("Postgres CompatDB.IsPostgres() should be true")
	}
}

func TestBeginTxSQL(t *testing.T) {
	if got := sqliteDB().BeginTxSQL(); got != "BEGIN IMMEDIATE" {
		t.Errorf("SQLite = %q, want BEGIN IMMEDIATE", got)
	}
	if got := pgDB().BeginTxSQL(); got != "BEGIN" {
		t.Errorf("Postgres = %q, want BEGIN", got)
	}
}

func TestNowUTC(t *testing.T) {
	if got := sqliteDB().NowUTC(); !strings.Contains(got, "strftime") {
		t.Errorf("SQLite NowUTC = %q: expected strftime", got)
	}
	if got := pgDB().NowUTC(); !strings.Contains(got, "now()") {
		t.Errorf("Postgres NowUTC = %q: expected now()", got)
	}
}

func TestRandomFloat(t *testing.T) {
	if got := sqliteDB().RandomFloat(); !strings.Contains(got, "RANDOM") {
		t.Errorf("SQLite RandomFloat = %q", got)
	}
	if got := pgDB().RandomFloat(); !strings.Contains(got, "random") {
		t.Errorf("Postgres RandomFloat = %q", got)
	}
}

func TestAgeHoursExpr(t *testing.T) {
	col := "created_at"
	sq := sqliteDB().AgeHoursExpr(col)
	if !strings.Contains(sq, "julianday") {
		t.Errorf("SQLite AgeHoursExpr = %q: expected julianday", sq)
	}
	if !strings.Contains(sq, col) {
		t.Errorf("SQLite AgeHoursExpr = %q: missing column %q", sq, col)
	}

	pg := pgDB().AgeHoursExpr(col)
	if !strings.Contains(pg, "EXTRACT") {
		t.Errorf("Postgres AgeHoursExpr = %q: expected EXTRACT", pg)
	}
	if !strings.Contains(pg, col) {
		t.Errorf("Postgres AgeHoursExpr = %q: missing column %q", pg, col)
	}
}

func TestDatetimeModifier_StripsMinus(t *testing.T) {
	mod := "-24 hours"
	sq := sqliteDB().DatetimeModifier(mod)
	if !strings.Contains(sq, "datetime") || !strings.Contains(sq, mod) {
		t.Errorf("SQLite DatetimeModifier = %q", sq)
	}

	pg := pgDB().DatetimeModifier(mod)
	if !strings.Contains(pg, "interval") {
		t.Errorf("Postgres DatetimeModifier = %q: expected interval", pg)
	}
	// Leading minus must be stripped for Postgres interval syntax.
	if strings.Contains(pg, "-24") {
		t.Errorf("Postgres DatetimeModifier = %q: minus should be stripped", pg)
	}
	if !strings.Contains(pg, "24 hours") {
		t.Errorf("Postgres DatetimeModifier = %q: expected '24 hours'", pg)
	}
}

func TestDatetimeRecencyExpr_HasPlaceholder(t *testing.T) {
	// Both dialects must keep the ? placeholder so callers can bind a parameter.
	if sq := sqliteDB().DatetimeRecencyExpr(); !strings.Contains(sq, "?") {
		t.Errorf("SQLite DatetimeRecencyExpr = %q: missing placeholder", sq)
	}
	if pg := pgDB().DatetimeRecencyExpr(); !strings.Contains(pg, "?") {
		t.Errorf("Postgres DatetimeRecencyExpr = %q: missing placeholder", pg)
	}
}

func TestDBSizeExpr(t *testing.T) {
	if got := sqliteDB().DBSizeExpr(); !strings.Contains(got, "pragma_page") {
		t.Errorf("SQLite DBSizeExpr = %q", got)
	}
	if got := pgDB().DBSizeExpr(); !strings.Contains(got, "pg_database_size") {
		t.Errorf("Postgres DBSizeExpr = %q", got)
	}
}

func TestDateExpr_StripsMinus(t *testing.T) {
	mod := "-7 days"
	if got := sqliteDB().DateExpr(mod); !strings.Contains(got, "date") {
		t.Errorf("SQLite DateExpr = %q", got)
	}
	pg := pgDB().DateExpr(mod)
	if !strings.Contains(pg, "interval") || strings.Contains(pg, "-7") {
		t.Errorf("Postgres DateExpr = %q: should use interval and strip minus", pg)
	}
}

func TestDateOfExpr(t *testing.T) {
	col := "created_at"
	if got := sqliteDB().DateOfExpr(col); !strings.Contains(got, "date(") || !strings.Contains(got, col) {
		t.Errorf("SQLite DateOfExpr = %q", got)
	}
	if got := pgDB().DateOfExpr(col); !strings.Contains(got, "LEFT(") || !strings.Contains(got, col) {
		t.Errorf("Postgres DateOfExpr = %q", got)
	}
}

func TestPurgeDatetimeComparison_StripsMinus(t *testing.T) {
	coalesced := "COALESCE(expires_at, created_at)"
	mod := "-30 days"
	if got := sqliteDB().PurgeDatetimeComparison(coalesced, mod); !strings.Contains(got, "datetime(") {
		t.Errorf("SQLite PurgeDatetimeComparison = %q", got)
	}
	pg := pgDB().PurgeDatetimeComparison(coalesced, mod)
	if !strings.Contains(pg, "interval") || strings.Contains(pg, "-30") {
		t.Errorf("Postgres PurgeDatetimeComparison = %q: should use interval and strip minus", pg)
	}
}
