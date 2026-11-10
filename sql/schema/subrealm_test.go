// Copyright 2025-present The Atlas Authors. All rights reserved.
// This source code is licensed under the Apache 2.0 license found
// in the LICENSE file in the root directory of this source tree.

package schema_test

import (
	"errors"
	"fmt"
	"testing"

	"ariga.io/atlas/sql/schema"

	"github.com/stretchr/testify/require"
)

// extType is a driver-specific extension type that wraps an underlying
// (supported) type and carries dependencies SubRealm cannot determine.
type extType struct {
	schema.Type
	T string
}

func (e *extType) Underlying() schema.Type { return e.Type }

// buildRealm builds a realm that covers: cross-schema same-name tables,
// self-reference, an FK cycle, a composite FK, a shared declared enum, inline
// (MySQL-style) enums, an incoming reference that must not be pulled in, and
// a schema that is only reachable through an outgoing FK.
func buildRealm(t *testing.T) (*schema.Realm, map[string]*schema.Table) {
	t.Helper()
	// s1: accounts (shared enum "state"), orders -> accounts,
	// nodes (self reference), employees <-> depts cycle.
	s1 := schema.New("s1")
	// s2: a table with the same name "accounts" as in s1 but distinct,
	// referencing s1.orders across schemas.
	s2 := schema.New("s2")
	// s3: holds only a table that references s1; it must not be created
	// unless one of its tables is in the closure.
	s3 := schema.New("s3")

	stateS1 := &schema.EnumType{T: "state", Schema: s1, Values: []string{"on", "off"}}
	stateS2 := &schema.EnumType{T: "state", Schema: s2, Values: []string{"a", "b"}}
	s1.AddObjects(stateS1)
	s2.AddObjects(stateS2)

	// s1.accounts uses the shared, declared enum.
	id1 := schema.NewIntColumn("id", "int")
	stateCol := schema.NewColumn("state").SetType(stateS1)
	accounts1 := schema.NewTable("accounts").AddColumns(id1, stateCol)
	accounts1.SetPrimaryKey(schema.NewPrimaryKey(id1))
	accounts1.SetComment("s1 accounts")
	accounts1.AddChecks(schema.NewCheck().SetName("state_known").SetExpr("(`state` in ('on','off'))"))
	accounts1.AddIndexes(schema.NewIndex("idx_accounts_state").AddColumns(stateCol))

	orders := schema.NewTable("orders").AddColumns(
		schema.NewIntColumn("id", "int"),
		schema.NewIntColumn("account_id", "int"),
		schema.NewNullStringColumn("kind", "varchar", schema.StringSize(16)),
	)
	orders.SetPrimaryKey(schema.NewPrimaryKey(mustCol(orders, "id")))
	orders.AddForeignKeys(
		schema.NewForeignKey("fk_orders_accounts").
			SetRefTable(accounts1).
			AddColumns(mustCol(orders, "account_id")).
			AddRefColumns(id1),
	)
	// Inline enum column (MySQL-style): independent per column.
	orders.AddColumns(schema.NewEnumColumn("priority", schema.EnumValues("low", "high")))
	orders.AddChecks(schema.NewCheck().SetExpr("(`id` > 0)"))
	orders.AddColumns(schema.NewIntColumn("tenant_id", "int"))
	orders.AddIndexes(schema.NewUniqueIndex("uniq_orders_tenant_id").
		AddColumns(mustCol(orders, "tenant_id"), mustCol(orders, "id")))

	// Nodes with a self reference.
	nodes := schema.NewTable("nodes").AddColumns(
		schema.NewIntColumn("id", "int"),
		schema.NewNullIntColumn("parent_id", "int"),
	)
	nodes.SetPrimaryKey(schema.NewPrimaryKey(mustCol(nodes, "id")))
	nodes.AddForeignKeys(
		schema.NewForeignKey("fk_nodes_parent").
			SetRefTable(nodes).
			AddColumns(mustCol(nodes, "parent_id")).
			AddRefColumns(mustCol(nodes, "id")),
	)

	// employees <-> depts form an FK cycle.
	employees := schema.NewTable("employees").AddColumns(
		schema.NewIntColumn("id", "int"), schema.NewNullIntColumn("dept_id", "int"))
	employees.SetPrimaryKey(schema.NewPrimaryKey(mustCol(employees, "id")))
	depts := schema.NewTable("depts").AddColumns(
		schema.NewIntColumn("id", "int"), schema.NewNullIntColumn("manager_id", "int"))
	depts.SetPrimaryKey(schema.NewPrimaryKey(mustCol(depts, "id")))
	employees.AddForeignKeys(
		schema.NewForeignKey("fk_emp_dept").
			SetRefTable(depts).
			AddColumns(mustCol(employees, "dept_id")).
			AddRefColumns(mustCol(depts, "id")),
	)
	depts.AddForeignKeys(
		schema.NewForeignKey("fk_dept_manager").
			SetRefTable(employees).
			SetOnDelete(schema.Cascade).
			AddColumns(mustCol(depts, "manager_id")).
			AddRefColumns(mustCol(employees, "id")),
	)

	// order_items has a composite FK to orders: pairing order must be
	// preserved (tenant first, id second).
	orderItems := schema.NewTable("order_items").AddColumns(
		schema.NewIntColumn("id", "int"),
		schema.NewIntColumn("tenant_id", "int"),
		schema.NewIntColumn("order_id", "int"),
	)
	orderItems.SetPrimaryKey(schema.NewPrimaryKey(mustCol(orderItems, "id")))
	orderItems.AddForeignKeys(
		schema.NewForeignKey("fk_items_order").
			SetRefTable(orders).
			AddColumns(mustCol(orderItems, "tenant_id"), mustCol(orderItems, "order_id")).
			AddRefColumns(mustCol(orders, "tenant_id"), mustCol(orders, "id")),
	)

	s1.AddTables(accounts1, orders, nodes, employees, depts, orderItems)

	// s2.accounts has the same simple name as s1.accounts; it holds a column
	// typed by the s2 shared enum and an FK across schemas to s1.orders.
	accounts2 := schema.NewTable("accounts").AddColumns(
		schema.NewIntColumn("id", "int"),
		schema.NewColumn("remote_state").SetType(stateS2),
		schema.NewNullIntColumn("order_id", "int"),
	)
	accounts2.SetPrimaryKey(schema.NewPrimaryKey(mustCol(accounts2, "id")))
	accounts2.AddForeignKeys(
		schema.NewForeignKey("fk_s2_orders").
			SetRefTable(orders).
			AddColumns(mustCol(accounts2, "order_id")).
			AddRefColumns(mustCol(orders, "id")),
	)
	s2.AddTables(accounts2)

	// audits references orders: an incoming reference that must NOT pull the
	// other direction when extracting from orders.
	audits := schema.NewTable("audits").AddColumns(
		schema.NewIntColumn("id", "int"), schema.NewNullIntColumn("order_id", "int"))
	audits.SetPrimaryKey(schema.NewPrimaryKey(mustCol(audits, "id")))
	audits.AddForeignKeys(
		schema.NewForeignKey("fk_audit_order").
			SetRefTable(orders).
			AddColumns(mustCol(audits, "order_id")).
			AddRefColumns(mustCol(orders, "id")),
	)
	s3.AddTables(audits)

	realm := schema.NewRealm(s1, s2, s3)
	return realm, map[string]*schema.Table{
		"s1.accounts":    accounts1,
		"s1.orders":      orders,
		"s1.nodes":       nodes,
		"s1.employees":   employees,
		"s1.depts":       depts,
		"s1.order_items": orderItems,
		"s2.accounts":    accounts2,
		"s3.audits":      audits,
	}
}

