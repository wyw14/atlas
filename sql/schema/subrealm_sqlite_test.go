// Copyright 2025-present The Atlas Authors. All rights reserved.
// This source code is licensed under the Apache 2.0 license found
// in the LICENSE file in the root directory of this source tree.

package schema_test

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"

	"ariga.io/atlas/sql/migrate"
	"ariga.io/atlas/sql/schema"
	"ariga.io/atlas/sql/sqlite"

	_ "github.com/mattn/go-sqlite3"
	"github.com/stretchr/testify/require"
)

// TestSubRealm_SQLitePlanner verifies that an extracted sub-realm is fully
// independent: its CREATE SQL, produced by the existing SQLite planner and
// executed against a temporary in-memory database, does not reference any
// unselected object.
func TestSubRealm_SQLitePlanner(t *testing.T) {
	ctx := context.Background()
	main := schema.New("main")

	// customers <- orders <- order_items (composite FK).
	customers := schema.NewTable("customers").AddColumns(
		schema.NewIntColumn("id", "integer"),
		schema.NewStringColumn("name", "text"),
	)
	customers.SetPrimaryKey(schema.NewPrimaryKey(mustCol(customers, "id")))
	customers.AddChecks(schema.NewCheck().SetName("name_not_empty").SetExpr("(`name` <> '')"))

	orders := schema.NewTable("orders").AddColumns(
		schema.NewIntColumn("id", "integer"),
		schema.NewIntColumn("customer_id", "integer"),
	)
	orders.SetPrimaryKey(schema.NewPrimaryKey(mustCol(orders, "id")))
	orders.AddIndexes(schema.NewUniqueIndex("uniq_orders_customer_id").
		AddColumns(mustCol(orders, "customer_id"), mustCol(orders, "id")))
	orders.AddForeignKeys(schema.NewForeignKey("fk_orders_customers").
		SetRefTable(customers).
		AddColumns(mustCol(orders, "customer_id")).
		AddRefColumns(mustCol(customers, "id")).
		SetOnDelete(schema.Restrict))
	orders.AddChecks(schema.NewCheck().SetExpr("(`id` > 0)"))

	orderItems := schema.NewTable("order_items").AddColumns(
		schema.NewIntColumn("id", "integer"),
		schema.NewIntColumn("customer_id", "integer"),
		schema.NewIntColumn("order_id", "integer"),
	)
	orderItems.SetPrimaryKey(schema.NewPrimaryKey(mustCol(orderItems, "id")))
	orderItems.AddForeignKeys(schema.NewForeignKey("fk_items_orders").
		SetRefTable(orders).
		AddColumns(mustCol(orderItems, "customer_id"), mustCol(orderItems, "order_id")).
		AddRefColumns(mustCol(orders, "customer_id"), mustCol(orders, "id")))

	// Self reference.
	nodes := schema.NewTable("nodes").AddColumns(
		schema.NewIntColumn("id", "integer"),
		schema.NewNullIntColumn("parent_id", "integer"),
	)
	nodes.SetPrimaryKey(schema.NewPrimaryKey(mustCol(nodes, "id")))
	nodes.AddForeignKeys(schema.NewForeignKey("fk_nodes_parent").
		SetRefTable(nodes).
		AddColumns(mustCol(nodes, "parent_id")).
		AddRefColumns(mustCol(nodes, "id")))

	// Cycle: employees <-> depts.
	employees := schema.NewTable("employees").AddColumns(
		schema.NewIntColumn("id", "integer"),
		schema.NewNullIntColumn("dept_id", "integer"),
	)
	employees.SetPrimaryKey(schema.NewPrimaryKey(mustCol(employees, "id")))
	depts := schema.NewTable("depts").AddColumns(
		schema.NewIntColumn("id", "integer"),
		schema.NewNullIntColumn("manager_id", "integer"),
	)
	depts.SetPrimaryKey(schema.NewPrimaryKey(mustCol(depts, "id")))
	employees.AddForeignKeys(schema.NewForeignKey("fk_emp_dept").
		SetRefTable(depts).
		AddColumns(mustCol(employees, "dept_id")).
		AddRefColumns(mustCol(depts, "id")))
	depts.AddForeignKeys(schema.NewForeignKey("fk_dept_manager").
		SetRefTable(employees).
		AddColumns(mustCol(depts, "manager_id")).
		AddRefColumns(mustCol(employees, "id")))

	// Decoy: an incoming reference that must not be pulled into the sub-realm.
	audits := schema.NewTable("audits").AddColumns(
		schema.NewIntColumn("id", "integer"),
		schema.NewNullIntColumn("order_id", "integer"),
	)
	audits.SetPrimaryKey(schema.NewPrimaryKey(mustCol(audits, "id")))
	audits.AddForeignKeys(schema.NewForeignKey("fk_audit_orders").
		SetRefTable(orders).
		AddColumns(mustCol(audits, "order_id")).
		AddRefColumns(mustCol(orders, "id")))

	main.AddTables(customers, orders, orderItems, nodes, employees, depts, audits)
	realm := schema.NewRealm(main)

	sub, err := realm.SubRealm(
		schema.TableRef{Schema: "main", Table: "order_items"},
		schema.TableRef{Schema: "main", Table: "nodes"},
		schema.TableRef{Schema: "main", Table: "depts"},
	)
	require.NoError(t, err)
	require.Equal(t, []string{
		"customers", "orders", "order_items", "nodes", "employees", "depts",
	}, tableNames(t, sub.Schemas[0]))

	// Generate the CREATE SQL through the existing SQLite planner.
	db := openSQLite(t)
	drv, err := sqlite.Open(db)
	require.NoError(t, err)
	empty := &schema.Realm{Schemas: []*schema.Schema{{Name: "main"}}}
	changes, err := drv.(schema.Differ).RealmDiff(empty, sub)
	require.NoError(t, err)
	plan, err := drv.(migrate.PlanApplier).PlanChanges(ctx, "init", changes)
	require.NoError(t, err)
	require.NotEmpty(t, plan.Changes)

	// No generated statement may depend on an unselected object.
	var cmds []string
	for _, c := range plan.Changes {
		require.NotContains(t, c.Cmd, "audits", "plan references unselected table: %s", c.Cmd)
		cmds = append(cmds, c.Cmd)
	}
	for _, c := range plan.Changes {
		_, err := db.ExecContext(ctx, c.Cmd, c.Args...)
		require.NoError(t, err, "executing: %s", c.Cmd)
	}

	// Inspect the temporary database and verify the created object set.
	got, err := drv.(schema.Inspector).InspectRealm(ctx, nil)
	require.NoError(t, err)
	gotTables := tableNames(t, got.Schemas[0])
	require.ElementsMatch(t,
		[]string{"customers", "orders", "order_items", "nodes", "employees", "depts"},
		gotTables)
	require.NotContains(t, gotTables, "audits")

	// All expected foreign keys were materialized, including self and cyclic.
	type fkRef struct{ table, refTable string }
	gotFKs := map[fkRef]bool{}
	gotCols := map[string][]string{}
	for _, tb := range got.Schemas[0].Tables {
		for _, fk := range tb.ForeignKeys {
			gotFKs[fkRef{tb.Name, fk.RefTable.Name}] = true
			var cols []string
			for _, c := range fk.RefColumns {
				cols = append(cols, c.Name)
			}
			gotCols[tb.Name+"->"+fk.RefTable.Name] = cols
		}
	}
	require.True(t, gotFKs[fkRef{"orders", "customers"}])
	require.True(t, gotFKs[fkRef{"order_items", "orders"}])
	require.True(t, gotFKs[fkRef{"nodes", "nodes"}])
	require.True(t, gotFKs[fkRef{"employees", "depts"}])
	require.True(t, gotFKs[fkRef{"depts", "employees"}])
	// Composite FK pairing survives the round trip.
	require.Equal(t, []string{"customer_id", "id"}, gotCols["order_items->orders"])

	// The checks and the secondary index are present in the generated DDL.
	joined := strings.Join(cmds, "\n")
	require.Contains(t, joined, "name_not_empty")
	require.Contains(t, joined, "uniq_orders_customer_id")
	require.Contains(t, joined, "fk_nodes_parent")
}

var sqliteSeq atomic.Int64

func openSQLite(t *testing.T) *sql.DB {
	t.Helper()
	dsn := fmt.Sprintf("file:subrealm_%d?mode=memory&cache=shared", sqliteSeq.Add(1))
	db, err := sql.Open("sqlite3", dsn)
	require.NoError(t, err)
	t.Cleanup(func() { db.Close() })
	// Verify the connection is usable before handing it to the driver.
	require.NoError(t, db.Ping())
	return db
}
