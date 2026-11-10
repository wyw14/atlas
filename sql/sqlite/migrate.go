// Copyright 2021-present The Atlas Authors. All rights reserved.
// This source code is licensed under the Apache 2.0 license found
// in the LICENSE file in the root directory of this source tree.

package sqlite

import (
	"context"
	"fmt"
	"regexp"
	"slices"
	"strings"

	"ariga.io/atlas/sql/internal/sqlx"
	"ariga.io/atlas/sql/migrate"
	"ariga.io/atlas/sql/schema"
)

// DefaultPlan provides basic planning capabilities for SQLite dialects.
// Note, it is recommended to call Open, create a new Driver and use its
// migrate.PlanApplier when a database connection is available.
var DefaultPlan migrate.PlanApplier = &planApply{conn: &conn{ExecQuerier: sqlx.NoRows}}

// A planApply provides migration capabilities for schema elements.
type planApply struct{ *conn }

// PlanChanges returns a migration plan for the given schema changes.
func (p *planApply) PlanChanges(ctx context.Context, name string, changes []schema.Change, opts ...migrate.PlanOption) (*migrate.Plan, error) {
	s := &state{
		conn: p.conn,
		Plan: migrate.Plan{
			Name:          name,
			Transactional: true,
		},
		PlanOptions: migrate.PlanOptions{
			// Currently, the driver does not support attached
			// schemas and assumed the connected schema is "main".
			SchemaQualifier: new(string),
		},
	}
	for _, o := range opts {
		o(&s.PlanOptions)
	}
	if err := s.plan(ctx, changes); err != nil {
		return nil, err
	}
	if err := sqlx.SetReversible(&s.Plan); err != nil {
		return nil, err
	}
	// Disable foreign-keys enforcement if it is required
	// by one of the changes in the plan.
	if s.skipFKs {
		// Callers should note that these 2 pragmas are no-op in transactions,
		// See: https://sqlite.org/pragma.html#pragma_foreign_keys.
		s.Changes = append([]*migrate.Change{{Cmd: "PRAGMA foreign_keys = off", Comment: "disable the enforcement of foreign-keys constraints"}}, s.Changes...)
		s.append(&migrate.Change{Cmd: "PRAGMA foreign_keys = on", Comment: "enable back the enforcement of foreign-keys constraints"})
	}
	return &s.Plan, nil
}

// ApplyChanges applies the changes on the database. An error is returned
// if the driver is unable to produce a plan to it, or one of the statements
// is failed or unsupported.
func (p *planApply) ApplyChanges(ctx context.Context, changes []schema.Change, opts ...migrate.PlanOption) error {
	return sqlx.ApplyChanges(ctx, changes, p, opts...)
}

// state represents the state of a planning. It's not part of
// planApply so that multiple planning/applying can be called
// in parallel.
type state struct {
	*conn
	migrate.Plan
	migrate.PlanOptions
	skipFKs bool
}

// Exec executes the changes on the database. An error is returned
// if one of the operations fail, or a change is not supported.
func (s *state) plan(ctx context.Context, changes []schema.Change) (err error) {
	for _, c := range changes {
		switch c := c.(type) {
		case *schema.AddTable:
			err = s.addTable(ctx, c)
		case *schema.DropTable:
			err = s.dropTable(ctx, c)
		case *schema.ModifyTable:
			err = s.modifyTable(ctx, c)
		case *schema.RenameTable:
			s.renameTable(c)
		default:
			err = fmt.Errorf("unsupported change %T", c)
		}
		if err != nil {
			return err
		}
	}
	return nil
}