func TestSubRealm_ClosureAndGraph(t *testing.T) {
	realm, _ := buildRealm(t)
	roots := []schema.TableRef{
		{Schema: "s1", Table: "order_items"},
		{Schema: "s2", Table: "accounts"},
		{Schema: "s1", Table: "nodes"},
		{Schema: "s1", Table: "depts"},
	}
	sub, err := realm.SubRealm(roots...)
	require.NoError(t, err)

	// Only schemas s1 and s2 are needed; s3 only holds an incoming reference.
	require.Equal(t, []string{"s1", "s2"}, schemaNames(sub))
	require.Equal(t, []string{
		"accounts", "orders", "nodes", "employees", "depts", "order_items",
	}, tableNames(t, sub.Schemas[0]))
	require.Equal(t, []string{"accounts"}, tableNames(t, sub.Schemas[1]))

	// Enums: s1.state (used by accounts), s2.state (used by s2.accounts).
	var enumNames []string
	for _, s := range sub.Schemas {
		for _, o := range s.Objects {
			e, ok := o.(*schema.EnumType)
			require.True(t, ok)
			enumNames = append(enumNames, s.Name+"."+e.T)
		}
	}
	require.Equal(t, []string{"s1.state", "s2.state"}, enumNames)

	// The audits table references orders, but the outgoing direction never
	// pulls auditing in.
	require.NotContains(t, tableNames(t, sub.Schemas[0]), "audits")
	require.Empty(t, sub.Objects)
	require.Empty(t, sub.Schemas[0].Views)

	// Shared enum identity: all references point to the single output object,
	// and same-name enums from different schemas are not merged.
	require.Len(t, sub.Schemas[0].Objects, 1)
	require.Len(t, sub.Schemas[1].Objects, 1)
	s1Enum := sub.Schemas[0].Objects[0].(*schema.EnumType)
	s2Enum := sub.Schemas[1].Objects[0].(*schema.EnumType)
	require.Equal(t, []string{"on", "off"}, s1Enum.Values)
	require.Equal(t, []string{"a", "b"}, s2Enum.Values)
	require.NotSame(t, s1Enum, s2Enum)
	require.Same(t, sub.Schemas[0], s1Enum.Schema)
	require.Same(t, sub.Schemas[1], s2Enum.Schema)
	orders := findTable(t, sub, "s1", "orders")
	accounts := findTable(t, sub, "s1", "accounts")
	require.Same(t, s1Enum, mustCol(accounts, "state").Type.Type)

	// Inline enums are cloned per column and stay schema-local.
	priority := mustCol(orders, "priority").Type.Type.(*schema.EnumType)
	require.Equal(t, []string{"low", "high"}, priority.Values)
	require.Nil(t, priority.Schema)
	for _, o := range sub.Schemas[0].Objects {
		require.NotSame(t, o, priority)
	}

	// Composite FK pairing order is preserved.
	items := findTable(t, sub, "s1", "order_items")
	fk := mustFK(items, "fk_items_order")
	require.Same(t, orders, fk.RefTable)
	require.Equal(t, []string{"tenant_id", "order_id"}, columnNames(fk.Columns))
	require.Equal(t, []string{"tenant_id", "id"}, columnNames(fk.RefColumns))

	// Self reference survives and points inside the graph.
	nodes := findTable(t, sub, "s1", "nodes")
	selfFK := mustFK(nodes, "fk_nodes_parent")
	require.Same(t, nodes, selfFK.RefTable)

	// Cycle survives.
	emps := findTable(t, sub, "s1", "employees")
	depts := findTable(t, sub, "s1", "depts")
	require.Same(t, depts, mustFK(emps, "fk_emp_dept").RefTable)
	require.Same(t, emps, mustFK(depts, "fk_dept_manager").RefTable)
	require.Equal(t, schema.Cascade, mustFK(depts, "fk_dept_manager").OnDelete)

	// Checks, comments and indexes (with parts) are preserved.
	require.Equal(t, "s1 accounts", tableComment(accounts))
	require.Len(t, accounts.Checks(), 1)
	idx, ok := accounts.Index("idx_accounts_state")
	require.True(t, ok)
	require.Len(t, idx.Parts, 1)
	require.Equal(t, "state", idx.Parts[0].C.Name)
	require.Same(t, accounts, idx.Table)
	require.Len(t, orders.Checks(), 1)
	require.NotNil(t, orders.PrimaryKey)
	require.Equal(t, []string{"id"}, pkColumns(orders.PrimaryKey))

	// Nothing in the output points back at the input realm or its objects.
	assertGraphIsolation(t, realm, sub)
}

