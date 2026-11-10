// Copyright 2021-present The Atlas Authors. All rights reserved.
// This source code is licensed under the Apache 2.0 license found
// in the LICENSE file in the root directory of this source tree.

package schema_test

import (
	"errors"
	"fmt"
	"math/rand"
	"testing"

	"ariga.io/atlas/sql/schema"

	"github.com/stretchr/testify/require"
)

// richRealm builds a realm exercising every supported node kind. It is called
// repeatedly to obtain structurally identical realms with fresh pointers.
func richRealm() *schema.Realm {
	id := schema.NewUintColumn("id", "bigint").SetComment("pk col")
	name := schema.NewNullStringColumn("na.me", "varchar", schema.StringSize(255)).
		SetDefault(&schema.Literal{V: "'a b'"})
	created := schema.NewTimeColumn("created", "timestamp", schema.TimePrecision(6)).
		SetDefault(&schema.RawExpr{X: "current_timestamp(3)"})
	active := schema.NewBoolColumn("active", "bool").SetDefault(&schema.Literal{V: "true"})
	status := schema.NewNullEnumColumn("status", schema.EnumName("status"), schema.EnumValues("a", "b")).
		SetCharset("ascii")
	data := schema.NewBinaryColumn("data", "varbinary", schema.BinarySize(1024))
	score := schema.NewDecimalColumn("score", "decimal", schema.DecimalPrecision(10), schema.DecimalScale(2))
	ratio := schema.NewFloatColumn("ratio", "float", schema.FloatUnsigned(true), schema.FloatPrecision(24))
	geo := schema.NewSpatialColumn("geo", "point")
	doc := schema.NewJSONColumn("doc", "json")
	uid := schema.NewColumn("uid").SetType(&schema.UUIDType{T: "uuid"})
	weird := schema.NewColumn("weird").SetType(&schema.UnsupportedType{T: "customtype"})
	gen := schema.NewIntColumn("gen", "int").
		SetGeneratedExpr(&schema.GeneratedExpr{Expr: "id * 2", Type: "STORED"})
	named := schema.NewColumn("named").SetType(&schema.IntegerType{T: "int"}).SetDefault(
		&schema.NamedDefault{
			Name:  "df",
			Expr:  &schema.RawExpr{X: "now()"},
			Attrs: []schema.Attr{&schema.Comment{Text: "nd"}},
		},
	)

	users := schema.NewTable("users").
		AddColumns(id, name, created, active, status, data, score, ratio, geo, doc, uid, weird, gen, named).
		SetPrimaryKey(schema.NewPrimaryKey(id)).
		AddIndexes(
			schema.NewIndex("idx.name").
				AddParts(
					schema.NewColumnPart(name).SetDesc(true),
					schema.NewExprPart(&schema.RawExpr{X: "lower(`na.me`)"}),
				).
				SetComment("name index"),
			schema.NewUniqueIndex("idx_status").AddColumns(status),
		).
		SetComment("users table").
		SetCharset("utf8mb4").
		SetCollation("utf8mb4_bin").
		AddChecks(
			schema.NewCheck().SetName("chk_active").SetExpr("active in (0,1)").
				AddAttrs(&schema.Comment{Text: "check comment"}),
			schema.NewCheck().SetExpr("score >= 0"),
		)
	// Self-referencing foreign key (same-table cycle).
	users.AddForeignKeys(
		schema.NewForeignKey("fk_self").
			AddColumns(id).
			SetRefTable(users).
			AddRefColumns(id),
	)
	// Position attributes must never enter the representation.
	users.SetPos(schema.NewFilePos("users.hcl").SetStart(struct{ Line, Column, Byte int }{Line: 1, Column: 2, Byte: 3}))
	id.SetPos(schema.NewFilePos("users.hcl"))

	postID := schema.NewIntColumn("id", "int")
	authorID := schema.NewNullIntColumn("author_id", "bigint")
	// An explicit NULL default is distinct from a missing default.
	title := schema.NewStringColumn("title", "varchar", schema.StringSize(100)).
		SetDefault(&schema.Literal{V: "NULL"})
	posts := schema.NewTable("posts").
		AddColumns(postID, authorID, title).
		SetPrimaryKey(schema.NewPrimaryKey(postID))

	nodeID := schema.NewIntColumn("id", "int")
	parentID := schema.NewNullIntColumn("parent_id", "int")
	nodes := schema.NewTable("nodes").
		AddColumns(nodeID, parentID).
		SetPrimaryKey(schema.NewPrimaryKey(nodeID))
	nodes.AddForeignKeys(
		schema.NewForeignKey("fk_parent").
			AddColumns(parentID).
			SetRefTable(nodes).
			AddRefColumns(nodeID),
	)

	accountID := schema.NewIntColumn("id", "int")
	accountSub := schema.NewIntColumn("sub", "int")
	owner := schema.NewIntColumn("owner", "bigint")
	accounts := schema.NewTable("accounts").
		AddColumns(accountID, accountSub, owner).
		SetPrimaryKey(schema.NewPrimaryKey(accountID, accountSub))
	accounts.AddForeignKeys(
		schema.NewForeignKey("fk_owner").
			AddColumns(owner).
			SetRefTable(users).
			AddRefColumns(id),
	)
	// Composite foreign key: child/ref column pairing order must be preserved.
	// Together with fk_owner this closes a cross-schema cycle.
	posts.AddForeignKeys(
		schema.NewForeignKey("fk_author").
			AddColumns(authorID, postID).
			SetRefTable(accounts).
			AddRefColumns(accountID, accountSub).
			SetOnDelete(schema.Cascade).
			SetOnUpdate(schema.SetNull),
	)

	s1 := schema.New("s.1").
		SetCharset("utf8mb4").
		SetCollation("utf8mb4_bin").
		SetComment("first schema").
		AddObjects(&schema.EnumType{
			T:      "color",
			Values: []string{"red", "green", "blue"},
			Attrs:  []schema.Attr{&schema.Comment{Text: "colors"}},
		}).
		AddTables(users, posts)
	s1.SetPos(schema.NewFilePos("s1.hcl"))

	s2 := schema.New("s2").AddTables(nodes, accounts)

	r := schema.NewRealm(s1, s2)
	r.Attrs = append(r.Attrs, &schema.Comment{Text: "realm comment"})
	r.AddObjects(&schema.EnumType{T: "status_enum", Values: []string{"on", "off", "unknown"}})
	return r
}

