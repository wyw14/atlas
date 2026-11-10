// Copyright 2021-present The Atlas Authors. All rights reserved.
// This source code is licensed under the Apache 2.0 license found
// in the LICENSE file in the root directory of this source tree.

package migrate

import (
	"context"
	"fmt"
	"reflect"
	"strings"
)

// AssertStmt represents a single file-level atlas:assert assertion parsed
// from a migration file. Assertions are executed in declaration order before
// any business statement of the file is executed and before its first
// revision is written.
type AssertStmt struct {
	// Pos is the 1-based position of the assertion among the atlas:assert
	// directives of the file. It is stable across execution attempts.
	Pos int
	// Text is the assertion query text.
	Text string
	// Stmt is the parsed assertion statement, used for logging.
	Stmt *Stmt
}

// AssertError is returned when a file-level atlas:assert assertion cannot be
// satisfied. It carries the migration file name and the 1-based assertion
// position within that file.
type AssertError struct {
	// File is the name of the migration file that holds the assertion.
	File string
	// Pos is the 1-based position of the failed assertion.
	Pos int
	// Stmt is the assertion query text.
	Stmt string
	// Reason describes why the assertion failed.
	Reason string
	// Err wraps the underlying database error, if the failure was caused
	// by the query itself (execution, scanning or context errors).
	Err error
}

// Error implements error.
func (e *AssertError) Error() string {
	s := fmt.Sprintf("sql/migrate: assertion %d in file %q", e.Pos, e.File)
	if e.Stmt != "" {
		s += fmt.Sprintf(" (%q)", e.Stmt)
	}
	s += ": " + e.Reason
	if e.Err != nil {
		s += ": " + e.Err.Error()
	}
	return s
}

// Unwrap returns the underlying database error, if any.
func (e *AssertError) Unwrap() error { return e.Err }

// AssertRecoveryError is returned when a migration file that declares
// atlas:assert directives has been partially applied in a previous attempt.
// Auto-recovery is refused because re-running assertions that depend on the
// initial database state, or silently skipping them, cannot be made safe.
type AssertRecoveryError struct {
	// File is the name of the partially applied migration file.
	File string
	// Applied is the number of business statements already applied.
	Applied int
	// Total is the number of business statements the file contains.
	Total int
}

// Error implements error.
func (e *AssertRecoveryError) Error() string {
	return fmt.Sprintf(
		"sql/migrate: file %q declares atlas:assert directives but already records %d of %d applied statements; "+
			"automatic recovery is not supported, resolve the migration state manually before re-running",
		e.File, e.Applied, e.Total,
	)
}

// AssertChecksumError is returned when a file declares atlas:assert directives
// but opts out of the migration directory checksum (atlas:sum ignore). Such a
// file must not acquire a "verified assertion" state because its assertion text
// is not covered by any recorded checksum.
type AssertChecksumError struct {
	// File is the name of the migration file excluded from the checksum.
	File string
}

// Error implements error.
func (e *AssertChecksumError) Error() string {
	return fmt.Sprintf(
		"sql/migrate: file %q declares atlas:assert directives but is excluded from the migration directory "+
			"checksum (atlas:sum ignore); remove the sum directive to enable verified assertions",
		e.File,
	)
}

// fileAsserts extracts the file-level atlas:assert directives of the given
// migration file and parses each one into exactly one top-level SQL statement
// using the configured statement scanner. Hence, semicolons inside string
// literals and comments are handled correctly, and a directive carrying more
// than one top-level statement (or none) is rejected.
//
// Assertion SQL is treated as trusted migration code: the caller is
// responsible for its read-only contract. The executor does not attempt to
// prove that an arbitrary SQL function or expression is side-effect free.
func (e *Executor) fileAsserts(m File) ([]*AssertStmt, error) {
	lf, ok := m.(*LocalFile)
	if !ok {
		return nil, nil
	}
	args := lf.AssertDirectives()
	if len(args) == 0 {
		return nil, nil
	}
	asserts := make([]*AssertStmt, 0, len(args))
	for i, text := range args {
		// Directive arguments are single line texts; terminate them with a
		// newline before scanning so line comments end exactly as they do in
		// regular migration files (e.g. a ";" inside a "--" comment stays
		// part of the comment). Semicolons inside quoted strings are handled
		// by the scanner itself.
		scan := text
		if !strings.HasSuffix(scan, "\n") {
			scan += "\n"
		}
		// Reuse the existing scanning capability, including driver specific
		// scanner options, instead of splitting on semicolons or keywords.
		stmts, err := FileStmtDecls(e.drv, NewLocalFile(m.Name(), []byte(scan)))
		if err != nil {
			return nil, fmt.Errorf("sql/migrate: parsing assertion %d in file %q: %w", i+1, m.Name(), err)
		}
		switch len(stmts) {
		case 0:
			return nil, fmt.Errorf("sql/migrate: assertion %d in file %q is empty", i+1, m.Name())
		case 1:
		default:
			return nil, fmt.Errorf(
				"sql/migrate: assertion %d in file %q contains %d top-level statements; expect exactly one statement per atlas:assert directive",
				i+1, m.Name(), len(stmts),
			)
		}
		asserts = append(asserts, &AssertStmt{
			Pos:  i + 1,
			Text: stmts[0].Text,
			Stmt: stmts[0],
		})
	}
	return asserts, nil
}

