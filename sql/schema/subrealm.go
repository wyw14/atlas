// Copyright 2025-present The Atlas Authors. All rights reserved.
// This source code is licensed under the Apache 2.0 license found
// in the LICENSE file in the root directory of this source tree.

package schema

import (
	"errors"
	"fmt"
	"slices"
)

// Sentinel errors returned by Realm.SubRealm. Callers can use errors.Is to
// inspect the failure kind, and errors.As to extract the *ResourceError.
var (
	// ErrTableNotFound is returned when a root table, or a table referenced by
	// a foreign-key or a dependency, cannot be found in the input realm.
	ErrTableNotFound = errors.New("schema: table not found in realm")

	// ErrDuplicateRoot is returned when the same fully-qualified table is
	// listed more than once as a root.
	ErrDuplicateRoot = errors.New("schema: duplicate root table")

	// ErrUnresolvedRef is returned when a selected table holds a reference
	// (foreign-key, index part or dependency) to an object that is missing
	// from the input realm.
	ErrUnresolvedRef = errors.New("schema: unresolved reference to an object missing from the input realm")

	// ErrUnsupportedType is returned when a selected column uses a type that
	// is not part of the supported type set of SubRealm.
	ErrUnsupportedType = errors.New("schema: column type is not in the supported set")

	// ErrUnsupportedDep is returned when a selected table depends on an
	// extension object whose dependencies cannot be determined by SubRealm.
	ErrUnsupportedDep = errors.New("schema: dependency is not in the supported set")
)

// TableRef is a fully-qualified reference to a table within a Realm.
type TableRef struct {
	// Schema is the name of the schema (named database) holding the table.
	Schema string
	// Table is the name of the table.
	Table string
}

// String returns the fully-qualified form "schema.table".
func (r TableRef) String() string {
	return r.Schema + "." + r.Table
}

// ResourceError wraps an error with the path of the resource that caused it.
// Paths are dot-separated and rooted at the schema, for example
// "shop.orders.foreign_key[fk_orders_customers]" or
// "shop.orders.customer_id.type".
type ResourceError struct {
	// Path is the path of the resource that caused the error.
	Path string
	// Err is the underlying error.
	Err error
}

// Error implements the error interface.
func (e *ResourceError) Error() string {
	switch {
	case e == nil:
		return ""
	case e.Path == "":
		return e.Err.Error()
	default:
		return fmt.Sprintf("%s: %v", e.Path, e.Err)
	}
}

// Unwrap returns the wrapped error.
func (e *ResourceError) Unwrap() error { return e.Err }

func resourceErr(path string, err error, detail string) error {
	if detail != "" {
		err = fmt.Errorf("%w: %s", err, detail)
	}
	return &ResourceError{Path: path, Err: err}
}