// shuffleRealm randomly reorders every unordered collection. Order-sensitive
// collections (columns, index parts, enum values, FK column pairing) are left
// untouched.
func shuffleRealm(r *schema.Realm, rng *rand.Rand) {
	shuffleAttrs := func(attrs []schema.Attr) {
		rng.Shuffle(len(attrs), func(i, j int) { attrs[i], attrs[j] = attrs[j], attrs[i] })
	}
	rng.Shuffle(len(r.Schemas), func(i, j int) { r.Schemas[i], r.Schemas[j] = r.Schemas[j], r.Schemas[i] })
	rng.Shuffle(len(r.Objects), func(i, j int) { r.Objects[i], r.Objects[j] = r.Objects[j], r.Objects[i] })
	shuffleAttrs(r.Attrs)
	for _, s := range r.Schemas {
		rng.Shuffle(len(s.Tables), func(i, j int) { s.Tables[i], s.Tables[j] = s.Tables[j], s.Tables[i] })
		rng.Shuffle(len(s.Objects), func(i, j int) { s.Objects[i], s.Objects[j] = s.Objects[j], s.Objects[i] })
		shuffleAttrs(s.Attrs)
		for _, o := range s.Objects {
			if e, ok := o.(*schema.EnumType); ok {
				shuffleAttrs(e.Attrs)
			}
		}
		for _, t := range s.Tables {
			rng.Shuffle(len(t.Indexes), func(i, j int) { t.Indexes[i], t.Indexes[j] = t.Indexes[j], t.Indexes[i] })
			rng.Shuffle(len(t.ForeignKeys), func(i, j int) {
				t.ForeignKeys[i], t.ForeignKeys[j] = t.ForeignKeys[j], t.ForeignKeys[i]
			})
			shuffleAttrs(t.Attrs)
			for _, c := range t.Columns {
				shuffleAttrs(c.Attrs)
			}
			for _, idx := range t.Indexes {
				shuffleAttrs(idx.Attrs)
			}
			if t.PrimaryKey != nil {
				shuffleAttrs(t.PrimaryKey.Attrs)
			}
			for _, fk := range t.ForeignKeys {
				shuffleAttrs(fk.Attrs)
			}
			for _, ck := range t.Checks() {
				shuffleAttrs(ck.Attrs)
			}
		}
	}
}