func TestSubRealm_DeterministicOrder(t *testing.T) {
	realm, _ := buildRealm(t)
	orderA := []schema.TableRef{
		{Schema: "s1", Table: "order_items"},
		{Schema: "s2", Table: "accounts"},
		{Schema: "s1", Table: "nodes"},
		{Schema: "s1", Table: "depts"},
	}
	orderB := []schema.TableRef{
		{Schema: "s1", Table: "depts"},
		{Schema: "s1", Table: "nodes"},
		{Schema: "s2", Table: "accounts"},
		{Schema: "s1", Table: "order_items"},
	}
	subA, err := realm.SubRealm(orderA...)
	require.NoError(t, err)
	subB, err := realm.SubRealm(orderB...)
	require.NoError(t, err)
	require.Equal(t, subA, subB)

	// Empty roots produce an empty, but valid, realm.
	empty, err := realm.SubRealm()
	require.NoError(t, err)
	require.NotNil(t, empty)
	require.Empty(t, empty.Schemas)
}

func TestSubRealm_DoesNotMutateInput(t *testing.T) {
	realm, _ := buildRealm(t)
	before := dumpRealm(realm)
	_, err := realm.SubRealm(schema.TableRef{Schema: "s1", Table: "orders"})
	require.NoError(t, err)
	require.Equal(t, before, dumpRealm(realm))
}