// addTable builds and executes the query for creating a table in a schema.
func (s *state) addTable(ctx context.Context, add *schema.AddTable) error {
	var (
		errs []string
		b    = s.Build("CREATE TABLE").Table(add.T)
	)
	if sqlx.Has(add.Extra, &schema.IfNotExists{}) {
		b.P("IF NOT EXISTS")
	}
	b.WrapIndent(func(b *sqlx.Builder) {
		b.MapIndent(add.T.Columns, func(i int, b *sqlx.Builder) {
			if err := s.column(b, add.T, add.T.Columns[i]); err != nil {
				errs = append(errs, err.Error())
			}
		})
		// Primary keys with auto-increment, or the historical inlined
		// "INTEGER PRIMARY KEY DESC" quirk, are rendered on the column definition.
		if pk := add.T.PrimaryKey; pk != nil && !autoincPK(pk) && !descRowidPK(add.T) {
			b.Comma().NL().P("PRIMARY KEY")
			s.indexParts(b, pk.Parts)
		}
		if len(add.T.ForeignKeys) > 0 {
			b.Comma()
			s.fks(b, add.T.ForeignKeys...)
		}
		for _, attr := range add.T.Attrs {
			if c, ok := attr.(*schema.Check); ok {
				b.Comma().NL()
				check(b, c)
			}
		}
	})
	if len(errs) > 0 {
		return fmt.Errorf("create table %q: %s", add.T.Name, strings.Join(errs, ", "))
	}
	var options []string
	if sqlx.Has(add.T.Attrs, &WithoutRowID{}) {
		options = append(options, "WITHOUT ROWID")
	}
	if sqlx.Has(add.T.Attrs, &Strict{}) {
		options = append(options, "STRICT")
	}
	b.MapComma(options, func(i int, b *sqlx.Builder) {
		b.P(options[i])
	})
	s.append(&migrate.Change{
		Cmd:     b.String(),
		Source:  add,
		Reverse: s.Build("DROP TABLE").Table(add.T).String(),
		Comment: fmt.Sprintf("create %q table", add.T.Name),
	})
	if err := s.tableSeq(ctx, add); err != nil {
		return err
	}
	return s.addIndexes(add.T, add.T.Indexes...)
}

// dropTable builds and executes the query for dropping a table from a schema.
func (s *state) dropTable(ctx context.Context, drop *schema.DropTable) error {
	rs := &state{conn: s.conn, PlanOptions: s.PlanOptions}
	if err := rs.addTable(ctx, &schema.AddTable{T: drop.T}); err != nil {
		return fmt.Errorf("calculate reverse for drop table %q: %w", drop.T.Name, err)
	}
	s.skipFKs = true
	b := s.Build("DROP TABLE")
	if sqlx.Has(drop.Extra, &schema.IfExists{}) {
		b.P("IF EXISTS")
	}
	b.Ident(drop.T.Name)
	s.append(&migrate.Change{
		Cmd:     b.String(),
		Source:  drop,
		Comment: fmt.Sprintf("drop %q table", drop.T.Name),
		// The reverse of 'DROP TABLE' might be a multi
		// statement operation. e.g., table with indexes.
		Reverse: func() any {
			cmd := make([]string, len(rs.Changes))
			for i, c := range rs.Changes {
				cmd[i] = c.Cmd
			}
			if len(cmd) == 1 {
				return cmd[0]
			}
			return cmd
		}(),
	})
	return nil
}

// modifyTable builds and executes the queries for bringing the table into its modified state.
// If the modification contains changes that are not index creation/deletion or a simple column
// addition, the changes are applied using a temporary table following the procedure mentioned
// in: https://www.sqlite.org/lang_altertable.html#making_other_kinds_of_table_schema_changes.
func (s *state) modifyTable(ctx context.Context, modify *schema.ModifyTable) error {
	if alterable(modify) {
		return s.alterTable(modify)
	}
	s.skipFKs = true
	newT := *modify.T
	indexes := newT.Indexes
	newT.Indexes = nil
	newT.Name = "new_" + newT.Name
	// Resolve how the hidden rowid (if any) is preserved before emitting
	// any statement, so that an unresolvable case fails the plan without
	// creating a temporary table or dropping the original one. ModifyTable.T
	// carries the target shape, so the source table shape is reconstructed
	// from the column and attribute changes.
	fromRef, toRef, copyRowID, err := rowidCopyPlan(sourceRowidTable(modify.T, modify.Changes), &newT)
	if err != nil {
		return err
	}
	// Create a new table with a temporary name, and copy the existing rows to it.
	if err := s.addTable(ctx, &schema.AddTable{T: &newT}); err != nil {
		return err
	}
	copied, err := s.copyRows(modify.T, &newT, modify.Changes, fromRef, toRef, copyRowID)
	if err != nil {
		return err
	}
	// Drop the current table, and rename the new one to its real name.
	s.append(&migrate.Change{
		Cmd:    s.Build("DROP TABLE").Ident(modify.T.Name).String(),
		Source: modify,
		Comment: fmt.Sprintf("drop %q table %s", modify.T.Name, func() string {
			if copied {
				return "after copying rows"
			}
			return "without copying rows (no columns)"
		}()),
	})
	s.append(&migrate.Change{
		Cmd:     s.Build("ALTER TABLE").Ident(newT.Name).P("RENAME TO").Ident(modify.T.Name).String(),
		Source:  modify,
		Comment: fmt.Sprintf("rename temporary table %q to %q", newT.Name, modify.T.Name),
	})
	return s.addIndexes(modify.T, indexes...)
}