func mustMarshal(t *testing.T, r *schema.Realm, dialect string) []byte {
	t.Helper()
	b, err := schema.MarshalCanonical(r, dialect)
	require.NoError(t, err)
	require.NotEmpty(t, b)
	return b
}

func TestFingerprint_StableUnderShuffleAndNewPointers(t *testing.T) {
	base := mustMarshal(t, richRealm(), "mysql")
	// Re-encoding the same value must be byte-identical.
	require.Equal(t, base, mustMarshal(t, richRealm(), "mysql"))
	// Fresh pointers (a brand-new realm) must not affect the digest.
	fp1, err := schema.Fingerprint(richRealm(), "mysql")
	require.NoError(t, err)
	fp2, err := schema.Fingerprint(richRealm(), "mysql")
	require.NoError(t, err)
	require.Equal(t, fp1.Sum, fp2.Sum)
	require.Equal(t, fp1.Hex(), fp2.Hex())

	// Randomly reorder every unordered collection many times.
	rng := rand.New(rand.NewSource(42))
	for i := 0; i < 50; i++ {
		r := richRealm()
		shuffleRealm(r, rng)
		require.Equalf(t, base, mustMarshal(t, r, "mysql"), "iteration %d", i)
	}
}

func orderSnapshot(r *schema.Realm) [][]string {
	var out [][]string
	for _, s := range r.Schemas {
		row := []string{s.Name}
		for _, t := range s.Tables {
			row = append(row, t.Name)
		}
		out = append(out, row)
	}
	return out
}

func TestFingerprint_DoesNotModifyRealm(t *testing.T) {
	r := richRealm()
	before := orderSnapshot(r)
	_, err := schema.Fingerprint(r, "mysql")
	require.NoError(t, err)
	require.Equal(t, before, orderSnapshot(r))
}

func TestFingerprint_SensitiveToStructure(t *testing.T) {
	base := mustMarshal(t, richRealm(), "mysql")

	tests := []struct {
		name    string
		dialect string
		modify  func(r *schema.Realm)
	}{
		{
			name: "column order",
			modify: func(r *schema.Realm) {
				cols := r.Schemas[0].Tables[0].Columns
				cols[0], cols[1] = cols[1], cols[0]
			},
		},
		{
			name: "index part order",
			modify: func(r *schema.Realm) {
				idx := r.Schemas[0].Tables[0].Indexes[0]
				idx.Parts[0], idx.Parts[1] = idx.Parts[1], idx.Parts[0]
			},
		},
		{
			name: "foreign-key ref column pairing",
			modify: func(r *schema.Realm) {
				fk := r.Schemas[0].Tables[1].ForeignKeys[0]
				fk.RefColumns[0], fk.RefColumns[1] = fk.RefColumns[1], fk.RefColumns[0]
			},
		},
		{
			name: "foreign-key child column pairing",
			modify: func(r *schema.Realm) {
				fk := r.Schemas[0].Tables[1].ForeignKeys[0]
				fk.Columns[0], fk.Columns[1] = fk.Columns[1], fk.Columns[0]
			},
		},
		{
			name: "raw expression whitespace",
			modify: func(r *schema.Realm) {
				c := r.Schemas[0].Tables[0].Columns[2]
				c.Default = &schema.RawExpr{X: "current_timestamp (3)"}
			},
		},
		{
			name: "literal vs raw expression tag",
			modify: func(r *schema.Realm) {
				c := r.Schemas[0].Tables[0].Columns[3]
				// Same text, different expression kind.
				c.Default = &schema.RawExpr{X: "true"}
			},
		},
		{
			name: "identifier casing",
			modify: func(r *schema.Realm) {
				r.Schemas[0].Tables[0].Name = "Users"
			},
		},
		{
			name: "enum value order",
			modify: func(r *schema.Realm) {
				e := r.Schemas[0].Objects[0].(*schema.EnumType)
				e.Values[0], e.Values[1] = e.Values[1], e.Values[0]
			},
		},
		{
			name: "check expression text",
			modify: func(r *schema.Realm) {
				tbl := r.Schemas[0].Tables[0]
				tbl.Checks()[0].Expr = "active in ( 0 , 1 )"
			},
		},
		{
			name:    "dialect is part of the identity",
			dialect: "postgres",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r := richRealm()
			if tc.modify != nil {
				tc.modify(r)
			}
			dialect := tc.dialect
			if dialect == "" {
				dialect = "mysql"
			}
			require.NotEqual(t, base, mustMarshal(t, r, dialect))
		})
	}
}