func TestSubRealm_Errors(t *testing.T) {
	t.Run("root schema missing", func(t *testing.T) {
		realm, _ := buildRealm(t)
		_, err := realm.SubRealm(schema.TableRef{Schema: "nope", Table: "orders"})
		require.ErrorIs(t, err, schema.ErrTableNotFound)
		assertResource(t, err, "nope.orders")
	})
	t.Run("root table missing", func(t *testing.T) {
		realm, _ := buildRealm(t)
		_, err := realm.SubRealm(schema.TableRef{Schema: "s1", Table: "nope"})
		require.ErrorIs(t, err, schema.ErrTableNotFound)
		assertResource(t, err, "s1.nope")
	})
	t.Run("duplicate root", func(t *testing.T) {
		realm, _ := buildRealm(t)
		_, err := realm.SubRealm(
			schema.TableRef{Schema: "s1", Table: "orders"},
			schema.TableRef{Schema: "s1", Table: "orders"},
		)
		require.ErrorIs(t, err, schema.ErrDuplicateRoot)
		assertResource(t, err, "s1.orders")
	})
	t.Run("fk target outside realm", func(t *testing.T) {
		s := schema.New("s1")
		orphan := schema.NewTable("orphan").AddColumns(schema.NewIntColumn("id", "int"))
		child := schema.NewTable("child").AddColumns(
			schema.NewIntColumn("id", "int"), schema.NewNullIntColumn("o", "int"))
		child.AddForeignKeys(schema.NewForeignKey("fk_o").
			SetRefTable(orphan).
			AddColumns(mustCol(child, "o")).
			AddRefColumns(mustCol(orphan, "id")))
		s.AddTables(child)
		realm := schema.NewRealm(s)
		_, err := realm.SubRealm(schema.TableRef{Schema: "s1", Table: "child"})
		require.ErrorIs(t, err, schema.ErrTableNotFound)
		assertResource(t, err, "s1.child.foreign_key[fk_o]")
	})
	t.Run("nil ref table", func(t *testing.T) {
		s := schema.New("s1")
		child := schema.NewTable("child").AddColumns(schema.NewIntColumn("id", "int"))
		child.AddForeignKeys(&schema.ForeignKey{Symbol: "fk_broken"})
		s.AddTables(child)
		realm := schema.NewRealm(s)
		_, err := realm.SubRealm(schema.TableRef{Schema: "s1", Table: "child"})
		require.ErrorIs(t, err, schema.ErrUnresolvedRef)
		assertResource(t, err, "s1.child.foreign_key[fk_broken]")
	})
	t.Run("ref column missing", func(t *testing.T) {
		s := schema.New("s1")
		parent := schema.NewTable("parent").AddColumns(schema.NewIntColumn("id", "int"))
		child := schema.NewTable("child").AddColumns(
			schema.NewIntColumn("id", "int"), schema.NewNullIntColumn("p", "int"))
		// Pair a child column with a referenced column that does not belong
		// to the parent table.
		ghost := schema.NewIntColumn("ghost", "int")
		child.AddForeignKeys(schema.NewForeignKey("fk_p").
			SetRefTable(parent).
			AddColumns(mustCol(child, "p")).
			AddRefColumns(ghost))
		s.AddTables(parent, child)
		realm := schema.NewRealm(s)
		_, err := realm.SubRealm(schema.TableRef{Schema: "s1", Table: "child"})
		require.ErrorIs(t, err, schema.ErrUnresolvedRef)
		assertResource(t, err, "s1.child.foreign_key[fk_p]")
	})
	t.Run("mismatched composite pairs", func(t *testing.T) {
		s := schema.New("s1")
		parent := schema.NewTable("parent").AddColumns(schema.NewIntColumn("a", "int"))
		parent.SetPrimaryKey(schema.NewPrimaryKey(mustCol(parent, "a")))
		child := schema.NewTable("child").AddColumns(
			schema.NewIntColumn("a", "int"), schema.NewNullIntColumn("b", "int"))
		child.AddForeignKeys(schema.NewForeignKey("fk_p").
			SetRefTable(parent).
			AddColumns(mustCol(child, "a"), mustCol(child, "b")).
			AddRefColumns(mustCol(parent, "a")))
		s.AddTables(parent, child)
		realm := schema.NewRealm(s)
		_, err := realm.SubRealm(schema.TableRef{Schema: "s1", Table: "child"})
		require.ErrorIs(t, err, schema.ErrUnresolvedRef)
	})
	t.Run("extension column type", func(t *testing.T) {
		s := schema.New("s1")
		tbl := schema.NewTable("t").AddColumns(schema.NewIntColumn("id", "int"))
		tbl.AddColumns(schema.NewColumn("d").SetType(&extType{Type: &schema.StringType{T: "text"}, T: "my_domain"}))
		s.AddTables(tbl)
		realm := schema.NewRealm(s)
		_, err := realm.SubRealm(schema.TableRef{Schema: "s1", Table: "t"})
		require.ErrorIs(t, err, schema.ErrUnsupportedType)
		assertResource(t, err, "s1.t.d.type")
	})
	t.Run("extension dependency object", func(t *testing.T) {
		s := schema.New("s1")
		tbl := schema.NewTable("t").AddColumns(schema.NewIntColumn("id", "int"))
		// A view dependency cannot be carried into an independent realm.
		tbl.AddDeps(schema.NewView("v", "SELECT 1"))
		s.AddTables(tbl)
		realm := schema.NewRealm(s)
		_, err := realm.SubRealm(schema.TableRef{Schema: "s1", Table: "t"})
		require.ErrorIs(t, err, schema.ErrUnsupportedDep)
		assertResource(t, err, "s1.t.depends_on[0]")
	})
	t.Run("table dependency is followed", func(t *testing.T) {
		s := schema.New("s1")
		parent := schema.NewTable("parent").AddColumns(schema.NewIntColumn("id", "int"))
		child := schema.NewTable("child").AddColumns(schema.NewIntColumn("id", "int"))
		child.AddDeps(parent)
		s.AddTables(parent, child)
		realm := schema.NewRealm(s)
		sub, err := realm.SubRealm(schema.TableRef{Schema: "s1", Table: "child"})
		require.NoError(t, err)
		require.Equal(t, []string{"parent", "child"}, tableNames(t, sub.Schemas[0]))
		// Reverse refs are rebuilt among output objects.
		require.Len(t, findTable(t, sub, "s1", "parent").Refs, 1)
	})
	t.Run("index referencing foreign column", func(t *testing.T) {
		s := schema.New("s1")
		t1 := schema.NewTable("t1").AddColumns(schema.NewIntColumn("a", "int"))
		t2 := schema.NewTable("t2").AddColumns(schema.NewIntColumn("b", "int"))
		t2.AddIndexes(schema.NewIndex("bad").AddColumns(mustCol(t1, "a")))
		s.AddTables(t1, t2)
		realm := schema.NewRealm(s)
		_, err := realm.SubRealm(schema.TableRef{Schema: "s1", Table: "t2"})
		require.ErrorIs(t, err, schema.ErrUnresolvedRef)
		assertResource(t, err, "s1.t2.index[bad].part[0]")
	})
	t.Run("nil realm", func(t *testing.T) {
		var r *schema.Realm
		_, err := r.SubRealm(schema.TableRef{Schema: "s1", Table: "t"})
		require.Error(t, err)
	})
}