func (s *state) renameTable(c *schema.RenameTable) {
	s.append(&migrate.Change{
		Source:  c,
		Comment: fmt.Sprintf("rename a table from %q to %q", c.From.Name, c.To.Name),
		Cmd:     s.Build("ALTER TABLE").Table(c.From).P("RENAME TO").Table(c.To).String(),
		Reverse: s.Build("ALTER TABLE").Table(c.To).P("RENAME TO").Table(c.From).String(),
	})
}

func (s *state) column(b *sqlx.Builder, t *schema.Table, c *schema.Column) error {
	tt, err := FormatType(c.Type.Type)
	if err != nil {
		return err
	}
	b.Ident(c.Name).P(tt)
	if !c.Type.Null {
		b.P("NOT")
	}
	b.P("NULL")
	if c.Default != nil {
		x, err := defaultValue(c)
		if err != nil {
			return err
		}
		b.P("DEFAULT", x)
	}
	switch hasA, hasX := sqlx.Has(c.Attrs, &AutoIncrement{}), sqlx.Has(c.Attrs, &schema.GeneratedExpr{}); {
	case hasA && hasX:
		return fmt.Errorf("both autoincrement and generation expression specified for column %q", c.Name)
	case hasA:
		b.P("PRIMARY KEY AUTOINCREMENT")
	case hasX:
		x := &schema.GeneratedExpr{}
		sqlx.Has(c.Attrs, x)
		b.P("AS", sqlx.MayWrap(x.Expr), x.Type)
	}
	// Keep the historical "INTEGER PRIMARY KEY DESC" quirk as an inlined
	// column constraint, otherwise an exact INTEGER column would become a
	// rowid alias when rebuilt through a table-level primary key.
	if t != nil && descRowidPK(t) && t.PrimaryKey.Parts[0].C == c {
		b.P("PRIMARY KEY DESC")
	}
	return nil
}

func (s *state) dropIndexes(t *schema.Table, indexes ...*schema.Index) error {
	rs := &state{conn: s.conn}
	if err := rs.addIndexes(t, indexes...); err != nil {
		return err
	}
	for i := range rs.Changes {
		s.append(&migrate.Change{
			Cmd:     rs.Changes[i].Reverse.(string),
			Reverse: rs.Changes[i].Cmd,
			Comment: fmt.Sprintf("drop index %q from table: %q", indexes[i].Name, t.Name),
		})
	}
	return nil
}

func (s *state) addIndexes(t *schema.Table, indexes ...*schema.Index) error {
	for _, idx := range indexes {
		if err := normalizeIdxName(idx, t); err != nil {
			return err
		}
		b := s.Build("CREATE")
		if idx.Unique {
			b.P("UNIQUE")
		}
		b.P("INDEX")
		if idx.Name != "" {
			b.Ident(idx.Name)
		}
		b.P("ON").Ident(t.Name)
		s.indexParts(b, idx.Parts)
		if p := (IndexPredicate{}); sqlx.Has(idx.Attrs, &p) {
			b.P("WHERE").P(p.P)
		}
		s.append(&migrate.Change{
			Cmd:     b.String(),
			Source:  &schema.AddIndex{I: idx},
			Reverse: s.Build("DROP INDEX").Ident(idx.Name).String(),
			Comment: fmt.Sprintf("create index %q to table: %q", idx.Name, t.Name),
		})
	}
	return nil
}

// normalizeIdxName normalizes the index name before generating the CREATE INDEX statement.
func normalizeIdxName(idx *schema.Index, t *schema.Table) error {
	// PRIMARY KEY or UNIQUE columns automatically create indexes with the generated name.
	// See: sqlite/build.c#sqlite3CreateIndex. Therefore, we ignore such PKs, but create
	// the inlined UNIQUE constraints manually with custom name, because SQLite does not
	// allow creating indexes with such names manually. Note, this case is possible if
	// "apply" schema that was inspected from the database as-is.
	if strings.HasPrefix(idx.Name, "sqlite_autoindex") {
		if i := (IndexOrigin{}); sqlx.Has(idx.Attrs, &i) && i.O == "p" {
			return nil
		}
		// Use the following format: <Table>_<Columns>.
		names := make([]string, len(idx.Parts)+1)
		names[0] = t.Name
		for i, p := range idx.Parts {
			if p.C == nil {
				return fmt.Errorf("unexpected index part %s (%d)", idx.Name, i)
			}
			names[i+1] = p.C.Name
		}
		idx.Name = strings.Join(names, "_")
	}
	return nil
}