func TestFingerprint_MissingDefaultVsExplicitNULL(t *testing.T) {
	build := func(def schema.Expr) *schema.Realm {
		c := schema.NewIntColumn("c", "int")
		c.Default = def
		tbl := schema.NewTable("t").AddColumns(c)
		return schema.NewRealm(schema.New("s").AddTables(tbl))
	}
	missing := mustMarshal(t, build(nil), "sqlite")
	explicitNull := mustMarshal(t, build(&schema.Literal{V: "NULL"}), "sqlite")
	rawNull := mustMarshal(t, build(&schema.RawExpr{X: "NULL"}), "sqlite")
	require.NotEqual(t, missing, explicitNull)
	require.NotEqual(t, missing, rawNull)
	require.NotEqual(t, explicitNull, rawNull)
}

func TestFingerprint_NilAndEmptyCollectionsAreSynonymous(t *testing.T) {
	build := func(attrs []schema.Attr) *schema.Realm {
		tbl := schema.NewTable("t").
			AddColumns(schema.NewIntColumn("c", "int"))
		tbl.Attrs = attrs
		return schema.NewRealm(schema.New("s").AddTables(tbl))
	}
	require.Equal(t,
		mustMarshal(t, build(nil), "sqlite"),
		mustMarshal(t, build([]schema.Attr{}), "sqlite"),
	)
}

func TestFingerprint_OptionalIntPointerPresence(t *testing.T) {
	build := func(size *int) *schema.Realm {
		tbl := schema.NewTable("t").AddColumns(
			schema.NewBinaryColumn("c", "blob", func(b *schema.BinaryType) { b.Size = size }),
		)
		return schema.NewRealm(schema.New("s").AddTables(tbl))
	}
	zero := 0
	require.NotEqual(t,
		mustMarshal(t, build(nil), "sqlite"),
		mustMarshal(t, build(&zero), "sqlite"),
	)
}

func TestFingerprint_AttributeOrderAndPosAreIrrelevant(t *testing.T) {
	base := mustMarshal(t, richRealm(), "mysql")
	r := richRealm()
	// Add positions everywhere and reorder existing attrs: representation
	// must not move.
	for _, s := range r.Schemas {
		s.Attrs = append([]schema.Attr{schema.NewFilePos("other.hcl")}, s.Attrs...)
		for _, t := range s.Tables {
			t.Attrs = append([]schema.Attr{schema.NewFilePos("other.hcl")}, t.Attrs...)
			if len(t.Attrs) >= 3 {
				t.Attrs[1], t.Attrs[2] = t.Attrs[2], t.Attrs[1]
			}
			for _, c := range t.Columns {
				c.Attrs = append(c.Attrs, schema.NewFilePos("x.hcl"))
			}
		}
	}
	require.Equal(t, base, mustMarshal(t, r, "mysql"))
}

