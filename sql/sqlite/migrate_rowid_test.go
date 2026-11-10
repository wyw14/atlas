// Copyright 2021-present The Atlas Authors. All rights reserved.
// This source code is licensed under the Apache 2.0 license found
// in the LICENSE file in the root directory of this source tree.

package sqlite

import (
	"context"
	"database/sql"
	"strings"
	"testing"

	"ariga.io/atlas/sql/migrate"
	"ariga.io/atlas/sql/schema"

	_ "github.com/mattn/go-sqlite3"
	"github.com/stretchr/testify/require"
)

// openRowIDDB opens an in-memory SQLite database and an Atlas driver on top of it.
func openRowIDDB(t *testing.T) (*sql.DB, migrate.Driver) {
	t.Helper()
	db, err := sql.Open("sqlite3", ":memory:")
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, db.Close()) })
	drv, err := Open(db)
	require.NoError(t, err)
	return db, drv
}

func mustExecLite(t *testing.T, db *sql.DB, q string) {
	t.Helper()
	_, err := db.Exec(q)
	require.NoError(t, err, q)
}

func planText(p *migrate.Plan) string {
	cmds := make([]string, len(p.Changes))
	for i, c := range p.Changes {
		cmds[i] = c.Cmd
	}
	return strings.Join(cmds, "\n")
}