func (s *state) indexParts(b *sqlx.Builder, parts []*schema.IndexPart) {
	b.Wrap(func(b *sqlx.Builder) {
		b.MapComma(parts, func(i int, b *sqlx.Builder) {
			switch part := parts[i]; {
			case part.C != nil:
				b.Ident(part.C.Name)
			case part.X != nil:
				b.WriteString(sqlx.MayWrap(part.X.(*schema.RawExpr).X))
			}
			if parts[i].Desc {
				b.P("DESC")
			}
		})
	})
}

func (s *state) fks(b *sqlx.Builder, fks ...*schema.ForeignKey) {
	b.MapIndent(fks, func(i int, b *sqlx.Builder) {
		fk := fks[i]
		if fk.Symbol != "" {
			b.P("CONSTRAINT").Ident(fk.Symbol)
		}
		b.P("FOREIGN KEY")
		b.Wrap(func(b *sqlx.Builder) {
			b.MapComma(fk.Columns, func(i int, b *sqlx.Builder) {
				b.Ident(fk.Columns[i].Name)
			})
		})
		b.P("REFERENCES").Ident(fk.RefTable.Name)
		b.Wrap(func(b *sqlx.Builder) {
			b.MapComma(fk.RefColumns, func(i int, b *sqlx.Builder) {
				b.Ident(fk.RefColumns[i].Name)
			})
		})
		if fk.OnUpdate != "" {
			b.P("ON UPDATE", string(fk.OnUpdate))
		}
		if fk.OnDelete != "" {
			b.P("ON DELETE", string(fk.OnDelete))
		}
	})
}

func (s *state) copyRows(from *schema.Table, to *schema.Table, changes []schema.Change, fromRowID, toRowID *rowidRef, copyRowID bool) (bool, error) {
	var fromC, toC []string
	for _, column := range to.Columns {
		// Skip generated columns in INSERT as they are computed.
		if sqlx.Has(column.Attrs, &schema.GeneratedExpr{}) {
			continue
		}
		// Find a change that associated with this column, if exists.
		var change schema.Change
		for i := range changes {
			switch c := changes[i].(type) {
			case *schema.AddColumn:
				if c.C.Name != column.Name {
					break
				}
				if change != nil {
					return false, fmt.Errorf("duplicate changes for column: %q: %T, %T", column.Name, change, c)
				}
				change = changes[i]
			case *schema.ModifyColumn:
				if c.To.Name != column.Name {
					break
				}
				if change != nil {
					return false, fmt.Errorf("duplicate changes for column: %q: %T, %T", column.Name, change, c)
				}
				change = changes[i]
			case *schema.RenameColumn:
				if c.To.Name != column.Name {
					break
				}
				if change != nil {
					return false, fmt.Errorf("duplicate changes for column: %q: %T, %T", column.Name, change, c)
				}
				change = changes[i]
			case *schema.DropColumn:
				if c.C.Name == column.Name {
					return false, fmt.Errorf("unexpected drop column: %q", column.Name)
				}
			}
		}
		switch change := change.(type) {
		// We expect that new columns are added with DEFAULT/GENERATED
		// values or defined as nullable if the table is not empty.
		case *schema.AddColumn:
		// Column modification requires special handling if it was
		// converted from nullable to non-nullable with default value.
		case *schema.ModifyColumn:
			toC = append(toC, column.Name)
			if !column.Type.Null && column.Default != nil && change.Change.Is(schema.ChangeNull|schema.ChangeDefault) {
				x, err := defaultValue(column)
				if err != nil {
					return false, err
				}
				fromC = append(fromC, fmt.Sprintf("IFNULL(`%[1]s`, %s) AS `%[1]s`", column.Name, x))
			} else {
				fromC = append(fromC, column.Name)
			}
		case *schema.RenameColumn:
			toC = append(toC, change.To.Name)
			fromC = append(fromC, change.From.Name)
		// Columns without changes should be transferred as-is.
		case nil:
			toC = append(toC, column.Name)
			fromC = append(fromC, column.Name)
		}
	}
	insert := len(toC) > 0
	if insert {
		// Preserve the hidden rowid of rowid tables. When the target table
		// exposes its rowid through a declared INTEGER PRIMARY KEY alias that
		// is already copied, feed the source rowid into the existing column
		// position. Otherwise, prepend the resolved rowid identifiers to both
		// column lists and let the database set the rowid explicitly.
		if copyRowID {
			if i := slices.Index(toC, toRowID.name); toRowID.alias && i != -1 {
				fromC[i] = fromRowID.name
			} else {
				fromC = append([]string{fromRowID.name}, fromC...)
				toC = append([]string{toRowID.name}, toC...)
			}
		}
		s.append(&migrate.Change{
			Cmd: fmt.Sprintf(
				"INSERT INTO `%s` (%s) SELECT %s FROM `%s`",
				to.Name, identComma(toC), identComma(fromC), from.Name,
			),
			Comment: fmt.Sprintf("copy rows from old table %q to new temporary table %q", from.Name, to.Name),
		})
	}
	return insert, nil
}