func TestFingerprint_CyclesTerminate(t *testing.T) {
	// Cross-schema and same-table cycles exist in richRealm; encoding must
	// terminate and stay stable.
	require.Equal(t, mustMarshal(t, richRealm(), "mysql"), mustMarshal(t, richRealm(), "mysql"))
}

func TestFingerprint_VerifyAndHeader(t *testing.T) {
	r := richRealm()
	fp, err := schema.Fingerprint(r, "mysql")
	require.NoError(t, err)
	require.Equal(t, schema.CanonicalVersion, fp.Version)
	require.Equal(t, "mysql", fp.Dialect)
	require.Len(t, fp.Sum, 32)
	require.NoError(t, fp.Verify(richRealm()))

	// A changed realm fails verification.
	changed := richRealm()
	changed.Schemas[0].Tables[0].Columns[0].Name = "renamed"
	require.Error(t, fp.Verify(changed))

	// A fingerprint from another dialect must not match, because the dialect
	// is part of the hashed representation.
	fpOther, err := schema.Fingerprint(richRealm(), "postgres")
	require.NoError(t, err)
	require.NotEqual(t, fp.Sum, fpOther.Sum)
	require.Error(t, fpOther.Verify(changed))

	// Different representation versions must never be treated as synonymous.
	old := *fp
	old.Version = fp.Version - 1
	var fe *schema.FingerprintError
	require.ErrorAs(t, old.Verify(richRealm()), &fe)

	// Empty dialect is rejected.
	_, err = schema.Fingerprint(richRealm(), "")
	require.ErrorAs(t, err, &fe)
	require.Equal(t, "realm", fe.Path)
}

func TestFingerprint_TrickyNamesAndBoundarySafety(t *testing.T) {
	// Names that would collide under naive concatenation or whitespace/case
	// normalization.
	build := func(schemaName, tableName string, cols ...string) *schema.Realm {
		tbl := schema.NewTable(tableName)
		for _, n := range cols {
			tbl.AddColumns(schema.NewIntColumn(n, "int"))
		}
		return schema.NewRealm(schema.New(schemaName).AddTables(tbl))
	}
	tricky := []string{
		"a.b", "a;b", "a b", "a\tb", `a"b`, "a\x00b", "a,b", "a/b",
		"ab", "a", "bc", "c", "A", "a b ", " a b", "uni👋code",
	}
	r := build("s.1; s2", "t`x`.y", tricky...)
	b, err := schema.MarshalCanonical(r, "sqlite")
	require.NoError(t, err)

	// The independent decoder must recover every name exactly.
	dec, err := decodeCanonical(b)
	require.NoError(t, err)
	require.Equal(t, "atlas-canonical-realm", dec.magic)
	require.Equal(t, uint64(schema.CanonicalVersion), dec.version)
	require.Equal(t, "sqlite", dec.dialect)
	require.Equal(t, "s.1; s2", dec.realm.Schemas[0].Name)
	require.Equal(t, "t`x`.y", dec.realm.Schemas[0].Tables[0].Name)
	require.Equal(t, tricky, dec.realm.Schemas[0].Tables[0].columnNames())

	// ("a","bc") and ("ab","c") must not be conflated.
	b1 := mustMarshal(t, build("a", "bc", "x"), "sqlite")
	b2 := mustMarshal(t, build("ab", "c", "x"), "sqlite")
	require.NotEqual(t, b1, b2)
	d1, err := decodeCanonical(b1)
	require.NoError(t, err)
	d2, err := decodeCanonical(b2)
	require.NoError(t, err)
	require.Equal(t, "a", d1.realm.Schemas[0].Name)
	require.Equal(t, "bc", d1.realm.Schemas[0].Tables[0].Name)
	require.Equal(t, "ab", d2.realm.Schemas[0].Name)
	require.Equal(t, "c", d2.realm.Schemas[0].Tables[0].Name)

	// Column-level boundary ambiguity.
	c1 := mustMarshal(t, build("s", "t", "ab", "c"), "sqlite")
	c2 := mustMarshal(t, build("s", "t", "a", "bc"), "sqlite")
	require.NotEqual(t, c1, c2)
	dc1, err := decodeCanonical(c1)
	require.NoError(t, err)
	dc2, err := decodeCanonical(c2)
	require.NoError(t, err)
	require.Equal(t, []string{"ab", "c"}, dc1.realm.Schemas[0].Tables[0].columnNames())
	require.Equal(t, []string{"a", "bc"}, dc2.realm.Schemas[0].Tables[0].columnNames())
}