// runAsserts executes the given assertions in declaration order using the
// executor's bound driver handle. Assertions therefore run on the same handle
// as the subsequent DDL statements: when the executor was created with a
// transactional driver, assertions participate in that transaction; in plain
// connection-pool mode no additional isolation is promised and no separate
// transaction is opened to fake atomicity with the DDL.
//
// Execution stops at the first failed assertion. The caller must not execute
// any business statement of the file when an error is returned.
func (e *Executor) runAsserts(ctx context.Context, m File, asserts []*AssertStmt) error {
	texts := make([]string, len(asserts))
	for i, a := range asserts {
		texts[i] = a.Text
	}
	e.log.Log(LogChecks{Name: m.Name(), Stmts: texts})
	for _, a := range asserts {
		err := e.runAssert(ctx, m, a)
		e.log.Log(LogCheck{Stmt: a.Text, Decl: a.Stmt, Error: err})
		if err != nil {
			e.log.Log(LogChecksDone{Error: err})
			return err
		}
	}
	e.log.Log(LogChecksDone{})
	return nil
}

// runAssert executes a single assertion and validates that its result set has
// exactly one row and one column, whose value is either boolean true or the
// integer 1. Every other outcome (false, NULL, zero rows, multiple rows,
// multiple columns, unsupported value or a query error) fails the assertion.
func (e *Executor) runAssert(ctx context.Context, m File, a *AssertStmt) error {
	// Derive a cancellable context so the resources bound to the query are
	// released promptly when the file execution stops or the parent is done.
	qctx, cancel := context.WithCancel(ctx)
	defer cancel()
	rows, err := e.drv.QueryContext(qctx, a.Text)
	if err != nil {
		return &AssertError{
			File: m.Name(), Pos: a.Pos, Stmt: a.Text,
			Reason: "assertion query failed", Err: err,
		}
	}
	// Rows must always be closed, including error and cancellation paths.
	defer rows.Close()
	cols, err := rows.Columns()
	if err != nil {
		return &AssertError{
			File: m.Name(), Pos: a.Pos, Stmt: a.Text,
			Reason: "assertion query failed", Err: err,
		}
	}
	if len(cols) != 1 {
		return &AssertError{
			File: m.Name(), Pos: a.Pos, Stmt: a.Text,
			Reason: fmt.Sprintf("returned %d columns; expect exactly one column", len(cols)),
		}
	}
	if !rows.Next() {
		if err := rows.Err(); err != nil {
			return &AssertError{
				File: m.Name(), Pos: a.Pos, Stmt: a.Text,
				Reason: "assertion query failed", Err: err,
			}
		}
		return &AssertError{
			File: m.Name(), Pos: a.Pos, Stmt: a.Text,
			Reason: "returned no rows; expect exactly one row with a true or 1 value",
		}
	}
	var v any
	if err := rows.Scan(&v); err != nil {
		return &AssertError{
			File: m.Name(), Pos: a.Pos, Stmt: a.Text,
			Reason: "assertion query failed", Err: err,
		}
	}
	if ok, reason := assertValue(v); !ok {
		return &AssertError{
			File: m.Name(), Pos: a.Pos, Stmt: a.Text,
			Reason: reason,
		}
	}
	// A second row means the result set is not exactly one row.
	if rows.Next() {
		return &AssertError{
			File: m.Name(), Pos: a.Pos, Stmt: a.Text,
			Reason: "returned more than one row; expect exactly one row",
		}
	}
	if err := rows.Err(); err != nil {
		return &AssertError{
			File: m.Name(), Pos: a.Pos, Stmt: a.Text,
			Reason: "assertion query failed", Err: err,
		}
	}
	return nil
}

// assertValue reports whether the scanned single value satisfies the
// assertion contract: boolean true or the integer 1.
func assertValue(v any) (bool, string) {
	if v == nil {
		return false, "returned NULL; expect true or 1"
	}
	rv := reflect.ValueOf(v)
	switch rv.Kind() {
	case reflect.Bool:
		if rv.Bool() {
			return true, ""
		}
		return false, "returned false; expect true or 1"
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		if n := rv.Int(); n != 1 {
			return false, fmt.Sprintf("returned %d; expect true or 1", n)
		}
		return true, ""
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		if n := rv.Uint(); n != 1 {
			return false, fmt.Sprintf("returned %d; expect true or 1", n)
		}
		return true, ""
	default:
		return false, fmt.Sprintf("returned %v of type %T; expect a boolean true or integer 1", v, v)
	}
}