// ---- helpers ----

func schemaNames(r *schema.Realm) []string {
	var out []string
	for _, s := range r.Schemas {
		out = append(out, s.Name)
	}
	return out
}

func tableNames(t *testing.T, s *schema.Schema) []string {
	t.Helper()
	var out []string
	for _, tb := range s.Tables {
		out = append(out, tb.Name)
	}
	return out
}

func columnNames(cs []*schema.Column) []string {
	var out []string
	for _, c := range cs {
		out = append(out, c.Name)
	}
	return out
}

func pkColumns(ix *schema.Index) []string {
	var out []string
	for _, p := range ix.Parts {
		if p.C != nil {
			out = append(out, p.C.Name)
		}
	}
	return out
}

func mustCol(t *schema.Table, name string) *schema.Column {
	c, ok := t.Column(name)
	if !ok {
		panic(fmt.Sprintf("column %q not found", name))
	}
	return c
}

func mustFK(t *schema.Table, symbol string) *schema.ForeignKey {
	fk, ok := t.ForeignKey(symbol)
	if !ok {
		panic(fmt.Sprintf("foreign key %q not found", symbol))
	}
	return fk
}

func tableComment(t *schema.Table) string {
	for _, a := range t.Attrs {
		if c, ok := a.(*schema.Comment); ok {
			return c.Text
		}
	}
	return ""
}

