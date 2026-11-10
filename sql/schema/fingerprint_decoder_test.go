// Copyright 2021-present The Atlas Authors. All rights reserved.
// This source code is licensed under the Apache 2.0 license found
// in the LICENSE file in the root directory of this source tree.

package schema_test

import (
	"encoding/binary"
	"fmt"
)

// This file contains an independent decoder for the canonical representation.
// It is written from the format specification rather than reusing encoder
// internals (the external test package cannot reach them anyway). Its job is
// to prove that every field boundary is self-describing: a strict reader must
// consume a valid representation exactly, recover every string verbatim and
// reject trailing bytes.

// Independent tag constants. The numeric values intentionally mirror the
// encoder tag order; a drift would make golden decoding fail loudly.
const (
	dTagRealm byte = iota + 1
	dTagSchema
	dTagTable
	dTagColumn
	dTagColumnType
	dTagIndex
	dTagIndexPart
	dTagForeignKey
	dTagCheck
	dTagEnumObject
	dTagLiteral
	dTagRawExpr
	dTagNamedDefault
	dTagBoolType
	dTagEnumType
	dTagTimeType
	dTagJSONType
	dTagFloatType
	dTagStringType
	dTagBinaryType
	dTagSpatialType
	dTagUUIDType
	dTagIntegerType
	dTagDecimalType
	dTagUnsupportedType
	dTagComment
	dTagCharset
	dTagCollation
	dTagGeneratedExpr
)

// canonReader is a strict big-endian reader with bounds checking.
type canonReader struct {
	b   []byte
	pos int
}

func (r *canonReader) u8() (byte, error) {
	if r.pos+1 > len(r.b) {
		return 0, fmt.Errorf("unexpected EOF reading byte at %d", r.pos)
	}
	v := r.b[r.pos]
	r.pos++
	return v, nil
}

func (r *canonReader) boolean() (bool, error) {
	v, err := r.u8()
	if err != nil {
		return false, err
	}
	switch v {
	case 0:
		return false, nil
	case 1:
		return true, nil
	default:
		return false, fmt.Errorf("invalid boolean marker %d at %d", v, r.pos-1)
	}
}

func (r *canonReader) u64() (uint64, error) {
	if r.pos+8 > len(r.b) {
		return 0, fmt.Errorf("unexpected EOF reading uint64 at %d", r.pos)
	}
	v := binary.BigEndian.Uint64(r.b[r.pos:])
	r.pos += 8
	return v, nil
}

func (r *canonReader) integer() (int64, error) {
	v, err := r.u64()
	return int64(v), err
}

func (r *canonReader) str() (string, error) {
	p, err := r.raw()
	return string(p), err
}

func (r *canonReader) raw() ([]byte, error) {
	n, err := r.u64()
	if err != nil {
		return nil, err
	}
	if n > uint64(len(r.b)-r.pos) {
		return nil, fmt.Errorf("length-prefixed field of %d bytes exceeds remaining input at %d", n, r.pos)
	}
	p := r.b[r.pos : r.pos+int(n)]
	r.pos += int(n)
	return p, nil
}

// count reads an inline list count.
func (r *canonReader) count() (int, error) {
	n, err := r.u64()
	return int(n), err
}

// chunk reads a length-prefixed record into a new sub-reader.
func (r *canonReader) chunk() (*canonReader, error) {
	p, err := r.raw()
	if err != nil {
		return nil, err
	}
	return &canonReader{b: p}, nil
}

// expectTag consumes a tag byte and verifies its value.
func (r *canonReader) expectTag(want byte) error {
	got, err := r.u8()
	if err != nil {
		return err
	}
	if got != want {
		return fmt.Errorf("expected tag %d, got %d at %d", want, got, r.pos-1)
	}
	return nil
}

// Decoded payloads.

type decodedCanon struct {
	magic   string
	version uint64
	dialect string
	realm   *decRealm
}

type decRealm struct {
	Attrs   []*decAttr
	Objects []*decEnum
	Schemas []*decSchema
}

type decSchema struct {
	Name    string
	Tables  []*decTable
	Objects []*decEnum
	Attrs   []*decAttr
}

type decTable struct {
	Name        string
	Columns     []*decColumn
	Indexes     []*decIndex
	PrimaryKey  *decIndex
	ForeignKeys []*decFK
	Attrs       []*decAttr
}