// goldenHex is the fixed canonical representation of goldenRealm. It is
// regenerated only intentionally: any change means a representation-version
// bump is required.
var goldenHex = "000000000000001561746c61732d63616e6f6e6963616c2d7265616c6d000000000000000100000000000000056d7973716c01000000000000000000000000000000000000000000000001000000000000017e0200000000000000027331000000000000000100000000000001530300000000000000027431000000000000000204000000000000000269640105170000000000000003696e74000000000000000000000000000000000000000000000000000001000000000000000f1a0000000000000006696420636f6c0400000000000000046e616d6501051300000000000000077661726368617200000000000000400000000000000000000000000000000001010b0000000000000003277827000000000000000000000000000000000106000000000000000001000000000000000107000000000000000000010000000000000002733100000000000000027431000000000000000269640000000000000000000000000000000000000000000000000000000000000000020000000000000021090000000000000002636b00000000000000066964203e2030000000000000000000000000000000121a00000000000000097461626c65206f6e6500000000000000000000000000000000"

func goldenRealm() *schema.Realm {
	id := schema.NewIntColumn("id", "int").SetComment("id col")
	name := schema.NewNullStringColumn("name", "varchar", schema.StringSize(64)).
		SetDefault(&schema.Literal{V: "'x'"})
	tbl := schema.NewTable("t1").
		AddColumns(id, name).
		AddChecks(schema.NewCheck().SetName("ck").SetExpr("id > 0")).
		SetComment("table one")
	tbl.SetPrimaryKey(schema.NewPrimaryKey(id))
	return schema.NewRealm(schema.New("s1").AddTables(tbl))
}

func TestFingerprint_GoldenBytes(t *testing.T) {
	got, err := schema.MarshalCanonical(goldenRealm(), "mysql")
	require.NoError(t, err)
	if goldenHex == "" {
		t.Fatalf("golden not pinned yet; candidate bytes:\n%x", got)
	}
	require.Equal(t, goldenHex, fmt.Sprintf("%x", got),
		"canonical representation changed; bump CanonicalVersion instead of silently updating the golden")

	// The independent decoder must consume every byte without ambiguity.
	dec, err := decodeCanonical(got)
	require.NoError(t, err)
	require.Equal(t, "mysql", dec.dialect)
	require.Equal(t, uint64(schema.CanonicalVersion), dec.version)
	tbl := dec.realm.Schemas[0].Tables[0]
	require.Equal(t, "t1", tbl.Name)
	require.Equal(t, []string{"id", "name"}, tbl.columnNames())
	require.NotNil(t, tbl.PrimaryKey)
	var checkName string
	for _, a := range tbl.Attrs {
		if a.Tag == dTagCheck {
			checkName = a.Name
		}
	}
	require.Equal(t, "ck", checkName)
}

func TestFingerprint_ErrorPaths(t *testing.T) {
	var fe *schema.FingerprintError
	_, err := schema.MarshalCanonical(nil, "mysql")
	require.ErrorAs(t, err, &fe)
	require.Equal(t, "realm", fe.Path)

	_, err = schema.MarshalCanonical(
		schema.NewRealm(schema.New("s").AddViews(schema.NewView("v", "SELECT 1"))),
		"mysql",
	)
	require.ErrorAs(t, err, &fe)
	require.Contains(t, fe.Path, ".views")
}

func TestFingerprint_ErrorWrapping(t *testing.T) {
	_, err := schema.Fingerprint(richRealm(), "")
	var fe *schema.FingerprintError
	require.True(t, errors.As(err, &fe))
	require.NotEmpty(t, fe.Error())
	require.NotNil(t, fe.Unwrap())
}