// dumpMatrix runs the query and returns all values as strings, using
// "<NULL>" for NULLs, so before/after states can be compared exactly.
func dumpMatrix(t *testing.T, db *sql.DB, q string) [][]string {
	t.Helper()
	rows, err := db.Query(q)
	require.NoError(t, err, q)
	defer rows.Close()
	cols, err := rows.Columns()
	require.NoError(t, err)
	var out [][]string
	for rows.Next() {
		vals := make([]sql.NullString, len(cols))
		ptrs := make([]any, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		require.NoError(t, rows.Scan(ptrs...))
		rec := make([]string, len(cols))
		for i, v := range vals {
			if v.Valid {
				rec[i] = v.String
			} else {
				rec[i] = "<NULL>"
			}
		}
		out = append(out, rec)
	}
	require.NoError(t, rows.Err())
	return out
}

// inspectLiteTable inspects a single table through the Atlas driver.
func inspectLiteTable(t *testing.T, drv migrate.Driver, name string) *schema.Table {
	t.Helper()
	s, err := drv.InspectSchema(context.Background(), mainFile, &schema.InspectOptions{Tables: []string{name}})
	require.NoError(t, err)
	tb, ok := s.Table(name)
	require.True(t, ok)
	return tb
}

// diffRebuildPlan diffs the inspected schema against a mutated copy of the
// table, plans the changes and asserts that a temporary-table rebuild is used.
func diffRebuildPlan(t *testing.T, drv migrate.Driver, name string, mutate func(*schema.Table)) (*migrate.Plan, []schema.Change) {
	t.Helper()
	ctx := context.Background()
	s1, err := drv.InspectSchema(ctx, mainFile, nil)
	require.NoError(t, err)
	s2, err := drv.InspectSchema(ctx, mainFile, nil)
	require.NoError(t, err)
	desired, ok := s2.Table(name)
	require.True(t, ok)
	mutate(desired)
	changes, err := drv.SchemaDiff(s1, s2)
	require.NoError(t, err)
	plan, err := drv.PlanChanges(ctx, "test", changes)
	require.NoError(t, err)
	text := planText(plan)
	require.Contains(t, text, "CREATE TABLE `new_"+name+"`")
	require.Contains(t, text, "DROP TABLE `"+name+"`")
	return plan, changes
}

// forceRebuildColumn is a column whose expression default forces the planner
// to rebuild the table instead of using ALTER TABLE ADD COLUMN.
func forceRebuildColumn() *schema.Column {
	return schema.NewStringColumn("extra", "text").
		SetDefault(&schema.RawExpr{X: "hex(1)"})
}

// TestMigrate_RowIDPreservation exercises the temporary-table rebuild flow
// against a real SQLite database and verifies that the hidden rowid identity
// survives column additions, deletions, default changes and renames.
func TestMigrate_RowIDPreservation(t *testing.T) {
	ctx := context.Background()

	t.Run("no primary key keeps negative, huge and gapped rowids", func(t *testing.T) {
		db, drv := openRowIDDB(t)
		mustExecLite(t, db, `CREATE TABLE users (v text)`)
		mustExecLite(t, db, `INSERT INTO users(rowid, v) VALUES(-5, 'neg'), (9223372036854775807, 'max'), (42, 'gap'), (7, 'kept')`)
		mustExecLite(t, db, `DELETE FROM users WHERE rowid = 42`)
		before := dumpMatrix(t, db, `SELECT rowid, v FROM users ORDER BY rowid`)

		plan, changes := diffRebuildPlan(t, drv, "users", func(desired *schema.Table) {
			desired.Columns = append(desired.Columns, forceRebuildColumn())
		})
		text := planText(plan)
		require.Contains(t, text, "INSERT INTO `new_users` (`_rowid_`, `v`) SELECT `_rowid_`, `v` FROM `users`")
		require.NoError(t, drv.ApplyChanges(ctx, changes))

		after := dumpMatrix(t, db, `SELECT rowid, v FROM users ORDER BY rowid`)
		require.Equal(t, before, after)
		// The new column is populated by its default, the row count must match as well.
		require.Equal(t, [][]string{{"-5", "neg", "31"}, {"7", "kept", "31"}, {"9223372036854775807", "max", "31"}},
			dumpMatrix(t, db, `SELECT rowid, v, extra FROM users ORDER BY rowid`))
	})

	t.Run("INT PRIMARY KEY is not a rowid alias", func(t *testing.T) {
		db, drv := openRowIDDB(t)
		mustExecLite(t, db, `CREATE TABLE users (id INT PRIMARY KEY, v text)`)
		mustExecLite(t, db, `INSERT INTO users(id, v) VALUES(100, 'a'), (200, 'b'), (-9, 'c')`)
		before := dumpMatrix(t, db, `SELECT rowid, id, v FROM users ORDER BY rowid`)
		// Hidden rowid and declared key differ.
		require.Equal(t, [][]string{{"1", "100", "a"}, {"2", "200", "b"}, {"3", "-9", "c"}}, before)

		plan, changes := diffRebuildPlan(t, drv, "users", func(desired *schema.Table) {
			desired.Columns = append(desired.Columns, forceRebuildColumn())
		})
		require.Contains(t, planText(plan), "`_rowid_`")
		require.NoError(t, drv.ApplyChanges(ctx, changes))

		require.Equal(t, before, dumpMatrix(t, db, `SELECT rowid, id, v FROM users ORDER BY rowid`))
	})

	t.Run("target alias primary key receives the source rowid", func(t *testing.T) {
		db, drv := openRowIDDB(t)
		mustExecLite(t, db, `CREATE TABLE users (id INT PRIMARY KEY, v text)`)
		mustExecLite(t, db, `INSERT INTO users(id, v) VALUES(100, 'a'), (200, 'b')`)
		beforeRowIDs := dumpMatrix(t, db, `SELECT rowid FROM users ORDER BY rowid`)
		require.Equal(t, [][]string{{"1"}, {"2"}}, beforeRowIDs)

		fromT := inspectLiteTable(t, drv, "users")
		to := inspectLiteTable(t, drv, "users")
		// Hand-crafted type promotion: the target gains a true rowid alias.
		toID := to.Columns[0]
		toID.Type.Raw = "INTEGER"
		changes := []schema.Change{&schema.ModifyTable{
			T: to,
			Changes: []schema.Change{
				&schema.ModifyColumn{From: fromT.Columns[0], To: toID, Change: schema.ChangeType},
			},
		}}
		plan, err := drv.PlanChanges(ctx, "test", changes)
		require.NoError(t, err)
		// The target alias position is fed with the source hidden rowid instead
		// of the old ordinary key values, so row identity stays stable.
		require.Contains(t, planText(plan), "INSERT INTO `new_users` (`id`, `v`) SELECT `_rowid_`, `v` FROM `users`")
		require.NoError(t, drv.ApplyChanges(ctx, changes))

		require.Equal(t, beforeRowIDs, dumpMatrix(t, db, `SELECT rowid FROM users ORDER BY rowid`))
		require.Equal(t, [][]string{{"1", "1", "a"}, {"2", "2", "b"}},
			dumpMatrix(t, db, `SELECT rowid, id, v FROM users ORDER BY rowid`))
	})

	t.Run("INTEGER PRIMARY KEY alias is copied without pseudo column injection", func(t *testing.T) {
		db, drv := openRowIDDB(t)
		mustExecLite(t, db, `CREATE TABLE users (id INTEGER PRIMARY KEY, v text)`)
		mustExecLite(t, db, `INSERT INTO users(id, v) VALUES(-9, 'neg'), (42, 'gap'), (9223372036854775807, 'max')`)
		mustExecLite(t, db, `DELETE FROM users WHERE id = 42`)
		before := dumpMatrix(t, db, `SELECT rowid, id, v FROM users ORDER BY rowid`)

		plan, changes := diffRebuildPlan(t, drv, "users", func(desired *schema.Table) {
			desired.Columns = append(desired.Columns, forceRebuildColumn())
		})
		text := planText(plan)
		require.NotContains(t, text, "`_rowid_`")
		require.NotContains(t, text, "`rowid`")
		require.NotContains(t, text, "`oid`")
		require.Contains(t, text, "INSERT INTO `new_users` (`id`, `v`")
		require.NoError(t, drv.ApplyChanges(ctx, changes))

		require.Equal(t, before, dumpMatrix(t, db, `SELECT rowid, id, v FROM users ORDER BY rowid`))
	})

	t.Run("inlined INTEGER PRIMARY KEY DESC is not a rowid alias", func(t *testing.T) {
		db, drv := openRowIDDB(t)
		mustExecLite(t, db, `CREATE TABLE users (id INTEGER PRIMARY KEY DESC, v text)`)
		mustExecLite(t, db, `INSERT INTO users(id, v) VALUES(100, 'a'), (200, 'b')`)
		before := dumpMatrix(t, db, `SELECT rowid, id, v FROM users ORDER BY rowid`)
		require.Equal(t, [][]string{{"1", "100", "a"}, {"2", "200", "b"}}, before)

		plan, changes := diffRebuildPlan(t, drv, "users", func(desired *schema.Table) {
			desired.Columns = append(desired.Columns, forceRebuildColumn())
		})
		text := planText(plan)
		// The quirk is preserved through an inlined column constraint, and
		// the hidden rowid is copied explicitly.
		require.Contains(t, text, "`id` integer NULL PRIMARY KEY DESC")
		require.Contains(t, text, "`_rowid_`")
		require.NotContains(t, text, "PRIMARY KEY (`id`")
		require.NoError(t, drv.ApplyChanges(ctx, changes))

		require.Equal(t, before, dumpMatrix(t, db, `SELECT rowid, id, v FROM users ORDER BY rowid`))
		// Re-inspection keeps detecting the descending inlined primary key.
		rebuilt := inspectLiteTable(t, drv, "users")
		require.True(t, rebuilt.PrimaryKey.Parts[0].Desc)
		require.Nil(t, rowidAliasColumn(rebuilt))
	})

	t.Run("table constraint PRIMARY KEY DESC stays a rowid alias", func(t *testing.T) {
		db, drv := openRowIDDB(t)
		mustExecLite(t, db, `CREATE TABLE users (id INTEGER, v text, PRIMARY KEY(id DESC))`)
		mustExecLite(t, db, `INSERT INTO users(id, v) VALUES(100, 'a'), (200, 'b')`)
		before := dumpMatrix(t, db, `SELECT rowid, id, v FROM users ORDER BY rowid`)
		// Table-constraint form keeps "id" as the rowid alias.
		require.Equal(t, [][]string{{"100", "100", "a"}, {"200", "200", "b"}}, before)

		plan, changes := diffRebuildPlan(t, drv, "users", func(desired *schema.Table) {
			desired.Columns = append(desired.Columns, forceRebuildColumn())
		})
		text := planText(plan)
		// The alias column is copied through the regular mapping: no hidden
		// rowid pseudo column is injected, hence rowid (== id) is preserved.
		require.NotContains(t, text, "`_rowid_`")
		require.NotContains(t, text, "`oid`")
		require.Contains(t, text, "INSERT INTO `new_users` (`id`, `v`) SELECT `id`, `v` FROM `users`")
		require.NoError(t, drv.ApplyChanges(ctx, changes))

		require.Equal(t, before, dumpMatrix(t, db, `SELECT rowid, id, v FROM users ORDER BY rowid`))
	})

	t.Run("WITHOUT ROWID tables keep explicit primary key without hidden columns", func(t *testing.T) {
		db, drv := openRowIDDB(t)
		mustExecLite(t, db, `CREATE TABLE users (id integer, v text, PRIMARY KEY(id)) WITHOUT ROWID`)
		mustExecLite(t, db, `INSERT INTO users(id, v) VALUES(-9, 'neg'), (9223372036854775807, 'max')`)
		before := dumpMatrix(t, db, `SELECT id, v FROM users ORDER BY id`)

		plan, changes := diffRebuildPlan(t, drv, "users", func(desired *schema.Table) {
			desired.Columns = append(desired.Columns, forceRebuildColumn())
		})
		text := planText(plan)
		require.NotContains(t, text, "`_rowid_`")
		require.NotContains(t, text, "`rowid`")
		require.NotContains(t, text, "`oid`")
		require.Contains(t, text, "WITHOUT ROWID")
		require.NoError(t, drv.ApplyChanges(ctx, changes))

		require.Equal(t, before, dumpMatrix(t, db, `SELECT id, v FROM users ORDER BY id`))
		_, err := db.Query(`SELECT rowid FROM users`)
		require.Error(t, err)
	})

	t.Run("storage model conversion does not inject hidden columns", func(t *testing.T) {
		// Rowid table -> WITHOUT ROWID.
		db, drv := openRowIDDB(t)
		mustExecLite(t, db, `CREATE TABLE users (id integer NOT NULL, v text)`)
		mustExecLite(t, db, `INSERT INTO users(rowid, id, v) VALUES(-5, 10, 'a'), (9223372036854775807, 20, 'b')`)

		s1, err := drv.InspectSchema(ctx, mainFile, nil)
		require.NoError(t, err)
		s2, err := drv.InspectSchema(ctx, mainFile, nil)
		require.NoError(t, err)
		desired, ok := s2.Table("users")
		require.True(t, ok)
		id, ok := desired.Column("id")
		require.True(t, ok)
		desired.SetPrimaryKey(schema.NewPrimaryKey(id))
		desired.AddAttrs(&WithoutRowID{})
		changes, err := drv.SchemaDiff(s1, s2)
		require.NoError(t, err)
		plan, err := drv.PlanChanges(ctx, "test", changes)
		require.NoError(t, err)
		text := planText(plan)
		require.NotContains(t, text, "`_rowid_`")
		require.NotContains(t, text, "`rowid`")
		require.NotContains(t, text, "`oid`")
		require.Contains(t, text, "WITHOUT ROWID")
		require.NoError(t, drv.ApplyChanges(ctx, changes))
		require.Equal(t, [][]string{{"10", "a"}, {"20", "b"}}, dumpMatrix(t, db, `SELECT id, v FROM users ORDER BY id`))

		// WITHOUT ROWID -> rowid table.
		s1, err = drv.InspectSchema(ctx, mainFile, nil)
		require.NoError(t, err)
		s2, err = drv.InspectSchema(ctx, mainFile, nil)
		require.NoError(t, err)
		desired, ok = s2.Table("users")
		require.True(t, ok)
		desired.PrimaryKey = nil
		desired.Attrs = removeWithoutRowID(desired.Attrs)
		changes, err = drv.SchemaDiff(s1, s2)
		require.NoError(t, err)
		plan, err = drv.PlanChanges(ctx, "test", changes)
		require.NoError(t, err)
		for _, c := range plan.Changes {
			require.NotContains(t, c.Cmd, "`_rowid_`")
			require.NotContains(t, c.Cmd, "`rowid`")
			require.NotContains(t, c.Cmd, "`oid`")
		}
		require.NoError(t, drv.ApplyChanges(ctx, changes))
		require.Equal(t, [][]string{{"10", "a"}, {"20", "b"}}, dumpMatrix(t, db, `SELECT id, v FROM users ORDER BY id`))
		// The rebuilt rowid table exposes a hidden rowid again.
		require.Len(t, dumpMatrix(t, db, `SELECT rowid FROM users`), 2)
	})

	t.Run("declared rowid column shadows the pseudo name", func(t *testing.T) {
		db, drv := openRowIDDB(t)
		mustExecLite(t, db, `CREATE TABLE users (rowid integer, v text)`)
		mustExecLite(t, db, `INSERT INTO users(rowid, v) VALUES(100, 'a'), (200, 'b')`)
		before := dumpMatrix(t, db, `SELECT _rowid_, rowid, v FROM users ORDER BY _rowid_`)

		plan, changes := diffRebuildPlan(t, drv, "users", func(desired *schema.Table) {
			desired.Columns = append(desired.Columns, forceRebuildColumn())
		})
		text := planText(plan)
		// "rowid" is an ordinary declared column here; the hidden identity
		// must be read and written through the non-shadowed "_rowid_" name.
		require.Contains(t, text, "INSERT INTO `new_users` (`_rowid_`, `rowid`, `v`) SELECT `_rowid_`, `rowid`, `v` FROM `users`")
		require.NoError(t, drv.ApplyChanges(ctx, changes))

		require.Equal(t, before, dumpMatrix(t, db, `SELECT _rowid_, rowid, v FROM users ORDER BY _rowid_`))
	})

	t.Run("source and target resolve different non-shadowed names", func(t *testing.T) {
		db, drv := openRowIDDB(t)
		// Source already shadows "_rowid_".
		mustExecLite(t, db, `CREATE TABLE users (_rowid_ integer, v text)`)
		mustExecLite(t, db, `INSERT INTO users(_rowid_, v) VALUES(100, 'a'), (200, 'b')`)
		before := dumpMatrix(t, db, `SELECT oid, _rowid_, v FROM users ORDER BY oid`)

		to := inspectLiteTable(t, drv, "users")
		// The target also declares "rowid": source must use "rowid" while
		// the target can only address its hidden rowid through "oid".
		to.Columns = append(to.Columns, schema.NewIntColumn("rowid", "integer").
			SetDefault(&schema.RawExpr{X: "hex(1)"}))
		changes := []schema.Change{&schema.ModifyTable{
			T:       to,
			Changes: []schema.Change{&schema.AddColumn{C: to.Columns[len(to.Columns)-1]}},
		}}
		plan, err := drv.PlanChanges(ctx, "test", changes)
		require.NoError(t, err)
		text := planText(plan)
		require.Contains(t, text, "INSERT INTO `new_users` (`oid`, `_rowid_`, `v`) SELECT `rowid`, `_rowid_`, `v` FROM `users`")
		require.NoError(t, drv.ApplyChanges(ctx, changes))

		// The rebuilt table declares both "_rowid_" and "rowid", so the
		// hidden rowid is only addressable through the remaining "oid" name.
		require.Equal(t, before, dumpMatrix(t, db, `SELECT oid, _rowid_, v FROM users ORDER BY oid`))
	})

	t.Run("all rowid names shadowed fail before any table is created", func(t *testing.T) {
		db, drv := openRowIDDB(t)
		mustExecLite(t, db, `CREATE TABLE users (rowid integer, oid integer, _rowid_ integer, v text)`)
		mustExecLite(t, db, `INSERT INTO users(rowid, oid, _rowid_, v) VALUES(1, 2, 3, 'a'), (4, 5, 6, 'b')`)
		before := dumpMatrix(t, db, `SELECT rowid, oid, _rowid_, v FROM users ORDER BY v`)

		from := inspectLiteTable(t, drv, "users")
		to := inspectLiteTable(t, drv, "users")
		snapshot := dumpMatrix(t, db, `SELECT name FROM sqlite_master WHERE type = 'table' ORDER BY name`)
		extra := forceRebuildColumn()
		to.Columns = append(to.Columns, extra)
		changes := []schema.Change{&schema.ModifyTable{
			T:       to,
			Changes: []schema.Change{&schema.AddColumn{C: extra}},
		}}
		plan, err := drv.PlanChanges(ctx, "test", changes)
		require.Nil(t, plan)
		require.ErrorContains(t, err, "cannot preserve rowid of table \"users\"")
		require.ErrorContains(t, err, "shadowed by declared columns")

		// Nothing was executed or created, and the input schema is untouched.
		require.Equal(t, snapshot, dumpMatrix(t, db, `SELECT name FROM sqlite_master WHERE type = 'table' ORDER BY name`))
		require.Empty(t, dumpMatrix(t, db, `SELECT name FROM sqlite_master WHERE name LIKE 'new_%'`))
		require.Equal(t, before, dumpMatrix(t, db, `SELECT rowid, oid, _rowid_, v FROM users ORDER BY v`))
		require.Equal(t, []string{"rowid", "oid", "_rowid_", "v"}, columnNames(from))
	})

	t.Run("all rowid names shadowed but INTEGER PRIMARY KEY alias exists", func(t *testing.T) {
		db, drv := openRowIDDB(t)
		mustExecLite(t, db, `CREATE TABLE users (id INTEGER PRIMARY KEY, rowid integer, oid integer, _rowid_ integer, v text)`)
		mustExecLite(t, db, `INSERT INTO users(id, rowid, oid, _rowid_, v) VALUES(-9, 1, 2, 3, 'a'), (77, 4, 5, 6, 'b')`)
		before := dumpMatrix(t, db, `SELECT rowid, id, rowid, oid, _rowid_, v FROM users ORDER BY id`)

		plan, changes := diffRebuildPlan(t, drv, "users", func(desired *schema.Table) {
			desired.Columns = append(desired.Columns, forceRebuildColumn())
		})
		// The alias column carries the rowid, so no pseudo column is injected:
		// the INSERT only references declared columns, including the shadowed names.
		require.Contains(t, planText(plan), "INSERT INTO `new_users` (`id`, `rowid`, `oid`, `_rowid_`, `v`) SELECT `id`, `rowid`, `oid`, `_rowid_`, `v` FROM `users`")
		require.NoError(t, drv.ApplyChanges(ctx, changes))

		require.Equal(t, before, dumpMatrix(t, db, `SELECT rowid, id, rowid, oid, _rowid_, v FROM users ORDER BY id`))
	})

	t.Run("column rename keeps the rowid and the values", func(t *testing.T) {
		db, drv := openRowIDDB(t)
		mustExecLite(t, db, `CREATE TABLE users (v text)`)
		mustExecLite(t, db, `INSERT INTO users(rowid, v) VALUES(-5, 'neg'), (9223372036854775807, 'max')`)
		before := dumpMatrix(t, db, `SELECT rowid, v FROM users ORDER BY rowid`)

		to := inspectLiteTable(t, drv, "users")
		note := to.Columns[0]
		note.Name = "note"
		extra := forceRebuildColumn()
		to.Columns = append(to.Columns, extra)
		changes := []schema.Change{&schema.ModifyTable{
			T: to,
			Changes: []schema.Change{
				&schema.RenameColumn{From: schema.NewColumn("v"), To: note},
				&schema.AddColumn{C: extra},
			},
		}}
		plan, err := drv.PlanChanges(ctx, "test", changes)
		require.NoError(t, err)
		require.Contains(t, planText(plan), "INSERT INTO `new_users` (`_rowid_`, `note`) SELECT `_rowid_`, `v` FROM `users`")
		require.NoError(t, drv.ApplyChanges(ctx, changes))

		require.Equal(t, before, dumpMatrix(t, db, `SELECT rowid, note FROM users ORDER BY rowid`))
	})

	t.Run("generated columns stay outside the INSERT and rowid is preserved", func(t *testing.T) {
		db, drv := openRowIDDB(t)
		mustExecLite(t, db, `CREATE TABLE users (v text, g integer GENERATED ALWAYS AS (length(v)) STORED)`)
		mustExecLite(t, db, `INSERT INTO users(rowid, v) VALUES(-5, 'abc'), (42, 'x')`)
		before := dumpMatrix(t, db, `SELECT rowid, v, g FROM users ORDER BY rowid`)

		plan, changes := diffRebuildPlan(t, drv, "users", func(desired *schema.Table) {
			desired.Columns = append(desired.Columns, forceRebuildColumn())
		})
		text := planText(plan)
		require.Contains(t, text, "`_rowid_`")
		require.NotContains(t, text, "`, g`")
		require.NoError(t, drv.ApplyChanges(ctx, changes))

		require.Equal(t, before, dumpMatrix(t, db, `SELECT rowid, v, g FROM users ORDER BY rowid`))
	})

	t.Run("nullable to non-nullable default conversion keeps IFNULL and rowid", func(t *testing.T) {
		db, drv := openRowIDDB(t)
		mustExecLite(t, db, `CREATE TABLE users (v text)`)
		mustExecLite(t, db, `INSERT INTO users(rowid, v) VALUES(-5, NULL), (42, 'x')`)
		beforeIDs := dumpMatrix(t, db, `SELECT rowid FROM users ORDER BY rowid`)

		s1, err := drv.InspectSchema(ctx, mainFile, nil)
		require.NoError(t, err)
		s2, err := drv.InspectSchema(ctx, mainFile, nil)
		require.NoError(t, err)
		desired, ok := s2.Table("users")
		require.True(t, ok)
		desired.Columns[0].Type.Null = false
		desired.Columns[0].Default = &schema.Literal{V: "'d'"}
		desired.Columns = append(desired.Columns, forceRebuildColumn())
		changes, err := drv.SchemaDiff(s1, s2)
		require.NoError(t, err)
		plan, err := drv.PlanChanges(ctx, "test", changes)
		require.NoError(t, err)
		require.Contains(t, planText(plan), "IFNULL(`v`, 'd')")
		require.Contains(t, planText(plan), "`_rowid_`")
		require.NoError(t, drv.ApplyChanges(ctx, changes))

		require.Equal(t, [][]string{{"-5", "d"}, {"42", "x"}}, dumpMatrix(t, db, `SELECT rowid, v FROM users ORDER BY rowid`))
		// Rowid identity must be unchanged.
		require.Equal(t, beforeIDs, dumpMatrix(t, db, `SELECT rowid FROM users ORDER BY rowid`))
	})

	t.Run("AUTOINCREMENT sequence and alias identity remain correct", func(t *testing.T) {
		db, drv := openRowIDDB(t)
		mustExecLite(t, db, `CREATE TABLE users (id INTEGER PRIMARY KEY AUTOINCREMENT, v text)`)
		mustExecLite(t, db, `INSERT INTO users(id, v) VALUES(100, 'a'), (100000, 'b')`)

		plan, changes := diffRebuildPlan(t, drv, "users", func(desired *schema.Table) {
			desired.Columns = append(desired.Columns, forceRebuildColumn())
		})
		text := planText(plan)
		require.NotContains(t, text, "`_rowid_`")
		require.Contains(t, text, "PRIMARY KEY AUTOINCREMENT")
		require.NoError(t, drv.ApplyChanges(ctx, changes))

		require.Equal(t, [][]string{{"100", "a"}, {"100000", "b"}}, dumpMatrix(t, db, `SELECT id, v FROM users ORDER BY id`))
		var seq int64
		require.NoError(t, db.QueryRow(`SELECT seq FROM sqlite_sequence WHERE name = 'users'`).Scan(&seq))
		require.GreaterOrEqual(t, seq, int64(100000))
		// New rows continue above the copied sequence value.
		mustExecLite(t, db, `INSERT INTO users(v) VALUES('c')`)
		var id int64
		require.NoError(t, db.QueryRow(`SELECT id FROM users WHERE v = 'c'`).Scan(&id))
		require.Greater(t, id, int64(100000))
	})

	t.Run("dropped columns do not duplicate or lose rowids", func(t *testing.T) {
		db, drv := openRowIDDB(t)
		mustExecLite(t, db, `CREATE TABLE users (a text, b text)`)
		mustExecLite(t, db, `INSERT INTO users(rowid, a, b) VALUES(-5, 'a1', 'b1'), (9223372036854775807, 'a2', 'b2')`)
		before := dumpMatrix(t, db, `SELECT rowid, a FROM users ORDER BY rowid`)

		s1, err := drv.InspectSchema(ctx, mainFile, nil)
		require.NoError(t, err)
		s2, err := drv.InspectSchema(ctx, mainFile, nil)
		require.NoError(t, err)
		desired, ok := s2.Table("users")
		require.True(t, ok)
		desired.Columns = []*schema.Column{desired.Columns[0]}
		changes, err := drv.SchemaDiff(s1, s2)
		require.NoError(t, err)
		plan, err := drv.PlanChanges(ctx, "test", changes)
		require.NoError(t, err)
		require.Contains(t, planText(plan), "INSERT INTO `new_users` (`_rowid_`, `a`) SELECT `_rowid_`, `a` FROM `users`")
		require.NoError(t, drv.ApplyChanges(ctx, changes))

		require.Equal(t, before, dumpMatrix(t, db, `SELECT rowid, a FROM users ORDER BY rowid`))
	})
}

// TestRowIDHelpers covers the detection logic without requiring a database.
func TestRowIDHelpers(t *testing.T) {
	col := func(name, raw string) *schema.Column {
		c := schema.NewIntColumn(name, raw)
		c.Type.Raw = raw
		return c
	}
	pk := func(c *schema.Column, desc bool) *schema.Index {
		return &schema.Index{Unique: true, Parts: []*schema.IndexPart{{C: c, Desc: desc}}}
	}

	t.Run("rowidAliasColumn", func(t *testing.T) {
		integer := col("id", "INTEGER")
		require.Nil(t, rowidAliasColumn(schema.NewTable("t").AddColumns(integer)))
		require.Equal(t, integer, rowidAliasColumn(schema.NewTable("t").AddColumns(integer).SetPrimaryKey(pk(integer, false))))
		// Non-exact integer types are ordinary unique keys.
		intPK := col("id", "INT")
		require.Nil(t, rowidAliasColumn(schema.NewTable("t").AddColumns(intPK).SetPrimaryKey(pk(intPK, false))))
		sized := col("id", "INTEGER(10)")
		require.Nil(t, rowidAliasColumn(schema.NewTable("t").AddColumns(sized).SetPrimaryKey(pk(sized, false))))
		// Composite primary keys never alias the rowid.
		a, b := col("a", "INTEGER"), col("b", "INTEGER")
		require.Nil(t, rowidAliasColumn(schema.NewTable("t").AddColumns(a, b).
			SetPrimaryKey(&schema.Index{Unique: true, Parts: []*schema.IndexPart{{C: a}, {C: b}}})))
		// Explicit descending part is not an alias.
		require.Nil(t, rowidAliasColumn(schema.NewTable("t").AddColumns(integer).SetPrimaryKey(pk(integer, true))))
		// WITHOUT ROWID tables have no hidden rowid.
		require.Nil(t, rowidAliasColumn(schema.NewTable("t").AddColumns(integer).
			SetPrimaryKey(pk(integer, false)).AddAttrs(&WithoutRowID{})))
		// DESC detected from the inspected CREATE statement (inlined form only).
		descCol := col("id", "INTEGER")
		descT := schema.NewTable("t").AddColumns(descCol).SetPrimaryKey(pk(descCol, false)).
			AddAttrs(&CreateStmt{S: "CREATE TABLE t (id INTEGER PRIMARY KEY DESC, v text)"})
		require.Nil(t, rowidAliasColumn(descT))
		// The table-constraint form does not carry the quirk: the column stays an alias.
		tableDesc := col("id", "INTEGER")
		tableDescT := schema.NewTable("t").AddColumns(tableDesc).SetPrimaryKey(pk(tableDesc, false)).
			AddAttrs(&CreateStmt{S: "CREATE TABLE t (id INTEGER, v text, PRIMARY KEY(id DESC))"})
		require.Equal(t, tableDesc, rowidAliasColumn(tableDescT))
		// ASC and ordinary inlined aliases are detected.
		ascT := schema.NewTable("t").AddColumns(integer).SetPrimaryKey(pk(integer, false)).
			AddAttrs(&CreateStmt{S: "CREATE TABLE t (id INTEGER PRIMARY KEY ASC, v text)"})
		require.Equal(t, integer, rowidAliasColumn(ascT))
	})

	t.Run("rowidPseudoColumn", func(t *testing.T) {
		require.Equal(t, "_rowid_", rowidPseudoColumn(schema.NewTable("t").AddColumns(col("v", "text"))))
		require.Equal(t, "rowid", rowidPseudoColumn(schema.NewTable("t").AddColumns(col("_ROWID_", "integer"))))
		require.Equal(t, "oid", rowidPseudoColumn(schema.NewTable("t").
			AddColumns(col("_rowid_", "integer"), col("RowID", "integer"))))
		require.Empty(t, rowidPseudoColumn(schema.NewTable("t").
			AddColumns(col("_rowid_", "integer"), col("rowid", "integer"), col("oid", "integer"))))
	})

	t.Run("resolveRowidRef", func(t *testing.T) {
		ref, err := resolveRowidRef(schema.NewTable("t").AddAttrs(&WithoutRowID{}))
		require.NoError(t, err)
		require.Nil(t, ref)

		shadowed := schema.NewTable("t").AddColumns(
			col("_rowid_", "integer"), col("rowid", "integer"), col("oid", "integer"), col("v", "text"),
		)
		_, err = resolveRowidRef(shadowed)
		require.ErrorContains(t, err, `cannot preserve rowid of table "t"`)

		integer := col("id", "INTEGER")
		// An alias wins over the pseudo names even when none of them is shadowed.
		aliasOnly := schema.NewTable("t").AddColumns(integer, col("v", "text")).SetPrimaryKey(pk(integer, false))
		ref, err = resolveRowidRef(aliasOnly)
		require.NoError(t, err)
		require.Equal(t, &rowidRef{name: "id", alias: true}, ref)

		// When all pseudo names are taken, the alias is still a safe fallback.
		withAlias := schema.NewTable("t").AddColumns(
			integer, col("_rowid_", "integer"), col("rowid", "integer"), col("oid", "integer"),
		).SetPrimaryKey(pk(integer, false))
		ref, err = resolveRowidRef(withAlias)
		require.NoError(t, err)
		require.Equal(t, &rowidRef{name: "id", alias: true}, ref)
	})

	t.Run("sourceRowidTable", func(t *testing.T) {
		// Added columns disappear, dropped columns come back and renames are reverted.
		to := schema.NewTable("users").AddColumns(
			col("kept", "integer"), col("extra", "integer"),
		)
		src := sourceRowidTable(to, []schema.Change{
			&schema.AddColumn{C: col("extra", "integer")},
			&schema.DropColumn{C: col("gone", "text")},
			&schema.RenameColumn{From: schema.NewColumn("old"), To: schema.NewColumn("kept")},
		})
		require.True(t, isRowidTable(src))
		names := map[string]bool{}
		for _, c := range src.Columns {
			names[c.Name] = true
		}
		// The renamed column reverts to its old name; the added column is removed.
		require.Equal(t, map[string]bool{"gone": true, "old": true}, names)

		// WITHOUT ROWID added on the target means the source stored a rowid.
		src = sourceRowidTable(schema.NewTable("users").AddAttrs(&WithoutRowID{}), []schema.Change{
			&schema.AddAttr{A: &WithoutRowID{}},
		})
		require.True(t, isRowidTable(src))

		// WITHOUT ROWID dropped on the target means the source had no hidden rowid.
		src = sourceRowidTable(schema.NewTable("users"), []schema.Change{
			&schema.DropAttr{A: &WithoutRowID{}},
		})
		require.False(t, isRowidTable(src))

		// Primary key changes are reflected on the reconstructed source table.
		integer := col("id", "INTEGER")
		srcPK := &schema.Index{Unique: true, Parts: []*schema.IndexPart{{C: integer}}}
		src = sourceRowidTable(schema.NewTable("users").AddColumns(integer), []schema.Change{
			&schema.DropPrimaryKey{P: srcPK},
		})
		require.Equal(t, srcPK, src.PrimaryKey)
		src = sourceRowidTable(schema.NewTable("users").AddColumns(integer).SetPrimaryKey(pk(integer, false)), []schema.Change{
			&schema.AddPrimaryKey{P: pk(integer, false)},
		})
		require.Nil(t, src.PrimaryKey)
	})

	t.Run("rowidCopyPlan", func(t *testing.T) {
		integer := col("id", "INTEGER")
		aliasT := func(name string) *schema.Table {
			return schema.NewTable(name).AddColumns(integer).SetPrimaryKey(pk(integer, false))
		}
		plainT := func(name string) *schema.Table {
			return schema.NewTable(name).AddColumns(col("v", "text"))
		}
		wrT := func(name string) *schema.Table {
			return schema.NewTable(name).AddColumns(integer).SetPrimaryKey(pk(integer, false)).AddAttrs(&WithoutRowID{})
		}

		// Both WITHOUT ROWID: neither side has a hidden rowid.
		from, to, copy, err := rowidCopyPlan(wrT("a"), wrT("b"))
		require.NoError(t, err)
		require.False(t, copy)
		require.Nil(t, from)
		require.Nil(t, to)
		// Storage model switch: exactly one side resolves a reference, so nothing is injected.
		from, to, copy, err = rowidCopyPlan(plainT("a"), wrT("b"))
		require.NoError(t, err)
		require.False(t, copy)
		require.NotNil(t, from)
		require.Nil(t, to)
		from, to, copy, err = rowidCopyPlan(wrT("a"), aliasT("b"))
		require.NoError(t, err)
		require.False(t, copy)
		require.Nil(t, from)
		require.NotNil(t, to)
		// Both expose a true alias: the declared mapping already preserves identity.
		from, to, copy, err = rowidCopyPlan(aliasT("a"), aliasT("b"))
		require.NoError(t, err)
		require.False(t, copy)
		require.True(t, from.alias && to.alias)

		// Any rowid table without an alias pair gets explicit pseudo columns.
		from, to, copy, err = rowidCopyPlan(plainT("a"), plainT("b"))
		require.NoError(t, err)
		require.True(t, copy)
		require.Equal(t, "_rowid_", from.name)
		require.Equal(t, "_rowid_", to.name)

		from, to, copy, err = rowidCopyPlan(aliasT("a"), plainT("b"))
		require.NoError(t, err)
		require.True(t, copy)
		require.True(t, from.alias)
		require.False(t, to.alias)

		// Unresolvable side fails the plan.
		blocked := schema.NewTable("a").AddColumns(
			col("_rowid_", "integer"), col("rowid", "integer"), col("oid", "integer"), col("v", "text"),
		)
		_, _, _, err = rowidCopyPlan(blocked, plainT("b"))
		require.ErrorContains(t, err, "shadowed by declared columns")
	})
}

func columnNames(t *schema.Table) []string {
	names := make([]string, len(t.Columns))
	for i, c := range t.Columns {
		names[i] = c.Name
	}
	return names
}

// removeWithoutRowID returns the attributes without any WithoutRowID attribute.
func removeWithoutRowID(attrs []schema.Attr) []schema.Attr {
	out := make([]schema.Attr, 0, len(attrs))
	for _, a := range attrs {
		if _, ok := a.(*WithoutRowID); !ok {
			out = append(out, a)
		}
	}
	return out
}