func findTable(t *testing.T, r *schema.Realm, s, name string) *schema.Table {
	t.Helper()
	sc, ok := r.Schema(s)
	require.True(t, ok)
	tb, ok := sc.Table(name)
	require.True(t, ok)
	return tb
}

func assertResource(t *testing.T, err error, path string) {
	t.Helper()
	var re *schema.ResourceError
	require.ErrorAs(t, err, &re)
	require.Equal(t, path, re.Path)
}

// assertGraphIsolation walks the entire output graph and verifies that every
// pointer reference resolves to an object inside it, and that no graph node is
// shared with the input realm.
func assertGraphIsolation(t *testing.T, in, out *schema.Realm) {
	t.Helper()
	var (
		schemas = map[*schema.Schema]bool{}
		tables  = map[*schema.Table]bool{}
		columns = map[*schema.Column]bool{}
		indexes = map[*schema.Index]bool{}
		fks     = map[*schema.ForeignKey]bool{}
		enums   = map[*schema.EnumType]bool{}
	)
	for _, s := range out.Schemas {
		schemas[s] = true
		for _, o := range s.Objects {
			if e, ok := o.(*schema.EnumType); ok {
				enums[e] = true
				require.Same(t, s, e.Schema)
			}
		}
		for _, tb := range s.Tables {
			tables[tb] = true
			require.Same(t, s, tb.Schema)
			require.Same(t, out, s.Realm)
			for _, c := range tb.Columns {
				columns[c] = true
			}
			if tb.PrimaryKey != nil {
				indexes[tb.PrimaryKey] = true
			}
			for _, ix := range tb.Indexes {
				indexes[ix] = true
			}
			for _, fk := range tb.ForeignKeys {
				fks[fk] = true
			}
		}
	}
	// No identity overlap with the input graph nodes.
	for _, s := range in.Schemas {
		require.False(t, schemas[s], "output shares schema pointer with input")
		for _, tb := range s.Tables {
			require.False(t, tables[tb], "output shares table pointer")
			for _, c := range tb.Columns {
				require.False(t, columns[c], "output shares column pointer")
			}
			if tb.PrimaryKey != nil {
				require.False(t, indexes[tb.PrimaryKey])
			}
			for _, ix := range tb.Indexes {
				require.False(t, indexes[ix])
			}
			for _, fk := range tb.ForeignKeys {
				require.False(t, fks[fk])
			}
		}
		for _, o := range s.Objects {
			if e, ok := o.(*schema.EnumType); ok {
				require.False(t, enums[e])
			}
		}
	}
	// All references resolve within the output.
	for _, s := range out.Schemas {
		for _, tb := range s.Tables {
			require.True(t, tables[tb])
			for _, c := range tb.Columns {
				for _, ix := range c.Indexes {
					require.True(t, indexes[ix], "column %s.%s index backref unresolved", tb.Name, c.Name)
					require.Same(t, tb, ix.Table)
				}
				for _, fk := range c.ForeignKeys {
					require.True(t, fks[fk])
					require.Same(t, tb, fk.Table)
				}
			}
			checkIndex := func(ix *schema.Index) {
				require.Same(t, tb, ix.Table)
				for _, p := range ix.Parts {
					if p.C != nil {
						require.True(t, columns[p.C], "index part column outside graph")
					}
				}
			}
			if tb.PrimaryKey != nil {
				checkIndex(tb.PrimaryKey)
			}
			for _, ix := range tb.Indexes {
				checkIndex(ix)
			}
			for _, fk := range tb.ForeignKeys {
				require.True(t, tables[fk.RefTable], "fk referenced table outside graph")
				require.Len(t, fk.RefColumns, len(fk.Columns))
				for _, c := range fk.Columns {
					require.True(t, columns[c])
				}
				for _, c := range fk.RefColumns {
					require.True(t, columns[c], "fk referenced column outside graph")
				}
			}
			for _, d := range tb.Deps {
				switch d := d.(type) {
				case *schema.Table:
					require.True(t, tables[d], "dep table outside graph")
				case *schema.EnumType:
					require.True(t, enums[d], "dep enum outside graph")
				default:
					t.Fatalf("unsupported dep type %T", d)
				}
			}
			for _, r := range tb.Refs {
				refT, ok := r.(*schema.Table)
				require.True(t, ok)
				require.True(t, tables[refT], "reverse ref points outside the output graph")
				require.NotSame(t, in, refT.Schema.Realm)
			}
		}
	}
	// Every enum used by a column resolves to the shared output object or an
	// inline copy; in both cases its schema (when set) is an output schema.
	for tb := range tables {
		for _, c := range tb.Columns {
			if e, ok := c.Type.Type.(*schema.EnumType); ok && e.Schema != nil {
				require.True(t, schemas[e.Schema])
			}
		}
	}
}

