// Copyright 2021-present The Atlas Authors. All rights reserved.
// This source code is licensed under the Apache 2.0 license found
// in the LICENSE file in the root directory of this source tree.

package migrate_test

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"ariga.io/atlas/sql/migrate"
	"ariga.io/atlas/sql/sqlite"

	"github.com/DATA-DOG/go-sqlmock"
	_ "github.com/mattn/go-sqlite3"
	"github.com/stretchr/testify/require"
)

const assertFileName = "1_assert.sql"

// assertFileContent builds a migration file with detached, file-level
// atlas:assert directives followed by the given business statements.
func assertFileContent(asserts, stmts []string) string {
	var b strings.Builder
	for _, a := range asserts {
		b.WriteString("-- atlas:assert " + a + "\n")
	}
	b.WriteByte('\n')
	for _, s := range stmts {
		b.WriteString(s + "\n")
	}
	return b.String()
}

// newMockDriver returns a migrate.Driver backed by a sqlmock database. The
// driver uses the SQLite statement scanner, so directive SQL is parsed with
// the production scanner.
func newMockDriver(t *testing.T) (*sql.DB, sqlmock.Sqlmock, migrate.Driver) {
	t.Helper()
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherEqual))
	require.NoError(t, err)
	// sqlmock treats Close as an expectation; each test asserts the
	// expectations it cares about, so closing the handle is best-effort here.
	t.Cleanup(func() { _ = db.Close() })
	drv, err := sqlite.Open(db)
	require.NoError(t, err)
	return db, mock, drv
}

// newAssertDir writes a single migration file plus a valid atlas.sum file.
func newAssertDir(t *testing.T, name, content string) *migrate.MemDir {
	t.Helper()
	mem := &migrate.MemDir{}
	require.NoError(t, mem.WriteFile(name, []byte(content)))
	sum, err := mem.Checksum()
	require.NoError(t, err)
	require.NoError(t, migrate.WriteSumFile(mem, sum))
	return mem
}

// countingRevisionReadWriter counts WriteRevision calls.
type countingRevisionReadWriter struct {
	mockRevisionReadWriter
	writes int
}

func (c *countingRevisionReadWriter) WriteRevision(ctx context.Context, r *migrate.Revision) error {
	c.writes++
	return c.mockRevisionReadWriter.WriteRevision(ctx, r)
}

func oneRow(values ...driver.Value) *sqlmock.Rows {
	cols := make([]string, len(values))
	for i := range values {
		cols[i] = "c"
	}
	return sqlmock.NewRows(cols).AddRow(values...)
}

func TestExecutor_AssertSuccess_CallSequenceAndRevisions(t *testing.T) {
	var (
		_, mock, drv = newMockDriver(t)
		q1, q2       = "SELECT 1;", "SELECT true;"
		ddl1, ddl2   = "CREATE TABLE t1(c int);", "CREATE TABLE t2(c int);"
		dir          = newAssertDir(t, assertFileName, assertFileContent([]string{q1, q2}, []string{ddl1, ddl2}))
		rrw          = &countingRevisionReadWriter{}
		log          = &mockLogger{}
	)
	ex, err := migrate.NewExecutor(drv, dir, rrw, migrate.WithLogger(log))
	require.NoError(t, err)

	// Assertion queries are expected before any business statement.
	mock.ExpectQuery(q1).RowsWillBeClosed().WillReturnRows(oneRow(int64(1)))
	mock.ExpectQuery(q2).RowsWillBeClosed().WillReturnRows(oneRow(true))
	mock.ExpectExec(ddl1).WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectExec(ddl2).WillReturnResult(sqlmock.NewResult(0, 0))

	require.NoError(t, ex.Execute(context.Background(), mustFile(t, dir, assertFileName)))
	require.NoError(t, mock.ExpectationsWereMet())

	// Initial write, one write per business statement and the deferred final write.
	require.Equal(t, 4, rrw.writes)
	revs, err := rrw.ReadRevisions(context.Background())
	require.NoError(t, err)
	require.Len(t, revs, 1)
	require.Equal(t, 2, revs[0].Applied)
	require.Equal(t, 2, revs[0].Total)
	require.Empty(t, revs[0].PartialHashes)
	require.Empty(t, revs[0].Error)

	// Log sequence: file, checks group, two checks, group done, then statements.
	require.Len(t, *log, 7)
	require.IsType(t, migrate.LogFile{}, (*log)[0])
	require.Equal(t, migrate.LogChecks{Name: assertFileName, Stmts: []string{q1, q2}}, (*log)[1])
	require.Equal(t, migrate.LogCheck{Stmt: q1, Decl: &migrate.Stmt{Pos: 0, Text: q1}, Error: nil}, (*log)[2])
	require.IsType(t, migrate.LogCheck{}, (*log)[3])
	require.Equal(t, migrate.LogChecksDone{}, (*log)[4])
	require.IsType(t, migrate.LogStmt{}, (*log)[5])
	require.IsType(t, migrate.LogStmt{}, (*log)[6])
}