func (t *decTable) columnNames() []string {
	out := make([]string, len(t.Columns))
	for i, c := range t.Columns {
		out[i] = c.Name
	}
	return out
}

type decColumn struct {
	Name    string
	HasType bool
	Type    *decType
	Raw     string
	Null    bool
	Default *decExpr
	Attrs   []*decAttr
}

type decType struct {
	Tag                    byte
	T                      string
	Unsigned               bool
	Size                   int64
	SizeSet                bool
	Precision, Scale       int64
	PrecisionSet, ScaleSet bool
	Values                 []string
	Attrs                  []*decAttr
}

type decIndex struct {
	Name   string
	Unique bool
	Parts  []*decPart
	Attrs  []*decAttr
}

type decPart struct {
	SeqNo     int64
	Desc      bool
	HasColumn bool
	ColSchema string
	ColTable  string
	ColName   string
	Expr      *decExpr
	Attrs     []*decAttr
}

type decFK struct {
	Symbol     string
	Columns    [][3]string
	HasRef     bool
	RefSchema  string
	RefTable   string
	RefColumns [][3]string
	OnUpdate   string
	OnDelete   string
	Attrs      []*decAttr
}

type decEnum struct {
	T      string
	Values []string
	Attrs  []*decAttr
}

type decAttr struct {
	Tag byte
	// Comment uses Text; Charset/Collation use V; Check uses Name/Expr/Attrs;
	// GeneratedExpr uses Expr/Type.
	Text, V, Name, Expr, Type string
	Attrs                     []*decAttr
}

type decExpr struct {
	Present bool
	Tag     byte
	Text    string // Literal V, RawExpr X or NamedDefault name
	Attrs   []*decAttr
	Inner   *decExpr
}

func decodeCanonical(b []byte) (*decodedCanon, error) {
	r := &canonReader{b: b}
	magic, err := r.str()
	if err != nil {
		return nil, err
	}
	version, err := r.u64()
	if err != nil {
		return nil, err
	}
	dialect, err := r.str()
	if err != nil {
		return nil, err
	}
	out := &decodedCanon{magic: magic, version: version, dialect: dialect}
	if err := r.expectTag(dTagRealm); err != nil {
		return nil, err
	}
	realm, err := decodeRealm(r)
	if err != nil {
		return nil, err
	}
	out.realm = realm
	if r.pos != len(r.b) {
		return nil, fmt.Errorf("trailing %d bytes after full decode: field boundaries are ambiguous", len(r.b)-r.pos)
	}
	return out, nil
}

func decodeRealm(r *canonReader) (*decRealm, error) {
	out := &decRealm{}
	attrs, err := decodeAttrChunks(r)
	if err != nil {
		return nil, err
	}
	out.Attrs = attrs
	enums, err := decodeEnumChunks(r)
	if err != nil {
		return nil, err
	}
	out.Objects = enums
	n, err := r.count()
	if err != nil {
		return nil, err
	}
	for i := 0; i < n; i++ {
		cr, err := r.chunk()
		if err != nil {
			return nil, err
		}
		if err := cr.expectTag(dTagSchema); err != nil {
			return nil, err
		}
		s, err := decodeSchema(cr)
		if err != nil {
			return nil, err
		}
		if err := cr.assertEOF("schema"); err != nil {
			return nil, err
		}
		out.Schemas = append(out.Schemas, s)
	}
	return out, nil
}

func (r *canonReader) assertEOF(what string) error {
	if r.pos != len(r.b) {
		return fmt.Errorf("trailing %d bytes in %s record", len(r.b)-r.pos, what)
	}
	return nil
}

func decodeSchema(r *canonReader) (*decSchema, error) {
	out := &decSchema{}
	name, err := r.str()
	if err != nil {
		return nil, err
	}
	out.Name = name
	n, err := r.count()
	if err != nil {
		return nil, err
	}
	for i := 0; i < n; i++ {
		cr, err := r.chunk()
		if err != nil {
			return nil, err
		}
		if err := cr.expectTag(dTagTable); err != nil {
			return nil, err
		}
		t, err := decodeTable(cr)
		if err != nil {
			return nil, err
		}
		if err := cr.assertEOF("table"); err != nil {
			return nil, err
		}
		out.Tables = append(out.Tables, t)
	}
	enums, err := decodeEnumChunks(r)
	if err != nil {
		return nil, err
	}
	out.Objects = enums
	attrs, err := decodeAttrChunks(r)
	if err != nil {
		return nil, err
	}
	out.Attrs = attrs
	return out, nil
}