// dumpRealm serializes the identity/order shape of the input realm so tests can
// assert extraction does not mutate it.
func dumpRealm(r *schema.Realm) string {
	var b []byte
	for _, s := range r.Schemas {
		b = append(b, s.Name...)
		b = append(b, ':')
		for _, tb := range s.Tables {
			b = append(b, tb.Name...)
			b = append(b, '[')
			for _, c := range tb.Columns {
				b = append(b, c.Name...)
				b = append(b, ',')
			}
			b = append(b, ']')
			for _, fk := range tb.ForeignKeys {
				b = append(b, fk.Symbol...)
				b = append(b, '(')
				for _, c := range fk.Columns {
					b = append(b, c.Name...)
					b = append(b, ',')
				}
				b = append(b, ')')
			}
		}
		b = append(b, ';')
	}
	return string(b)
}

// Ensure sentinel errors are usable with errors.Is.
func TestSubRealm_Sentinels(t *testing.T) {
	_, err := schema.NewRealm(schema.New("s")).SubRealm(schema.TableRef{Schema: "s", Table: "t"})
	require.True(t, errors.Is(err, schema.ErrTableNotFound))
}

func TestSubRealm_EnumOnlySchemaAndSharing(t *testing.T) {
	// s4 owns a shared enum but no selected table: it must still be created
	// as a schema container holding only the enum object.
	s1 := schema.New("s1")
	s4 := schema.New("s4")
	shared := &schema.EnumType{T: "mood", Schema: s4, Values: []string{"ok", "bad"}}
	s4.AddObjects(shared)

	parent := schema.NewTable("parent").AddColumns(
		schema.NewIntColumn("id", "int"),
		schema.NewColumn("mood1").SetType(shared),
	)
	parent.SetPrimaryKey(schema.NewPrimaryKey(mustCol(parent, "id")))
	child := schema.NewTable("child").AddColumns(
		schema.NewIntColumn("id", "int"),
		schema.NewNullIntColumn("parent_id", "int"),
		schema.NewColumn("mood2").SetType(shared),
	)
	child.SetPrimaryKey(schema.NewPrimaryKey(mustCol(child, "id")))
	child.AddForeignKeys(schema.NewForeignKey("fk").
		SetRefTable(parent).
		AddColumns(mustCol(child, "parent_id")).
		AddRefColumns(mustCol(parent, "id")))
	s1.AddTables(parent, child)
	// A decoy enum that nobody references must not be pulled in.
	s4.AddObjects(&schema.EnumType{T: "unused", Schema: s4, Values: []string{"x"}})
	realm := schema.NewRealm(s1, s4)

	sub, err := realm.SubRealm(schema.TableRef{Schema: "s1", Table: "child"})
	require.NoError(t, err)
	require.Equal(t, []string{"s1", "s4"}, schemaNames(sub))
	require.Empty(t, sub.Schemas[1].Tables, "enum-only schema must not gain tables")

	var outEnum *schema.EnumType
	require.Len(t, sub.Schemas[1].Objects, 1, "only the referenced enum is included")
	outEnum = sub.Schemas[1].Objects[0].(*schema.EnumType)
	require.Equal(t, "mood", outEnum.T)
	require.Same(t, sub.Schemas[1], outEnum.Schema)

	// Both columns share the single output enum by object identity.
	p := findTable(t, sub, "s1", "parent")
	c := findTable(t, sub, "s1", "child")
	require.Same(t, outEnum, mustCol(p, "mood1").Type.Type)
	require.Same(t, outEnum, mustCol(c, "mood2").Type.Type)
	assertGraphIsolation(t, realm, sub)
}