// rowidPseudoNames are the special case-insensitive names that can be used
// to reference the hidden rowid of a rowid table, unless a user-defined
// column with the same name shadows them. "_rowid_" is tried first as it is
// the conventional escape hatch and is rarely used as a column name.
// See: https://www.sqlite.org/lang_createtable.html#rowid.
var rowidPseudoNames = []string{"_rowid_", "rowid", "oid"}

// rePKDescColumn matches an inlined descending "INTEGER PRIMARY KEY DESC"
// column constraint. Such a column is intentionally not made a rowid alias
// by SQLite due to a long-standing quirk that is preserved for compatibility.
// Note that a table-level "PRIMARY KEY (col DESC)" constraint does not carry
// the same quirk and keeps an exact "INTEGER" column as a rowid alias.
var rePKDescColumn = regexp.MustCompile(`(?i)integer\s+[^,()]*primary\s+key\s+desc\b`)

// rowidRef describes how the hidden rowid of a rowid table can be
// referenced while rows are copied between tables.
type rowidRef struct {
	// name is the identifier used in SQL to read or write the hidden rowid.
	name string
	// alias reports whether name is a declared INTEGER PRIMARY KEY alias
	// column rather than one of the special rowid pseudo-columns.
	alias bool
}

// isRowidTable reports whether the table stores a hidden rowid, i.e. it is
// not declared with the WITHOUT ROWID option.
// See: https://www.sqlite.org/withoutrowid.html.
func isRowidTable(t *schema.Table) bool {
	return !sqlx.Has(t.Attrs, &WithoutRowID{})
}

// rowidAliasColumn returns the column that is an alias for the hidden rowid,
// or nil if the table has no such column.
//
// A column is a rowid alias only when the table is a rowid table, it forms a
// single-column PRIMARY KEY declared with the exact type "INTEGER", and the
// key is not descending. Types such as "INT" or "BIGINT" produce ordinary
// unique keys, and "INTEGER PRIMARY KEY DESC" is a SQLite quirk that prevents
// the alias from being created.
// See: https://www.sqlite.org/lang_createtable.html#rowid.
func rowidAliasColumn(t *schema.Table) *schema.Column {
	if !isRowidTable(t) || t.PrimaryKey == nil || len(t.PrimaryKey.Parts) != 1 {
		return nil
	}
	p := t.PrimaryKey.Parts[0]
	if p.C == nil || p.Desc || !strings.EqualFold(strings.TrimSpace(p.C.Type.Raw), "integer") {
		return nil
	}
	// Primary-key parts populated by inspection (pragma_table_xinfo) do not
	// carry the sort order, so detect the descending quirk from the original
	// CREATE statement when it is available.
	var c CreateStmt
	if sqlx.Has(t.Attrs, &c) && pkDescInStmt(c.S) {
		return nil
	}
	return p.C
}

// pkDescInStmt reports whether the CREATE statement declares an inlined
// descending "INTEGER PRIMARY KEY DESC" column constraint.
func pkDescInStmt(stmt string) bool {
	return rePKDescColumn.MatchString(stmt)
}