func decodeTable(r *canonReader) (*decTable, error) {
	out := &decTable{}
	name, err := r.str()
	if err != nil {
		return nil, err
	}
	out.Name = name
	n, err := r.count()
	if err != nil {
		return nil, err
	}
	for i := 0; i < n; i++ {
		if err := r.expectTag(dTagColumn); err != nil {
			return nil, err
		}
		c, err := decodeColumn(r)
		if err != nil {
			return nil, err
		}
		out.Columns = append(out.Columns, c)
	}
	// Indexes: chunk list.
	n, err = r.count()
	if err != nil {
		return nil, err
	}
	for i := 0; i < n; i++ {
		cr, err := r.chunk()
		if err != nil {
			return nil, err
		}
		if err := cr.expectTag(dTagIndex); err != nil {
			return nil, err
		}
		idx, err := decodeIndex(cr)
		if err != nil {
			return nil, err
		}
		if err := cr.assertEOF("index"); err != nil {
			return nil, err
		}
		out.Indexes = append(out.Indexes, idx)
	}
	hasPK, err := r.boolean()
	if err != nil {
		return nil, err
	}
	if hasPK {
		if err := r.expectTag(dTagIndex); err != nil {
			return nil, err
		}
		idx, err := decodeIndex(r)
		if err != nil {
			return nil, err
		}
		out.PrimaryKey = idx
	}
	// Foreign keys: chunk list.
	n, err = r.count()
	if err != nil {
		return nil, err
	}
	for i := 0; i < n; i++ {
		cr, err := r.chunk()
		if err != nil {
			return nil, err
		}
		if err := cr.expectTag(dTagForeignKey); err != nil {
			return nil, err
		}
		fk, err := decodeFK(cr)
		if err != nil {
			return nil, err
		}
		if err := cr.assertEOF("foreign-key"); err != nil {
			return nil, err
		}
		out.ForeignKeys = append(out.ForeignKeys, fk)
	}
	attrs, err := decodeAttrChunks(r)
	if err != nil {
		return nil, err
	}
	out.Attrs = attrs
	return out, nil
}

func decodeColumn(r *canonReader) (*decColumn, error) {
	out := &decColumn{}
	name, err := r.str()
	if err != nil {
		return nil, err
	}
	out.Name = name
	hasType, err := r.boolean()
	if err != nil {
		return nil, err
	}
	out.HasType = hasType
	if hasType {
		if err := r.expectTag(dTagColumnType); err != nil {
			return nil, err
		}
		t, err := decodeType(r)
		if err != nil {
			return nil, err
		}
		out.Type = t
		raw, err := r.str()
		if err != nil {
			return nil, err
		}
		out.Raw = raw
		null, err := r.boolean()
		if err != nil {
			return nil, err
		}
		out.Null = null
	}
	e, err := decodeExpr(r)
	if err != nil {
		return nil, err
	}
	out.Default = e
	attrs, err := decodeAttrChunks(r)
	if err != nil {
		return nil, err
	}
	out.Attrs = attrs
	return out, nil
}

func decodeIndex(r *canonReader) (*decIndex, error) {
	out := &decIndex{}
	name, err := r.str()
	if err != nil {
		return nil, err
	}
	out.Name = name
	unique, err := r.boolean()
	if err != nil {
		return nil, err
	}
	out.Unique = unique
	n, err := r.count()
	if err != nil {
		return nil, err
	}
	for i := 0; i < n; i++ {
		if err := r.expectTag(dTagIndexPart); err != nil {
			return nil, err
		}
		p, err := decodePart(r)
		if err != nil {
			return nil, err
		}
		out.Parts = append(out.Parts, p)
	}
	attrs, err := decodeAttrChunks(r)
	if err != nil {
		return nil, err
	}
	out.Attrs = attrs
	return out, nil
}