func TestSubRealm_SchemaAndColumnAttrsPreserved(t *testing.T) {
	s := schema.New("s1").SetCharset("utf8mb4").SetCollation("utf8mb4_bin")
	t1 := schema.NewTable("t1").
		AddColumns(
			schema.NewIntColumn("id", "int").SetCharset("ascii"),
			schema.NewColumn("code").SetType(&schema.UnsupportedType{T: "custom"}).
				SetDefault(&schema.RawExpr{X: "nextval('seq')"}),
		)
	t1.SetPrimaryKey(schema.NewPrimaryKey(mustCol(t1, "id")))
	s.AddTables(t1)
	realm := schema.NewRealm(s)

	sub, err := realm.SubRealm(schema.TableRef{Schema: "s1", Table: "t1"})
	require.NoError(t, err)
	require.Len(t, sub.Schemas, 1)
	require.Len(t, sub.Schemas[0].Tables, 1)
	out := sub.Schemas[0].Tables[0]
	// UnsupportedType is part of the supported set and RawExpr strings are
	// carried verbatim (never scanned for dependencies).
	require.IsType(t, &schema.UnsupportedType{}, mustCol(out, "code").Type.Type)
	require.Equal(t, "nextval('seq')", mustCol(out, "code").Default.(*schema.RawExpr).X)
	// Charset/collation attributes survive.
	var charset, collation bool
	for _, a := range sub.Schemas[0].Attrs {
		switch a.(type) {
		case *schema.Charset:
			charset = true
		case *schema.Collation:
			collation = true
		}
	}
	require.True(t, charset && collation)
	require.NotNil(t, out.PrimaryKey)
}