// descRowidPK reports whether the table has a single-column primary key that
// must be rendered as an inlined "PRIMARY KEY DESC" column constraint. Only
// the inlined form prevents an exact "INTEGER" column from becoming a rowid
// alias, so rebuilding such tables must keep the same syntax to preserve
// both the hidden rowid and the constraint semantics.
func descRowidPK(t *schema.Table) bool {
	if !isRowidTable(t) || t.PrimaryKey == nil || len(t.PrimaryKey.Parts) != 1 {
		return false
	}
	p := t.PrimaryKey.Parts[0]
	return p.C != nil && p.Desc && strings.EqualFold(strings.TrimSpace(p.C.Type.Raw), "integer")
}

// rowidPseudoColumn returns a special rowid name that is not shadowed by a
// declared column of the table. Column names are case-insensitive in SQLite,
// and a declared column named "rowid", "oid" or "_rowid_" always refers to
// the ordinary column instead of the hidden rowid. An empty string is
// returned when all three names are shadowed.
func rowidPseudoColumn(t *schema.Table) string {
	for _, name := range rowidPseudoNames {
		if !slices.ContainsFunc(t.Columns, func(c *schema.Column) bool {
			return strings.EqualFold(c.Name, name)
		}) {
			return name
		}
	}
	return ""
}

// resolveRowidRef returns a reference that is guaranteed to address the
// hidden rowid of the table rather than an ordinary declared column. It
// returns nil for WITHOUT ROWID tables, which have no hidden rowid. An error
// is returned when every special rowid name is shadowed by a declared column
// and the table does not have an INTEGER PRIMARY KEY alias to fall back to.
func resolveRowidRef(t *schema.Table) (*rowidRef, error) {
	if !isRowidTable(t) {
		return nil, nil
	}
	// A true INTEGER PRIMARY KEY alias column is the rowid itself, so prefer
	// it over the pseudo names whenever it exists.
	if c := rowidAliasColumn(t); c != nil {
		return &rowidRef{name: c.Name, alias: true}, nil
	}
	if name := rowidPseudoColumn(t); name != "" {
		return &rowidRef{name: name}, nil
	}
	return nil, fmt.Errorf(
		"sqlite: cannot preserve rowid of table %q during rebuild: the hidden rowid names %q, %q and %q are all shadowed by declared columns and the table has no INTEGER PRIMARY KEY alias",
		t.Name, "rowid", "oid", "_rowid_",
	)
}

// sourceRowidTable rebuilds the rowid-relevant shape of the source table that
// a temporary-table rebuild reads from. ModifyTable.T carries the target shape,
// therefore added columns are removed, dropped columns are restored, renamed
// columns are mapped back to their old names, and WITHOUT ROWID attribute
// changes are reversed to reflect the source storage model.
func sourceRowidTable(to *schema.Table, changes []schema.Change) *schema.Table {
	var (
		src   = &schema.Table{Name: to.Name}
		cols  = make(map[string]*schema.Column)
		rowid = isRowidTable(to)
	)
	for _, c := range to.Columns {
		cols[c.Name] = c
	}
	for _, ch := range changes {
		switch ch := ch.(type) {
		case *schema.AddColumn:
			delete(cols, ch.C.Name)
		case *schema.DropColumn:
			cols[ch.C.Name] = ch.C
		case *schema.ModifyColumn:
			// Keep the source column object, as its old type may decide
			// whether the source primary key was a rowid alias.
			cols[ch.From.Name] = ch.From
		case *schema.RenameColumn:
			delete(cols, ch.To.Name)
			cols[ch.From.Name] = ch.From
		case *schema.AddAttr:
			if _, ok := ch.A.(*WithoutRowID); ok {
				// The target became WITHOUT ROWID, hence the source had a rowid.
				rowid = true
			}
		case *schema.DropAttr:
			if _, ok := ch.A.(*WithoutRowID); ok {
				// The target dropped WITHOUT ROWID, hence the source had no rowid.
				rowid = false
			}
		}
	}
	for _, c := range cols {
		src.Columns = append(src.Columns, c)
	}
	if !rowid {
		src.Attrs = append(src.Attrs, &WithoutRowID{})
		return src
	}
	src.PrimaryKey = sourcePrimaryKey(to, changes, cols)
	return src
}