func decodePart(r *canonReader) (*decPart, error) {
	out := &decPart{}
	seq, err := r.integer()
	if err != nil {
		return nil, err
	}
	out.SeqNo = seq
	desc, err := r.boolean()
	if err != nil {
		return nil, err
	}
	out.Desc = desc
	hasCol, err := r.boolean()
	if err != nil {
		return nil, err
	}
	out.HasColumn = hasCol
	if hasCol {
		ref, err := decodeColRef(r)
		if err != nil {
			return nil, err
		}
		out.ColSchema, out.ColTable, out.ColName = ref[0], ref[1], ref[2]
	}
	e, err := decodeExpr(r)
	if err != nil {
		return nil, err
	}
	out.Expr = e
	attrs, err := decodeAttrChunks(r)
	if err != nil {
		return nil, err
	}
	out.Attrs = attrs
	return out, nil
}

func decodeColRef(r *canonReader) ([3]string, error) {
	var out [3]string
	for i := range out {
		s, err := r.str()
		if err != nil {
			return out, err
		}
		out[i] = s
	}
	return out, nil
}

func decodeFK(r *canonReader) (*decFK, error) {
	out := &decFK{}
	symbol, err := r.str()
	if err != nil {
		return nil, err
	}
	out.Symbol = symbol
	n, err := r.count()
	if err != nil {
		return nil, err
	}
	for i := 0; i < n; i++ {
		ref, err := decodeColRef(r)
		if err != nil {
			return nil, err
		}
		out.Columns = append(out.Columns, ref)
	}
	hasRef, err := r.boolean()
	if err != nil {
		return nil, err
	}
	out.HasRef = hasRef
	if hasRef {
		if out.RefSchema, err = r.str(); err != nil {
			return nil, err
		}
		if out.RefTable, err = r.str(); err != nil {
			return nil, err
		}
		n, err = r.count()
		if err != nil {
			return nil, err
		}
		for i := 0; i < n; i++ {
			ref, err := decodeColRef(r)
			if err != nil {
				return nil, err
			}
			out.RefColumns = append(out.RefColumns, ref)
		}
	}
	if out.OnUpdate, err = r.str(); err != nil {
		return nil, err
	}
	if out.OnDelete, err = r.str(); err != nil {
		return nil, err
	}
	attrs, err := decodeAttrChunks(r)
	if err != nil {
		return nil, err
	}
	out.Attrs = attrs
	return out, nil
}

func decodeExpr(r *canonReader) (*decExpr, error) {
	out := &decExpr{}
	present, err := r.boolean()
	if err != nil {
		return nil, err
	}
	if !present {
		return out, nil
	}
	out.Present = true
	tag, err := r.u8()
	if err != nil {
		return nil, err
	}
	out.Tag = tag
	switch tag {
	case dTagLiteral, dTagRawExpr:
		s, err := r.str()
		if err != nil {
			return nil, err
		}
		out.Text = s
	case dTagNamedDefault:
		name, err := r.str()
		if err != nil {
			return nil, err
		}
		out.Text = name
		attrs, err := decodeAttrChunks(r)
		if err != nil {
			return nil, err
		}
		out.Attrs = attrs
		inner, err := decodeExpr(r)
		if err != nil {
			return nil, err
		}
		out.Inner = inner
	default:
		return nil, fmt.Errorf("unknown expression tag %d", tag)
	}
	return out, nil
}