// SubRealm creates a new, standalone Realm that contains the given root tables
// and every table they transitively reference through their outgoing
// foreign-keys, along with the EnumType objects the selected tables require.
//
// Closure rules:
//
//   - Only outgoing references are followed: foreign-keys (ForeignKey.RefTable),
//     explicit table dependencies (Table.Deps of kind *Table), and enum
//     dependencies (column types or Table.Deps that reference an EnumType
//     declared in Schema.Objects). Tables that reference a root table are not
//     pulled in, and no full-realm diff is performed.
//   - Self-references, cross-schema references and foreign-key cycles are
//     supported. Cross-schema same-name tables and same-name enums residing in
//     different schemas are never merged: identity is based on object identity,
//     not on names.
//   - Only schemas that hold at least one selected table, or that own an enum
//     object required by the closure, are created.
//
// The result is an independent object graph: schemas, tables, columns, indexes,
// index-parts, foreign-keys and enum types are freshly allocated and all their
// references (including reverse Refs) point to objects within the returned
// realm. No pointer to the input realm or to an unselected object is retained.
// Slice ordering of the selected elements (columns, index parts, composite
// foreign-key pairs, attributes) is preserved. The order of the root list does
// not affect the resulting object set or its ordering, which follows the
// ordering of the input realm, schemas and tables.
//
// The input realm is never modified.
//
// Support set:
//
//   - Resources: schemas, tables, columns, primary-keys, indexes and their
//     parts, foreign-keys, checks, comments and the built-in attributes.
//   - Column types: EnumType and the built-in scalar types (BoolType,
//     BinaryType, StringType, IntegerType, DecimalType, FloatType, TimeType,
//     JSONType, SpatialType, UUIDType and UnsupportedType). Types that wrap an
//     underlying type (e.g. driver-specific domains, composites or arrays) are
//     extension objects that may carry additional dependencies; SubRealm does
//     not guess them and returns a *ResourceError instead.
//   - Attributes and expressions (including RawExpr, check and generated
//     column expressions) are carried over verbatim and their strings are
//     never scanned to infer dependencies.
//   - Table.Deps entries must be *Table or *EnumType objects declared in the
//     input realm. Views, triggers and arbitrary user objects are out of scope
//     and reported as errors when they are explicit dependencies of a selected
//     table.
//
// An empty root list produces an empty, but valid, realm.
func (r *Realm) SubRealm(roots ...TableRef) (*Realm, error) {
	if r == nil {
		return nil, errors.New("schema: cannot extract a sub-realm from a nil realm")
	}
	idx := indexRealm(r)

	// Phase 1: resolve the roots and collect the table closure by following
	// outgoing foreign-key and *Table dependency edges.
	var (
		selected = make(map[*Table]struct{})
		queue    []*Table
	)
	enqueue := func(t *Table) {
		if _, ok := selected[t]; !ok {
			selected[t] = struct{}{}
			queue = append(queue, t)
		}
	}
	seenRoots := make(map[TableRef]struct{}, len(roots))
	for _, ref := range roots {
		if _, dup := seenRoots[ref]; dup {
			return nil, resourceErr(ref.String(), ErrDuplicateRoot, "")
		}
		seenRoots[ref] = struct{}{}
		t, ok := idx.tables[ref]
		if !ok {
			return nil, resourceErr(ref.String(), ErrTableNotFound, "")
		}
		enqueue(t)
	}
	// Enum objects explicitly required through Table.Deps or column types,
	// and schemas that must be created because they hold selected tables or
	// required enum objects.
	var (
		needEnums   = make(map[*EnumType]struct{})
		needSchemas = make(map[*Schema]struct{})
	)
	for len(queue) > 0 {
		t := queue[0]
		queue = queue[1:]
		for i, d := range t.Deps {
			switch d := d.(type) {
			case nil:
				return nil, resourceErr(depPath(t, i), ErrUnresolvedRef, "nil dependency")
			case *Table:
				if _, ok := idx.tableIn[d]; !ok {
					return nil, resourceErr(depPath(t, i), ErrTableNotFound, tableName(d))
				}
				enqueue(d)
			case *EnumType:
				owner, ok := idx.enumOwner[d]
				if !ok {
					return nil, resourceErr(depPath(t, i), ErrUnresolvedRef, fmt.Sprintf("enum %q is not declared as a schema object", enumName(d)))
				}
				needEnums[d] = struct{}{}
				needSchemas[owner] = struct{}{}
			default:
				return nil, resourceErr(depPath(t, i), ErrUnsupportedDep, fmt.Sprintf("%T carries dependencies that cannot be determined", d))
			}
		}
		for i, fk := range t.ForeignKeys {
			if err := validateFK(idx, t, fk, i); err != nil {
				return nil, err
			}
			enqueue(fk.RefTable)
		}
	}

	// Phase 2: iterate the closure deterministically (in input realm/schema/
	// table order) to validate the remaining references and collect enums.
	var ordered []*Table
	for _, s := range r.Schemas {
		if s == nil {
			continue
		}
		for _, t := range s.Tables {
			if _, ok := selected[t]; !ok {
				continue
			}
			needSchemas[s] = struct{}{}
			ordered = append(ordered, t)
			if err := validateIndexParts(idx, t, t.PrimaryKey, true); err != nil {
				return nil, err
			}
			for _, ix := range t.Indexes {
				if err := validateIndexParts(idx, t, ix, false); err != nil {
					return nil, err
				}
			}
			for _, c := range t.Columns {
				if c == nil || c.Type == nil || c.Type.Type == nil {
					return nil, resourceErr(columnPath(t, c)+".type", ErrUnsupportedType, "column is missing its type")
				}
				if err := checkColumnType(t, c); err != nil {
					return nil, err
				}
				if e, ok := c.Type.Type.(*EnumType); ok {
					if owner, ok := idx.enumOwner[e]; ok {
						// A declared enum object: include it once and ensure
						// its owning schema container is created.
						needEnums[e] = struct{}{}
						needSchemas[owner] = struct{}{}
					}
					// Otherwise it is an inline (column-local) enum, cloned
					// per occurrence, that requires no schema container.
				}
			}
		}
	}

	// Phase 3: construct the independent object graph.
	out := &Realm{}
	var (
		outS = make(map[*Schema]*Schema)
		outT = make(map[*Table]*Table)
		outC = make(map[*Column]*Column)
		outI = make(map[*Index]*Index)
		outF = make(map[*ForeignKey]*ForeignKey)
		outE = make(map[*EnumType]*EnumType)
	)
	// Schema containers, created in input realm order and only when needed.
	for _, s := range r.Schemas {
		if _, ok := needSchemas[s]; !ok {
			continue
		}
		ns := &Schema{
			Name:  s.Name,
			Realm: out,
			Attrs: slices.Clone(s.Attrs),
		}
		outS[s] = ns
		out.Schemas = append(out.Schemas, ns)
	}
	// Declared enum objects, in their original schema-object order. They are
	// created before columns so column types can link to them.
	for _, s := range r.Schemas {
		ns, ok := outS[s]
		if !ok {
			continue
		}
		for _, o := range s.Objects {
			e, ok := o.(*EnumType)
			if !ok {
				continue
			}
			if _, need := needEnums[e]; !need {
				continue
			}
			ne := cloneEnum(e, ns)
			outE[e] = ne
			ns.Objects = append(ns.Objects, ne)
		}
	}
	// Tables and their columns, in input schema/table order.
	for _, t := range ordered {
		nt := &Table{
			Name:   t.Name,
			Schema: outS[t.Schema],
			Attrs:  slices.Clone(t.Attrs),
		}
		outT[t] = nt
		outS[t.Schema].Tables = append(outS[t.Schema].Tables, nt)
		for _, c := range t.Columns {
			nc := &Column{
				Name:    c.Name,
				Type:    &ColumnType{Type: mapColumnType(c.Type.Type, outE, outS), Raw: c.Type.Raw, Null: c.Type.Null},
				Default: c.Default,
				Attrs:   slices.Clone(c.Attrs),
			}
			outC[c] = nc
			nt.Columns = append(nt.Columns, nc)
		}
	}
	// Primary-keys, indexes and foreign-keys.
	for _, t := range ordered {
		nt := outT[t]
		if t.PrimaryKey != nil {
			nt.PrimaryKey = cloneIndex(t.PrimaryKey, nt, outC, outI)
		}
		for _, ix := range t.Indexes {
			nt.Indexes = append(nt.Indexes, cloneIndex(ix, nt, outC, outI))
		}
		for _, fk := range t.ForeignKeys {
			nf := &ForeignKey{
				Symbol:     fk.Symbol,
				Table:      nt,
				RefTable:   outT[fk.RefTable],
				OnUpdate:   fk.OnUpdate,
				OnDelete:   fk.OnDelete,
				Attrs:      slices.Clone(fk.Attrs),
				Columns:    make([]*Column, 0, len(fk.Columns)),
				RefColumns: make([]*Column, 0, len(fk.RefColumns)),
			}
			// Composite foreign-key pairing order is preserved.
			for _, c := range fk.Columns {
				nf.Columns = append(nf.Columns, outC[c])
			}
			for _, c := range fk.RefColumns {
				nf.RefColumns = append(nf.RefColumns, outC[c])
			}
			outF[fk] = nf
			nt.ForeignKeys = append(nt.ForeignKeys, nf)
		}
		// Explicit dependencies are limited to tables and declared enums.
		for _, d := range t.Deps {
			switch d := d.(type) {
			case *Table:
				nt.Deps = append(nt.Deps, outT[d])
			case *EnumType:
				nt.Deps = append(nt.Deps, outE[d])
			}
		}
	}
	// Column back-references to indexes (including the primary-key) and
	// foreign-keys, preserving their original order.
	for _, t := range ordered {
		nt := outT[t]
		for i, c := range t.Columns {
			nc := nt.Columns[i]
			for _, ix := range c.Indexes {
				if nix, ok := outI[ix]; ok {
					nc.Indexes = append(nc.Indexes, nix)
				}
			}
			for _, fk := range c.ForeignKeys {
				if nfk, ok := outF[fk]; ok {
					nc.ForeignKeys = append(nc.ForeignKeys, nfk)
				}
			}
		}
	}
	// Rebuild reverse Refs from the output dependency edges. Iterating the
	// output tables in their deterministic order keeps the result stable and
	// ensures refs never point to unselected or input-realm objects.
	for _, t := range ordered {
		child := outT[t]
		for _, d := range child.Deps {
			if parent, ok := d.(*Table); ok {
				addRef(parent, child)
			}
		}
	}
	return out, nil
}

