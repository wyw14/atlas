// Copyright 2021-present The Atlas Authors. All rights reserved.
// This source code is licensed under the Apache 2.0 license found
// in the LICENSE file in the root directory of this source tree.

package schema

import (
	"errors"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// Unknown element implementations. They can only be defined inside the
// package because the marker methods are unexported.

type unknownAttr struct{}

func (*unknownAttr) attr() {}

type unknownType struct{}

func (*unknownType) typ() {}

type unknownExpr struct{}

func (*unknownExpr) expr() {}

type unknownObject struct{}

func (*unknownObject) obj() {}

func TestFingerprint_UnknownAttributeReturnsResourcePath(t *testing.T) {
	tbl := NewTable("t").
		AddColumns(NewIntColumn("c", "int")).
		AddAttrs(&unknownAttr{})
	r := NewRealm(New("s").AddTables(tbl))
	_, err := MarshalCanonical(r, "mysql")
	var fe *FingerprintError
	require.True(t, errors.As(err, &fe))
	require.Contains(t, fe.Path, `schema["s"]`)
	require.Contains(t, fe.Path, `table["t"]`)
	require.Contains(t, fe.Path, ".attrs[")
	require.Contains(t, err.Error(), "unknownAttr")
}

func TestFingerprint_UnknownNestedAttributeReturnsResourcePath(t *testing.T) {
	tbl := NewTable("t").
		AddColumns(NewIntColumn("c", "int")).
		AddChecks(NewCheck().SetName("ck").SetExpr("x > 0").AddAttrs(&unknownAttr{}))
	r := NewRealm(New("s").AddTables(tbl))
	_, err := MarshalCanonical(r, "mysql")
	var fe *FingerprintError
	require.True(t, errors.As(err, &fe))
	require.Contains(t, fe.Path, `table["t"]`)
	require.Contains(t, fe.Path, ".attrs[")
	require.Contains(t, fe.Path, ".attrs[")
}

func TestFingerprint_UnknownColumnTypeReturnsResourcePath(t *testing.T) {
	tbl := NewTable("t").AddColumns(NewColumn("c").SetType(&unknownType{}))
	r := NewRealm(New("s").AddTables(tbl))
	_, err := MarshalCanonical(r, "mysql")
	var fe *FingerprintError
	require.True(t, errors.As(err, &fe))
	require.True(t, strings.HasSuffix(fe.Path, `.column["c"].type`))
	require.Contains(t, err.Error(), "unknownType")
}

func TestFingerprint_UnknownExpressionReturnsResourcePath(t *testing.T) {
	tbl := NewTable("t").AddColumns(NewIntColumn("c", "int").SetDefault(&unknownExpr{}))
	r := NewRealm(New("s").AddTables(tbl))
	_, err := MarshalCanonical(r, "mysql")
	var fe *FingerprintError
	require.True(t, errors.As(err, &fe))
	require.True(t, strings.HasSuffix(fe.Path, `.column["c"].default`))

	// Unknown underlying expression of a named default.
	tbl2 := NewTable("t").AddColumns(NewIntColumn("c2", "int").SetDefault(
		&NamedDefault{Name: "df", Expr: &unknownExpr{}},
	))
	r2 := NewRealm(New("s").AddTables(tbl2))
	_, err = MarshalCanonical(r2, "mysql")
	require.True(t, errors.As(err, &fe))
	require.Contains(t, fe.Path, `.column["c2"].default.expr`)
}

func TestFingerprint_UnknownObjectReturnsResourcePath(t *testing.T) {
	r1 := NewRealm(New("s").AddObjects(&unknownObject{}))
	_, err := MarshalCanonical(r1, "mysql")
	var fe *FingerprintError
	require.True(t, errors.As(err, &fe))
	require.Contains(t, fe.Path, `schema["s"].objects[0]`)
	require.Contains(t, err.Error(), "unknownObject")

	r2 := NewRealm().AddObjects(&unknownObject{})
	_, err = MarshalCanonical(r2, "sqlite")
	require.True(t, errors.As(err, &fe))
	require.Equal(t, "realm.objects[0]", fe.Path)
}

func TestFingerprint_UnsupportedStructuralFieldsReturnPath(t *testing.T) {
	// Non-empty views are refused instead of being silently dropped.
	r1 := NewRealm(New("s").AddViews(NewView("v", "SELECT 1")))
	_, err := MarshalCanonical(r1, "mysql")
	var fe *FingerprintError
	require.True(t, errors.As(err, &fe))
	require.Contains(t, fe.Path, `schema["s"].views`)

	// Non-empty table dependencies are refused as well.
	tbl := NewTable("t").
		AddColumns(NewIntColumn("c", "int")).
		AddDeps(NewTable("dep"))
	r2 := NewRealm(New("s").AddTables(tbl))
	_, err = MarshalCanonical(r2, "mysql")
	require.True(t, errors.As(err, &fe))
	require.Contains(t, fe.Path, `table["t"].deps`)
}

func TestFingerprint_NilAndValidationErrors(t *testing.T) {
	var fe *FingerprintError

	_, err := MarshalCanonical(nil, "mysql")
	require.True(t, errors.As(err, &fe))
	require.Equal(t, "realm", fe.Path)

	_, err = MarshalCanonical(NewRealm(), "")
	require.True(t, errors.As(err, &fe))
	require.Equal(t, "realm", fe.Path)
	require.Contains(t, err.Error(), "dialect identifier is required")

	// Nil type inside a column type.
	tbl := NewTable("t").AddColumns(&Column{Name: "c", Type: &ColumnType{}})
	_, err = MarshalCanonical(NewRealm(New("s").AddTables(tbl)), "mysql")
	require.True(t, errors.As(err, &fe))
	require.Contains(t, fe.Path, `.column["c"].type`)
}

func TestFingerprint_DerivedReverseRefsAreIgnored(t *testing.T) {
	// Populating the derived Refs field must not change the representation.
	base := NewTable("t").AddColumns(NewIntColumn("c", "int"))
	r1 := NewRealm(New("s").AddTables(base))
	withRefs := NewTable("t").AddColumns(NewIntColumn("c", "int"))
	withRefs.Refs = []Object{NewTable("other")}
	r2 := NewRealm(New("s").AddTables(withRefs))
	b1, err := MarshalCanonical(r1, "mysql")
	require.NoError(t, err)
	b2, err := MarshalCanonical(r2, "mysql")
	require.NoError(t, err)
	require.Equal(t, b1, b2)
}

func TestFingerprint_UnknownEnumAttributeReturnsResourcePath(t *testing.T) {
	tbl := NewTable("t").AddColumns(
		NewEnumColumn("c", EnumValues("a", "b")).AddAttrs(&Comment{Text: "ok"}),
	)
	r := NewRealm(New("s").AddTables(tbl))
	b, err := MarshalCanonical(r, "mysql")
	require.NoError(t, err)
	require.NotEmpty(t, b)

	tbl2 := NewTable("t").AddColumns(NewColumn("c").SetType(
		&EnumType{T: "e", Values: []string{"a"}, Attrs: []Attr{&unknownAttr{}}},
	))
	_, err = MarshalCanonical(NewRealm(New("s").AddTables(tbl2)), "mysql")
	var fe *FingerprintError
	require.True(t, errors.As(err, &fe))
	require.Contains(t, fe.Path, ".type.attrs[")
}