func decodeType(r *canonReader) (*decType, error) {
	tag, err := r.u8()
	if err != nil {
		return nil, err
	}
	out := &decType{Tag: tag}
	switch tag {
	case dTagBoolType, dTagJSONType, dTagSpatialType, dTagUUIDType, dTagUnsupportedType:
		if out.T, err = r.str(); err != nil {
			return nil, err
		}
	case dTagEnumType:
		if out.T, err = r.str(); err != nil {
			return nil, err
		}
		n, err := r.count()
		if err != nil {
			return nil, err
		}
		for i := 0; i < n; i++ {
			v, err := r.str()
			if err != nil {
				return nil, err
			}
			out.Values = append(out.Values, v)
		}
		if out.Attrs, err = decodeAttrChunks(r); err != nil {
			return nil, err
		}
	case dTagTimeType:
		if out.T, err = r.str(); err != nil {
			return nil, err
		}
		if out.PrecisionSet, err = r.boolean(); err != nil {
			return nil, err
		}
		if out.PrecisionSet {
			if out.Precision, err = r.integer(); err != nil {
				return nil, err
			}
		}
		if out.ScaleSet, err = r.boolean(); err != nil {
			return nil, err
		}
		if out.ScaleSet {
			if out.Scale, err = r.integer(); err != nil {
				return nil, err
			}
		}
		if out.Attrs, err = decodeAttrChunks(r); err != nil {
			return nil, err
		}
	case dTagFloatType:
		if out.T, err = r.str(); err != nil {
			return nil, err
		}
		if out.Unsigned, err = r.boolean(); err != nil {
			return nil, err
		}
		if out.Precision, err = r.integer(); err != nil {
			return nil, err
		}
	case dTagStringType:
		if out.T, err = r.str(); err != nil {
			return nil, err
		}
		if out.Size, err = r.integer(); err != nil {
			return nil, err
		}
		if out.Attrs, err = decodeAttrChunks(r); err != nil {
			return nil, err
		}
	case dTagBinaryType:
		if out.T, err = r.str(); err != nil {
			return nil, err
		}
		if out.SizeSet, err = r.boolean(); err != nil {
			return nil, err
		}
		if out.SizeSet {
			if out.Size, err = r.integer(); err != nil {
				return nil, err
			}
		}
	case dTagIntegerType:
		if out.T, err = r.str(); err != nil {
			return nil, err
		}
		if out.Unsigned, err = r.boolean(); err != nil {
			return nil, err
		}
		if out.Attrs, err = decodeAttrChunks(r); err != nil {
			return nil, err
		}
	case dTagDecimalType:
		if out.T, err = r.str(); err != nil {
			return nil, err
		}
		if out.Precision, err = r.integer(); err != nil {
			return nil, err
		}
		if out.Scale, err = r.integer(); err != nil {
			return nil, err
		}
		if out.Unsigned, err = r.boolean(); err != nil {
			return nil, err
		}
	default:
		return nil, fmt.Errorf("unknown type tag %d", tag)
	}
	return out, nil
}

func decodeEnumChunks(r *canonReader) ([]*decEnum, error) {
	n, err := r.count()
	if err != nil {
		return nil, err
	}
	out := make([]*decEnum, 0, n)
	for i := 0; i < n; i++ {
		cr, err := r.chunk()
		if err != nil {
			return nil, err
		}
		if err := cr.expectTag(dTagEnumObject); err != nil {
			return nil, err
		}
		e := &decEnum{}
		if e.T, err = cr.str(); err != nil {
			return nil, err
		}
		vn, err := cr.count()
		if err != nil {
			return nil, err
		}
		for j := 0; j < vn; j++ {
			v, err := cr.str()
			if err != nil {
				return nil, err
			}
			e.Values = append(e.Values, v)
		}
		if e.Attrs, err = decodeAttrChunks(cr); err != nil {
			return nil, err
		}
		if err := cr.assertEOF("enum object"); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, nil
}

func decodeAttrChunks(r *canonReader) ([]*decAttr, error) {
	n, err := r.count()
	if err != nil {
		return nil, err
	}
	out := make([]*decAttr, 0, n)
	for i := 0; i < n; i++ {
		cr, err := r.chunk()
		if err != nil {
			return nil, err
		}
		tag, err := cr.u8()
		if err != nil {
			return nil, err
		}
		a := &decAttr{Tag: tag}
		switch tag {
		case dTagComment:
			if a.Text, err = cr.str(); err != nil {
				return nil, err
			}
		case dTagCharset, dTagCollation:
			if a.V, err = cr.str(); err != nil {
				return nil, err
			}
		case dTagGeneratedExpr:
			if a.Expr, err = cr.str(); err != nil {
				return nil, err
			}
			if a.Type, err = cr.str(); err != nil {
				return nil, err
			}
		case dTagCheck:
			if a.Name, err = cr.str(); err != nil {
				return nil, err
			}
			if a.Expr, err = cr.str(); err != nil {
				return nil, err
			}
			if a.Attrs, err = decodeAttrChunks(cr); err != nil {
				return nil, err
			}
		default:
			return nil, fmt.Errorf("unknown attribute tag %d", tag)
		}
		if err := cr.assertEOF("attribute"); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, nil
}