func TestExecutor_AssertResultShapes(t *testing.T) {
	const (
		q   = "SELECT asserted;"
		ddl = "CREATE TABLE t(c int);"
	)
	tests := []struct {
		name    string
		rows    *sqlmock.Rows
		wantErr string // empty means the assertion passes
	}{
		{name: "int64 one", rows: oneRow(int64(1))},
		{name: "boolean true", rows: oneRow(true)},
		{name: "int64 zero", rows: oneRow(int64(0)), wantErr: "returned 0"},
		{name: "int64 other", rows: oneRow(int64(2)), wantErr: "returned 2"},
		{name: "boolean false", rows: oneRow(false), wantErr: "returned false"},
		{name: "null", rows: oneRow(nil), wantErr: "returned NULL"},
		{name: "no rows", rows: sqlmock.NewRows([]string{"c"}), wantErr: "returned no rows"},
		{name: "multiple rows", rows: oneRow(int64(1)).AddRow(int64(1)), wantErr: "more than one row"},
		{name: "multiple columns", rows: sqlmock.NewRows([]string{"a", "b"}).AddRow(int64(1), int64(1)), wantErr: "one column"},
		{name: "text value", rows: oneRow("1"), wantErr: "type"},
		{name: "float value", rows: oneRow(float64(1)), wantErr: "type"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, mock, drv := newMockDriver(t)
			dir := newAssertDir(t, assertFileName, assertFileContent([]string{q}, []string{ddl}))
			rrw := &countingRevisionReadWriter{}
			ex, err := migrate.NewExecutor(drv, dir, rrw)
			require.NoError(t, err)

			mock.ExpectQuery(q).RowsWillBeClosed().WillReturnRows(tt.rows)
			if tt.wantErr == "" {
				mock.ExpectExec(ddl).WillReturnResult(sqlmock.NewResult(0, 0))
			}
			err = ex.Execute(context.Background(), mustFile(t, dir, assertFileName))
			if tt.wantErr != "" {
				var ae *migrate.AssertError
				require.ErrorAs(t, err, &ae)
				require.Equal(t, assertFileName, ae.File)
				require.Equal(t, 1, ae.Pos)
				require.Equal(t, q, ae.Stmt)
				require.ErrorContains(t, err, tt.wantErr)
				// Nothing was applied or recorded.
				require.Equal(t, 0, rrw.writes)
				revs, rerr := rrw.ReadRevisions(context.Background())
				require.NoError(t, rerr)
				require.Empty(t, revs)
			} else {
				require.NoError(t, err)
				require.Equal(t, 3, rrw.writes, "initial, per-statement and final revision write")
			}
			require.NoError(t, mock.ExpectationsWereMet())
		})
	}
}