// sourcePrimaryKey reconstructs the primary key of the source table from the
// target key and the primary-key/rename changes involved in a rebuild.
func sourcePrimaryKey(to *schema.Table, changes []schema.Change, cols map[string]*schema.Column) *schema.Index {
	for _, ch := range changes {
		switch ch := ch.(type) {
		case *schema.AddPrimaryKey:
			// The key only exists in the target.
			return nil
		case *schema.DropPrimaryKey:
			// The change carries the inspected source key.
			return ch.P
		}
	}
	if to.PrimaryKey == nil {
		return nil
	}
	pk := *to.PrimaryKey
	pk.Parts = append([]*schema.IndexPart(nil), to.PrimaryKey.Parts...)
	for i, p := range pk.Parts {
		if p.C == nil {
			continue
		}
		name := p.C.Name
		for _, ch := range changes {
			if r, ok := ch.(*schema.RenameColumn); ok && r.To.Name == name {
				name = r.From.Name
			}
		}
		c, ok := cols[name]
		if !ok {
			// The key column did not exist in the source table.
			return nil
		}
		part := *p
		part.C = c
		pk.Parts[i] = &part
	}
	return &pk
}

// rowidCopyPlan decides whether and how the hidden rowid must be copied
// explicitly while a table is rebuilt. The returned references are resolved
// independently for the source and target tables because declared columns may
// shadow different rowid names on each side.
//
// No copy is planned when either table is WITHOUT ROWID (storage models
// differ, or both tables keep their explicit primary key), or when both
// tables expose a true INTEGER PRIMARY KEY alias column that is already
// transferred by the regular declared-column mapping.
func rowidCopyPlan(from, to *schema.Table) (fromRef, toRef *rowidRef, copy bool, err error) {
	if fromRef, err = resolveRowidRef(from); err != nil {
		return nil, nil, false, err
	}
	if toRef, err = resolveRowidRef(to); err != nil {
		return nil, nil, false, err
	}
	// At least one side has no hidden rowid: never reference a column that
	// does not exist on the other storage model.
	if fromRef == nil || toRef == nil {
		return fromRef, toRef, false, nil
	}
	// Both alias columns are copied as declared columns, hence inserting
	// their values already restores the rowid identity without extra SQL.
	if fromRef.alias && toRef.alias {
		return fromRef, toRef, false, nil
	}
	return fromRef, toRef, true, nil
}

// alterTable alters the table with the given changes. Assuming the changes are "alterable".
func (s *state) alterTable(modify *schema.ModifyTable) error {
	for _, change := range modify.Changes {
		switch change := change.(type) {
		case *schema.AddIndex:
			if err := s.addIndexes(modify.T, change.I); err != nil {
				return err
			}
		case *schema.DropIndex:
			if err := s.dropIndexes(modify.T, change.I); err != nil {
				return err
			}
		case *schema.RenameIndex:
			if err := s.addIndexes(modify.T, change.To); err != nil {
				return err
			}
			if err := s.dropIndexes(modify.T, change.From); err != nil {
				return err
			}
		case *schema.AddColumn:
			b := s.Build("ALTER TABLE").Ident(modify.T.Name)
			r := b.Clone()
			if err := s.column(b.P("ADD COLUMN"), nil, change.C); err != nil {
				return err
			}
			s.append(&migrate.Change{
				Source:  change,
				Cmd:     b.String(),
				Reverse: r.P("DROP COLUMN").Ident(change.C.Name).String(),
				Comment: fmt.Sprintf("add column %q to table: %q", change.C.Name, modify.T.Name),
			})
		case *schema.RenameColumn:
			b := s.Build("ALTER TABLE").Ident(modify.T.Name).P("RENAME COLUMN")
			r := b.Clone()
			s.append(&migrate.Change{
				Source:  change,
				Cmd:     b.Ident(change.From.Name).P("TO").Ident(change.To.Name).String(),
				Reverse: r.Ident(change.To.Name).P("TO").Ident(change.From.Name).String(),
				Comment: fmt.Sprintf("rename a column from %q to %q", change.From.Name, change.To.Name),
			})
		default:
			return fmt.Errorf("unexpected change in alter table: %T", change)
		}
	}
	return nil
}