// realmIndex is a single-pass, read-only index over the realm. It allows the
// closure traversal to resolve tables and enum objects in constant time.
type realmIndex struct {
	tables    map[TableRef]*Table
	tableIn   map[*Table]struct{}
	colOwner  map[*Column]*Table
	enumOwner map[*EnumType]*Schema
}

// indexRealm builds the realm index in a single linear pass over the realm.
func indexRealm(r *Realm) *realmIndex {
	idx := &realmIndex{
		tables:    make(map[TableRef]*Table),
		tableIn:   make(map[*Table]struct{}),
		colOwner:  make(map[*Column]*Table),
		enumOwner: make(map[*EnumType]*Schema),
	}
	for _, s := range r.Schemas {
		if s == nil {
			continue
		}
		for _, t := range s.Tables {
			if t == nil {
				continue
			}
			idx.tables[TableRef{Schema: s.Name, Table: t.Name}] = t
			idx.tableIn[t] = struct{}{}
			for _, c := range t.Columns {
				if c != nil {
					idx.colOwner[c] = t
				}
			}
		}
		for _, o := range s.Objects {
			if e, ok := o.(*EnumType); ok && e != nil {
				idx.enumOwner[e] = s
			}
		}
	}
	return idx
}

// validateFK ensures the foreign-key and every paired column resolves to
// objects that exist in the input realm.
func validateFK(idx *realmIndex, t *Table, fk *ForeignKey, i int) error {
	path := fkPath(t, fk, i)
	switch {
	case fk == nil:
		return resourceErr(path, ErrUnresolvedRef, "nil foreign-key")
	case fk.RefTable == nil:
		return resourceErr(path, ErrUnresolvedRef, "referenced table is missing")
	}
	if _, ok := idx.tableIn[fk.RefTable]; !ok {
		return resourceErr(path, ErrTableNotFound, tableName(fk.RefTable))
	}
	if len(fk.Columns) != len(fk.RefColumns) {
		return resourceErr(path, ErrUnresolvedRef, fmt.Sprintf(
			"foreign-key pairs %d child columns with %d referenced columns", len(fk.Columns), len(fk.RefColumns)))
	}
	for j, c := range fk.Columns {
		if c == nil || idx.colOwner[c] != t {
			return resourceErr(path, ErrUnresolvedRef, fmt.Sprintf("child column at position %d is missing from table %q", j, t.Name))
		}
		rc := fk.RefColumns[j]
		if rc == nil || idx.colOwner[rc] != fk.RefTable {
			return resourceErr(path, ErrUnresolvedRef, fmt.Sprintf("referenced column at position %d is missing from table %q", j, tableName(fk.RefTable)))
		}
	}
	return nil
}