func TestExecutor_AssertQueryError(t *testing.T) {
	var (
		_, mock, drv = newMockDriver(t)
		q            = "SELECT asserted;"
		sentinel     = errors.New("boom")
		dir          = newAssertDir(t, assertFileName, assertFileContent([]string{q}, []string{"CREATE TABLE t(c int);"}))
		rrw          = &countingRevisionReadWriter{}
	)
	ex, err := migrate.NewExecutor(drv, dir, rrw)
	require.NoError(t, err)
	// A query error produces no rows, so no RowsWillBeClosed expectation applies.
	mock.ExpectQuery(q).WillReturnError(sentinel)

	err = ex.Execute(context.Background(), mustFile(t, dir, assertFileName))
	var ae *migrate.AssertError
	require.ErrorAs(t, err, &ae)
	require.ErrorIs(t, err, sentinel)
	require.Equal(t, "assertion query failed", ae.Reason)
	require.Equal(t, 0, rrw.writes, "no revision write must happen on assertion failure")
	require.NoError(t, mock.ExpectationsWereMet(), "no business statement may run after an assertion error")
}

func TestExecutor_AssertContextCancellation(t *testing.T) {
	var (
		_, mock, drv = newMockDriver(t)
		q            = "SELECT asserted;"
		dir          = newAssertDir(t, assertFileName, assertFileContent([]string{q}, []string{"CREATE TABLE t(c int);"}))
		rrw          = &countingRevisionReadWriter{}
	)
	ex, err := migrate.NewExecutor(drv, dir, rrw)
	require.NoError(t, err)
	// Delay the response so the canceled context wins the race deterministically.
	mock.ExpectQuery(q).WillDelayFor(time.Second).WillReturnRows(oneRow(int64(1)))

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()
	err = ex.Execute(ctx, mustFile(t, dir, assertFileName))
	var ae *migrate.AssertError
	require.ErrorAs(t, err, &ae)
	// sqlmock reports the cancellation with its own message; real drivers
	// surface context.Canceled through the same error path.
	require.ErrorContains(t, err, "cancel")
	require.Equal(t, 0, rrw.writes)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestExecutor_AssertHandleBinding(t *testing.T) {
	t.Run("same transaction for assertions and DDL", func(t *testing.T) {
		db, mock, _ := newMockDriver(t)
		var (
			q   = "SELECT asserted;"
			ddl = "CREATE TABLE t(c int);"
			dir = newAssertDir(t, assertFileName, assertFileContent([]string{q}, []string{ddl}))
			rrw = &countingRevisionReadWriter{}
		)
		mock.ExpectBegin()
		tx, err := db.BeginTx(context.Background(), nil)
		require.NoError(t, err)
		drv, err := sqlite.Open(tx)
		require.NoError(t, err)
		ex, err := migrate.NewExecutor(drv, dir, rrw)
		require.NoError(t, err)

		mock.ExpectQuery(q).RowsWillBeClosed().WillReturnRows(oneRow(int64(1)))
		mock.ExpectExec(ddl).WillReturnResult(sqlmock.NewResult(0, 0))
		mock.ExpectCommit()
		require.NoError(t, ex.Execute(context.Background(), mustFile(t, dir, assertFileName)))
		require.NoError(t, tx.Commit())
		// The single Begin/Commit pair proves assertions and DDL share the tx handle.
		require.NoError(t, mock.ExpectationsWereMet())
	})

	t.Run("pool mode opens no independent transaction", func(t *testing.T) {
		_, mock, drv := newMockDriver(t)
		var (
			q   = "SELECT asserted;"
			ddl = "CREATE TABLE t(c int);"
			dir = newAssertDir(t, assertFileName, assertFileContent([]string{q}, []string{ddl}))
			rrw = &countingRevisionReadWriter{}
		)
		ex, err := migrate.NewExecutor(drv, dir, rrw)
		require.NoError(t, err)
		// No ExpectBegin is registered: an unexpected Begin would fail the test.
		mock.ExpectQuery(q).RowsWillBeClosed().WillReturnRows(oneRow(int64(1)))
		mock.ExpectExec(ddl).WillReturnResult(sqlmock.NewResult(0, 0))
		require.NoError(t, ex.Execute(context.Background(), mustFile(t, dir, assertFileName)))
		require.NoError(t, mock.ExpectationsWereMet())
	})
}

func TestExecutor_AssertParsing(t *testing.T) {
	t.Run("multiple top-level statements are rejected", func(t *testing.T) {
		_, mock, drv := newMockDriver(t)
		content := "-- atlas:assert SELECT 1; SELECT 2\n\nCREATE TABLE t(c int);\n"
		dir := newAssertDir(t, assertFileName, content)
		rrw := &countingRevisionReadWriter{}
		ex, err := migrate.NewExecutor(drv, dir, rrw)
		require.NoError(t, err)
		// No database expectation is registered: parsing fails before any query is sent.
		err = ex.Execute(context.Background(), mustFile(t, dir, assertFileName))
		require.ErrorContains(t, err, "contains 2 top-level statements")
		require.ErrorContains(t, err, assertFileName)
		require.Equal(t, 0, rrw.writes)
		require.NoError(t, mock.ExpectationsWereMet())
	})

	t.Run("empty assertion is rejected", func(t *testing.T) {
		_, mock, drv := newMockDriver(t)
		content := "-- atlas:assert\n\nCREATE TABLE t(c int);\n"
		dir := newAssertDir(t, assertFileName, content)
		rrw := &countingRevisionReadWriter{}
		ex, err := migrate.NewExecutor(drv, dir, rrw)
		require.NoError(t, err)
		err = ex.Execute(context.Background(), mustFile(t, dir, assertFileName))
		require.ErrorContains(t, err, "is empty")
		require.Equal(t, 0, rrw.writes)
		require.NoError(t, mock.ExpectationsWereMet())
	})

	t.Run("semicolons in string literals and comments are preserved", func(t *testing.T) {
		_, mock, drv := newMockDriver(t)
		var (
			q1 = "SELECT 'a;b' = 'a;b';"
			q2 = "SELECT 1 /* ;semicolon; */;"
			q3 = "SELECT 1 -- line ;comment"
		)
		dir := newAssertDir(t, assertFileName, assertFileContent([]string{q1, q2, q3}, nil))
		rrw := &countingRevisionReadWriter{}
		ex, err := migrate.NewExecutor(drv, dir, rrw)
		require.NoError(t, err)
		// Each directive must reach the driver as exactly one query statement.
		mock.ExpectQuery(q1).RowsWillBeClosed().WillReturnRows(oneRow(int64(1)))
		mock.ExpectQuery(q2).RowsWillBeClosed().WillReturnRows(oneRow(int64(1)))
		mock.ExpectQuery(q3).RowsWillBeClosed().WillReturnRows(oneRow(int64(1)))
		require.NoError(t, ex.Execute(context.Background(), mustFile(t, dir, assertFileName)))
		require.NoError(t, mock.ExpectationsWereMet())
	})

	t.Run("declaration order is preserved", func(t *testing.T) {
		_, mock, drv := newMockDriver(t)
		qs := []string{"SELECT 1;", "SELECT 2;", "SELECT 3;"}
		dir := newAssertDir(t, assertFileName, assertFileContent(qs, nil))
		log := &mockLogger{}
		ex, err := migrate.NewExecutor(drv, dir, &countingRevisionReadWriter{}, migrate.WithLogger(log))
		require.NoError(t, err)
		// sqlmock matches expectations in order by default.
		for _, q := range qs {
			mock.ExpectQuery(q).RowsWillBeClosed().WillReturnRows(oneRow(int64(1)))
		}
		require.NoError(t, ex.Execute(context.Background(), mustFile(t, dir, assertFileName)))
		require.NoError(t, mock.ExpectationsWereMet())
		require.Equal(t, migrate.LogChecks{Name: assertFileName, Stmts: qs}, (*log)[1])
	})
}

func TestExecutor_AssertPartialRecovery(t *testing.T) {
	t.Run("assert file refuses automatic recovery", func(t *testing.T) {
		var (
			_, mock, drv = newMockDriver(t)
			q            = "SELECT asserted;"
			ddl1, ddl2   = "CREATE TABLE t1(c int);", "CREATE TABLE t2(c int);"
			dir          = newAssertDir(t, assertFileName, assertFileContent([]string{q}, []string{ddl1, ddl2}))
			rrw          = &countingRevisionReadWriter{}
		)
		ex, err := migrate.NewExecutor(drv, dir, rrw)
		require.NoError(t, err)

		// First attempt: assertion passes, first DDL applies, second fails.
		mock.ExpectQuery(q).RowsWillBeClosed().WillReturnRows(oneRow(int64(1)))
		mock.ExpectExec(ddl1).WillReturnResult(sqlmock.NewResult(0, 0))
		mock.ExpectExec(ddl2).WillReturnError(errors.New("ddl boom"))
		err = ex.Execute(context.Background(), mustFile(t, dir, assertFileName))
		require.Error(t, err)
		revs, err := rrw.ReadRevisions(context.Background())
		require.NoError(t, err)
		require.Len(t, revs, 1)
		require.Equal(t, 1, revs[0].Applied)
		writesAfterFirstAttempt := rrw.writes

		// Second attempt: recovery must be refused before any query or statement runs.
		err = ex.Execute(context.Background(), mustFile(t, dir, assertFileName))
		var are *migrate.AssertRecoveryError
		require.ErrorAs(t, err, &are)
		require.Equal(t, assertFileName, are.File)
		require.Equal(t, 1, are.Applied)
		require.Equal(t, 2, are.Total)
		require.Equal(t, writesAfterFirstAttempt, rrw.writes, "no revision write on refused recovery")
		revs, err = rrw.ReadRevisions(context.Background())
		require.NoError(t, err)
		require.Equal(t, 1, revs[0].Applied, "applied state must not advance")
		require.NoError(t, mock.ExpectationsWereMet())
	})

	t.Run("plain files keep the existing recovery behavior", func(t *testing.T) {
		var (
			_, mock, drv = newMockDriver(t)
			ddl1, ddl2   = "CREATE TABLE t1(c int);", "CREATE TABLE t2(c int);"
			dir          = newAssertDir(t, assertFileName, assertFileContent(nil, []string{ddl1, ddl2}))
			rrw          = &countingRevisionReadWriter{}
		)
		ex, err := migrate.NewExecutor(drv, dir, rrw)
		require.NoError(t, err)

		mock.ExpectExec(ddl1).WillReturnResult(sqlmock.NewResult(0, 0))
		mock.ExpectExec(ddl2).WillReturnError(errors.New("ddl boom"))
		require.Error(t, ex.Execute(context.Background(), mustFile(t, dir, assertFileName)))

		// Retry continues at the second statement; the first one is not re-executed.
		mock.ExpectExec(ddl2).WillReturnResult(sqlmock.NewResult(0, 0))
		require.NoError(t, ex.Execute(context.Background(), mustFile(t, dir, assertFileName)))
		require.NoError(t, mock.ExpectationsWereMet())
		revs, err := rrw.ReadRevisions(context.Background())
		require.NoError(t, err)
		require.Equal(t, 2, revs[len(revs)-1].Applied)
		require.Empty(t, revs[len(revs)-1].PartialHashes)
	})
}

func TestExecutor_AssertChecksum(t *testing.T) {
	content := assertFileContent([]string{"SELECT 1;"}, []string{"CREATE TABLE t(c int);"})
	edited := assertFileContent([]string{"SELECT 2;"}, []string{"CREATE TABLE t(c int);"})

	// Assertion text is part of the file bytes covered by the directory checksum.
	h1, err := migrate.NewHashFile([]migrate.File{migrate.NewLocalFile(assertFileName, []byte(content))})
	require.NoError(t, err)
	h2, err := migrate.NewHashFile([]migrate.File{migrate.NewLocalFile(assertFileName, []byte(edited))})
	require.NoError(t, err)
	require.NotEqual(t, h1[0].H, h2[0].H, "editing an assertion must change the file checksum")

	// Checksum-ignored files are absent from the hash file.
	ignored := migrate.NewLocalFile("2_ignored.sql", []byte("-- atlas:sum ignore\nSELECT 1;"))
	h3, err := migrate.NewHashFile([]migrate.File{
		migrate.NewLocalFile(assertFileName, []byte(content)),
		ignored,
	})
	require.NoError(t, err)
	require.Len(t, h3, 1)
	require.Equal(t, assertFileName, h3[0].N)

	// A checksum-ignored file cannot run verified assertions.
	_, mock, drv := newMockDriver(t)
	ignoredContent := "-- atlas:sum ignore\n-- atlas:assert SELECT 1;\n\nCREATE TABLE t(c int);\n"
	dir := &migrate.MemDir{}
	require.NoError(t, dir.WriteFile(assertFileName, []byte(ignoredContent)))
	rrw := &countingRevisionReadWriter{}
	ex, err := migrate.NewExecutor(drv, dir, rrw)
	require.NoError(t, err)
	err = ex.Execute(context.Background(), mustFile(t, dir, assertFileName))
	var ace *migrate.AssertChecksumError
	require.ErrorAs(t, err, &ace)
	require.Equal(t, 0, rrw.writes)
	require.NoError(t, mock.ExpectationsWereMet(), "no database call must happen for checksum-ignored assertions")
}

func TestExecutor_AssertLogsStopOnError(t *testing.T) {
	var (
		_, mock, drv = newMockDriver(t)
		q1, q2       = "SELECT 1;", "SELECT 2;"
		ddl          = "CREATE TABLE t(c int);"
		dir          = newAssertDir(t, assertFileName, assertFileContent([]string{q1, q2}, []string{ddl}))
		log          = &mockLogger{}
		rrw          = &countingRevisionReadWriter{}
	)
	ex, err := migrate.NewExecutor(drv, dir, rrw, migrate.WithLogger(log))
	require.NoError(t, err)
	mock.ExpectQuery(q1).RowsWillBeClosed().WillReturnRows(oneRow(false))
	// No expectation for q2 or the DDL: they must never run.

	err = ex.Execute(context.Background(), mustFile(t, dir, assertFileName))
	require.Error(t, err)
	require.NoError(t, mock.ExpectationsWereMet())

	var (
		hasFile, hasChecks, hasCheck, hasChecksDone, hasErr bool
	)
	for _, e := range *log {
		switch e := e.(type) {
		case migrate.LogFile:
			hasFile = true
		case migrate.LogChecks:
			hasChecks = true
			require.Equal(t, []string{q1, q2}, e.Stmts)
		case migrate.LogCheck:
			hasCheck = true
			require.Equal(t, q1, e.Stmt)
			require.NotNil(t, e.Decl)
			require.Error(t, e.Error)
		case migrate.LogChecksDone:
			hasChecksDone = true
			require.Error(t, e.Error)
		case migrate.LogStmt:
			t.Fatalf("no statement must be logged after an assertion error: %v", e)
		case migrate.LogError:
			hasErr = true
		}
	}
	require.True(t, hasFile && hasChecks && hasCheck && hasChecksDone && hasErr)
}

func TestExecutor_AssertBatchStopsOnFailure(t *testing.T) {
	_, drv := func() (*sql.DB, migrate.Driver) {
		dsn := fmt.Sprintf("file:assert-batch-%d?mode=memory&cache=shared", time.Now().UnixNano())
		db, err := sql.Open("sqlite3", dsn)
		require.NoError(t, err)
		db.SetMaxOpenConns(1)
		t.Cleanup(func() { require.NoError(t, db.Close()) })
		d, err := sqlite.Open(db)
		require.NoError(t, err)
		return db, d
	}()
	mem := &migrate.MemDir{}
	require.NoError(t, mem.WriteFile("1_first.sql", []byte(
		"-- atlas:assert SELECT 0;\n\nCREATE TABLE first_t(c int);\n")))
	require.NoError(t, mem.WriteFile("2_second.sql", []byte("CREATE TABLE second_t(c int);\n")))
	sum, err := mem.Checksum()
	require.NoError(t, err)
	require.NoError(t, migrate.WriteSumFile(mem, sum))

	var (
		rrw = &countingRevisionReadWriter{}
		log = &mockLogger{}
	)
	ex, err := migrate.NewExecutor(drv, mem, rrw, migrate.WithLogger(log))
	require.NoError(t, err)
	err = ex.ExecuteN(context.Background(), 0)
	var ae *migrate.AssertError
	require.ErrorAs(t, err, &ae)
	require.Equal(t, "1_first.sql", ae.File)

	// The batch stops before the second file and no business SQL was executed.
	require.Equal(t, 0, rrw.writes)
	var fileEntries, stmtEntries int
	for _, e := range *log {
		switch e.(type) {
		case migrate.LogFile:
			fileEntries++
		case migrate.LogStmt:
			stmtEntries++
		}
	}
	require.Equal(t, 1, fileEntries, "only the failing file may be started")
	require.Equal(t, 0, stmtEntries, "no business statement may run after an assertion error")
}

func TestExecutor_AssertRealSQLite(t *testing.T) {
	openReal := func(t *testing.T) (*sql.DB, migrate.Driver) {
		dsn := fmt.Sprintf("file:assert-%d?mode=memory&cache=shared", time.Now().UnixNano())
		db, err := sql.Open("sqlite3", dsn)
		require.NoError(t, err)
		db.SetMaxOpenConns(1)
		t.Cleanup(func() { require.NoError(t, db.Close()) })
		drv, err := sqlite.Open(db)
		require.NoError(t, err)
		return db, drv
	}
	tableExists := func(t *testing.T, db *sql.DB, name string) bool {
		var n int
		require.NoError(t, db.QueryRow(
			"SELECT count(*) FROM sqlite_master WHERE type='table' AND name=?", name,
		).Scan(&n))
		return n == 1
	}

	t.Run("end to end shapes against a real database", func(t *testing.T) {
		_, drv := openReal(t)
		cases := []struct {
			query   string
			wantErr bool
		}{
			{"SELECT 1;", false},
			{"SELECT true;", false},
			{"SELECT 1 = 1;", false},
			{"SELECT 0;", true},
			{"SELECT 1 = 2;", true},
			{"SELECT NULL;", true},
			{"SELECT 1 WHERE 0;", true},            // zero rows
			{"SELECT 1 UNION ALL SELECT 2;", true}, // multiple rows
			{"SELECT 1, 2;", true},                 // multiple columns
			{"SELECT '1';", true},                  // text, not boolean/integer
			{"SELECT 'a;b' = 'a;b';", false},       // semicolon inside string literal
			{"SELECT 1 /* ;ignored; */;", false},   // semicolon inside block comment
		}
		for _, c := range cases {
			dir := newAssertDir(t, assertFileName, assertFileContent([]string{c.query}, nil))
			rrw := &countingRevisionReadWriter{}
			ex, err := migrate.NewExecutor(drv, dir, rrw)
			require.NoError(t, err)
			err = ex.Execute(context.Background(), mustFile(t, dir, assertFileName))
			if c.wantErr {
				var ae *migrate.AssertError
				require.ErrorAs(t, err, &ae, "query %q expected an assertion error", c.query)
				require.Empty(t, *rrw)
			} else {
				require.NoError(t, err, "query %q expected to pass", c.query)
			}
		}
	})

	t.Run("failure does not apply DDL and a corrected file re-runs cleanly", func(t *testing.T) {
		db, drv := openReal(t)
		bad := newAssertDir(t, assertFileName, assertFileContent(
			[]string{"SELECT 0;"}, []string{"CREATE TABLE should_not_exist(c int);"}))
		rrw := &countingRevisionReadWriter{}
		ex, err := migrate.NewExecutor(drv, bad, rrw)
		require.NoError(t, err)
		require.Error(t, ex.Execute(context.Background(), mustFile(t, bad, assertFileName)))
		require.False(t, tableExists(t, db, "should_not_exist"))
		require.Empty(t, *rrw, "assertion failure must not create a revision")

		// Re-running with a passing assertion evaluates it against the current state again.
		good := newAssertDir(t, assertFileName, assertFileContent(
			[]string{"SELECT 1;"}, []string{"CREATE TABLE should_not_exist(c int);"}))
		ex, err = migrate.NewExecutor(drv, good, &countingRevisionReadWriter{})
		require.NoError(t, err)
		require.NoError(t, ex.Execute(context.Background(), mustFile(t, good, assertFileName)))
		require.True(t, tableExists(t, db, "should_not_exist"))
	})

	t.Run("assertions see uncommitted state on the same transaction", func(t *testing.T) {
		db, _ := openReal(t)
		tx, err := db.BeginTx(context.Background(), nil)
		require.NoError(t, err)
		txDrv, err := sqlite.Open(tx)
		require.NoError(t, err)
		// Uncommitted DDL and DML on the transaction handle.
		_, err = txDrv.ExecContext(context.Background(), "CREATE TABLE u(c int);")
		require.NoError(t, err)
		_, err = txDrv.ExecContext(context.Background(), "INSERT INTO u VALUES (1);")
		require.NoError(t, err)

		dir := newAssertDir(t, assertFileName, assertFileContent(
			[]string{"SELECT COUNT(*) FROM u;"}, // sees the uncommitted row -> 1
			[]string{"CREATE TABLE v(c int);"},
		))
		ex, err := migrate.NewExecutor(txDrv, dir, &countingRevisionReadWriter{})
		require.NoError(t, err)
		require.NoError(t, ex.Execute(context.Background(), mustFile(t, dir, assertFileName)))
		require.NoError(t, tx.Commit())
		require.True(t, tableExists(t, db, "u"))
		require.True(t, tableExists(t, db, "v"))
	})

	t.Run("assertion failure rolls back together with the transaction", func(t *testing.T) {
		db, _ := openReal(t)
		tx, err := db.BeginTx(context.Background(), nil)
		require.NoError(t, err)
		txDrv, err := sqlite.Open(tx)
		require.NoError(t, err)
		_, err = txDrv.ExecContext(context.Background(), "CREATE TABLE w(c int);")
		require.NoError(t, err)
		dir := newAssertDir(t, assertFileName, assertFileContent(
			[]string{"SELECT 0;"}, []string{"CREATE TABLE w2(c int);"},
		))
		ex, err := migrate.NewExecutor(txDrv, dir, &countingRevisionReadWriter{})
		require.NoError(t, err)
		require.Error(t, ex.Execute(context.Background(), mustFile(t, dir, assertFileName)))
		require.NoError(t, tx.Rollback())
		require.False(t, tableExists(t, db, "w"))
		require.False(t, tableExists(t, db, "w2"))
	})
}

func mustFile(t *testing.T, d migrate.Dir, name string) migrate.File {
	t.Helper()
	files, err := d.Files()
	require.NoError(t, err)
	for _, f := range files {
		if f.Name() == name {
			return f
		}
	}
	t.Fatalf("file %q not found", name)
	return nil
}