// tableSeq sets the sequence value of the table if it was provided by
// the user on table creation.
func (s *state) tableSeq(ctx context.Context, add *schema.AddTable) error {
	var inc AutoIncrement
	switch pk := add.T.PrimaryKey; {
	// Sequence was set on table attributes.
	case sqlx.Has(add.T.Attrs, &inc) && inc.Seq > 0:
	// Sequence was set on table primary-key (a single column PK).
	case pk != nil && len(pk.Parts) == 1 && pk.Parts[0].C != nil && sqlx.Has(pk.Parts[0].C.Attrs, &inc) && inc.Seq > 0:
	default:
		return nil
	}
	// SQLite tracks the AUTOINCREMENT in the "sqlite_sequence" table that is created and initialized automatically
	// whenever the first "PRIMARY KEY AUTOINCREMENT" is created. However, rows in this table are populated after the
	// first insertion to the associated table (name, seq). Therefore, we check if the sequence table and the row exist,
	// and in case they are not, we insert a new non-zero sequence to it.
	rows, err := s.QueryContext(ctx, "SELECT seq FROM sqlite_sequence WHERE name = ?", add.T.Name)
	if err != nil || !rows.Next() {
		s.append(&migrate.Change{
			Cmd:     fmt.Sprintf("INSERT INTO sqlite_sequence (name, seq) VALUES (%q, %d)", add.T.Name, inc.Seq),
			Source:  add,
			Reverse: fmt.Sprintf("UPDATE sqlite_sequence SET seq = 0 WHERE name = %q", add.T.Name),
			Comment: fmt.Sprintf("set sequence for %q table", add.T.Name),
		})
	}
	if rows != nil {
		rows.Close()
		return rows.Err()
	}
	return nil
}

func (s *state) append(c *migrate.Change) {
	s.Changes = append(s.Changes, c)
}

func alterable(modify *schema.ModifyTable) bool {
	for _, change := range modify.Changes {
		switch change := change.(type) {
		case *schema.RenameColumn, *schema.RenameIndex, *schema.DropIndex, *schema.AddIndex:
		case *schema.AddColumn:
			if len(change.C.Indexes) > 0 || len(change.C.ForeignKeys) > 0 {
				return false
			}
			// If the column has a DEFAULT clause, it must be a constant value.
			// See: https://www.sqlite.org/lang_altertable.html#alter_table_add_column.
			switch x := change.C.Default.(type) {
			case *schema.Literal:
				if slices.Contains([]string{"CURRENT_TIME", "CURRENT_DATE", "CURRENT_TIMESTAMP"}, x.V) {
					return false
				}
			case *schema.RawExpr:
				return false
			}
			// Only VIRTUAL generated columns can be added using ALTER TABLE.
			if x := (schema.GeneratedExpr{}); sqlx.Has(change.C.Attrs, &x) && storedOrVirtual(x.Type) == stored {
				return false
			}
		default:
			return false
		}
	}
	return true
}

// checks writes the CHECK constraint to the builder.
func check(b *sqlx.Builder, c *schema.Check) {
	expr := c.Expr
	// Expressions should be wrapped with parens.
	if t := strings.TrimSpace(expr); !strings.HasPrefix(t, "(") || !strings.HasSuffix(t, ")") {
		expr = "(" + t + ")"
	}
	if c.Name != "" {
		b.P("CONSTRAINT").Ident(c.Name)
	}
	b.P("CHECK", expr)
}

func autoincPK(pk *schema.Index) bool {
	return sqlx.Has(pk.Attrs, &AutoIncrement{}) ||
		len(pk.Parts) == 1 && pk.Parts[0].C != nil && sqlx.Has(pk.Parts[0].C.Attrs, &AutoIncrement{})
}

// Build instantiates a new builder and writes the given phrase to it.
func (s *state) Build(phrases ...string) *sqlx.Builder {
	return (*Driver).StmtBuilder(nil, s.PlanOptions).
		P(phrases...)
}

func defaultValue(c *schema.Column) (string, error) {
	switch x := c.Default.(type) {
	case *schema.Literal:
		switch c.Type.Type.(type) {
		case *schema.BoolType, *schema.DecimalType, *schema.IntegerType, *schema.FloatType:
			return x.V, nil
		default:
			return sqlx.SingleQuote(x.V)
		}
	case *schema.RawExpr:
		return sqlx.MayWrap(x.X), nil
	default:
		return "", fmt.Errorf("unexpected default value type: %T", x)
	}
}

func identComma(c []string) string {
	b := &sqlx.Builder{QuoteOpening: '`', QuoteClosing: '`'}
	b.MapComma(c, func(i int, b *sqlx.Builder) {
		if strings.ContainsRune(c[i], '`') {
			b.WriteString(c[i])
		} else {
			b.Ident(c[i])
		}
	})
	return b.String()
}