// validateIndexParts ensures every column referenced by an index (or
// primary-key) part belongs to the indexed table.
func validateIndexParts(idx *realmIndex, t *Table, ix *Index, primary bool) error {
	if ix == nil {
		return nil
	}
	for j, p := range ix.Parts {
		if p == nil || p.C == nil {
			continue
		}
		if owner, ok := idx.colOwner[p.C]; !ok || owner != t {
			return resourceErr(indexPartPath(t, ix, j, primary), ErrUnresolvedRef,
				fmt.Sprintf("index column %q is missing from table %q", p.C.Name, t.Name))
		}
	}
	return nil
}

// checkColumnType reports an error if the column type is outside the supported
// type set. Wrapper types (implementing Underlying), such as driver-specific
// domains, composites or arrays, are extension objects that may carry
// dependencies and are rejected even if their underlying type is supported.
func checkColumnType(t *Table, c *Column) error {
	typ := c.Type.Type
	path := columnPath(t, c) + ".type"
	if UnderlyingType(typ) != typ {
		return resourceErr(path, ErrUnsupportedType, fmt.Sprintf("%T wraps an extension object", typ))
	}
	switch typ.(type) {
	case *EnumType, *BoolType, *BinaryType, *StringType, *IntegerType, *DecimalType,
		*FloatType, *TimeType, *JSONType, *SpatialType, *UUIDType, *UnsupportedType:
		return nil
	default:
		return resourceErr(path, ErrUnsupportedType, fmt.Sprintf("%T dependencies cannot be determined", typ))
	}
}

// mapColumnType resolves a (validated) input column type to its output
// representation. Declared enum objects are shared by identity, inline enums
// are cloned per column.
func mapColumnType(t Type, outE map[*EnumType]*EnumType, outS map[*Schema]*Schema) Type {
	e, ok := t.(*EnumType)
	if !ok {
		return t
	}
	if ne, ok := outE[e]; ok {
		return ne
	}
	ne := &EnumType{
		T:      e.T,
		Values: slices.Clone(e.Values),
		Attrs:  slices.Clone(e.Attrs),
	}
	if e.Schema != nil {
		// Link to the output schema when the inline enum carries a schema
		// that is part of the result; otherwise it remains schema-local.
		ne.Schema = outS[e.Schema]
	}
	return ne
}

// cloneEnum creates an independent copy of a declared enum object.
func cloneEnum(e *EnumType, owner *Schema) *EnumType {
	return &EnumType{
		T:      e.T,
		Values: slices.Clone(e.Values),
		Schema: owner,
		Attrs:  slices.Clone(e.Attrs),
	}
}

// cloneIndex creates an independent copy of an index (or primary-key) and its
// parts, reconnecting it to the output table and output columns.
func cloneIndex(ix *Index, nt *Table, outC map[*Column]*Column, outI map[*Index]*Index) *Index {
	nix := &Index{
		Name:   ix.Name,
		Unique: ix.Unique,
		Table:  nt,
		Attrs:  slices.Clone(ix.Attrs),
		Parts:  make([]*IndexPart, 0, len(ix.Parts)),
	}
	for _, p := range ix.Parts {
		np := &IndexPart{
			SeqNo: p.SeqNo,
			Desc:  p.Desc,
			X:     p.X,
			Attrs: slices.Clone(p.Attrs),
		}
		if p.C != nil {
			np.C = outC[p.C]
		}
		nix.Parts = append(nix.Parts, np)
	}
	outI[ix] = nix
	return nix
}

// addRef appends obj to the table Refs if it is not already referenced.
func addRef(t *Table, obj Object) {
	if !slices.Contains(t.Refs, obj) {
		t.Refs = append(t.Refs, obj)
	}
}

// Resource path helpers.

func schemaName(s *Schema) string {
	if s == nil {
		return ""
	}
	return s.Name
}

func tableName(t *Table) string {
	if t == nil {
		return "<nil>"
	}
	return schemaName(t.Schema) + "." + t.Name
}

func enumName(e *EnumType) string {
	if e == nil {
		return "<nil>"
	}
	return schemaName(e.Schema) + "." + e.T
}

func columnPath(t *Table, c *Column) string {
	name := "<nil>"
	if c != nil {
		name = c.Name
	}
	if t == nil {
		return name
	}
	return tableName(t) + "." + name
}

func depPath(t *Table, i int) string {
	return fmt.Sprintf("%s.depends_on[%d]", tableName(t), i)
}

func fkPath(t *Table, fk *ForeignKey, i int) string {
	if fk != nil && fk.Symbol != "" {
		return fmt.Sprintf("%s.foreign_key[%s]", tableName(t), fk.Symbol)
	}
	return fmt.Sprintf("%s.foreign_key[%d]", tableName(t), i)
}

func indexPartPath(t *Table, ix *Index, part int, primary bool) string {
	switch {
	case primary:
		return fmt.Sprintf("%s.primary_key.part[%d]", tableName(t), part)
	case ix.Name != "":
		return fmt.Sprintf("%s.index[%s].part[%d]", tableName(t), ix.Name, part)
	default:
		return fmt.Sprintf("%s.index[%d].part[%d]", tableName(t), part, part)
	}
}
