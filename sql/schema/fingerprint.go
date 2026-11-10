// Copyright 2021-present The Atlas Authors. All rights reserved.
// This source code is licensed under the Apache 2.0 license found
// in the LICENSE file in the root directory of this source tree.

package schema

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"sort"
)

// CanonicalVersion is the version of the canonical representation and digest
// algorithm implemented by this package. The version is part of the canonical
// representation and of the [RealmFingerprint] output. Digests produced by
// different representation versions are never synonymous: callers must not
// compare them directly.
const CanonicalVersion = 1

// canonicalMagic prefixes every canonical representation so that a byte slice
// can be recognized independently from its digest.
const canonicalMagic = "atlas-canonical-realm"

// RealmFingerprint is a deterministic, versioned identity of a normalized Realm.
//
// A fingerprint is derived from a self-describing, unambiguous binary
// representation of the Realm structure (see MarshalCanonical). It does not
// depend on HCL/DDL text formatting, Go pointer addresses, element positions
// (Pos) or derived reverse references. The fingerprint does not query a
// database, does not modify the fingerprinted Realm, and does not claim that
// two arbitrary SQL expressions or strings are semantically equivalent.
type RealmFingerprint struct {
	// Version of the canonical representation that produced the digest.
	Version int

	// Dialect identifies the dialect of the fingerprinted Realm (e.g. "mysql",
	// "postgres" or "sqlite"). It is part of the hashed representation.
	Dialect string

	// Sum is the SHA-256 digest of the canonical representation.
	Sum [sha256.Size]byte
}

// Hex returns the lowercase hexadecimal form of the digest.
func (f *RealmFingerprint) Hex() string {
	return fmt.Sprintf("%x", f.Sum)
}

// Verify reports whether r has the same canonical identity as the fingerprint.
// It recomputes the fingerprint without querying a database and without
// modifying r. A fingerprint produced by another representation version
// cannot be verified and returns an error.
func (f *RealmFingerprint) Verify(r *Realm) error {
	if f.Version != CanonicalVersion {
		return &FingerprintError{
			Path: "realm",
			Err: fmt.Errorf(
				"cannot verify fingerprint of representation version %d with version %d: digests of different versions are not synonymous",
				f.Version, CanonicalVersion,
			),
		}
	}
	got, err := Fingerprint(r, f.Dialect)
	if err != nil {
		return err
	}
	if got.Sum != f.Sum {
		return &FingerprintError{
			Path: "realm",
			Err:  fmt.Errorf("realm digest %s does not match stored fingerprint %s", got.Hex(), f.Hex()),
		}
	}
	return nil
}

// FingerprintError describes a failure to produce a canonical representation.
// The Path is a resource path (e.g. realm.schema["s"].table["t"].column["c"].type)
// that pinpoints the unsupported or invalid element.
type FingerprintError struct {
	// Path of the resource that caused the error.
	Path string
	// Err is the underlying error.
	Err error
}

// Error implements the error interface.
func (e *FingerprintError) Error() string {
	if e.Path == "" {
		return "sql/schema: fingerprint: " + e.Err.Error()
	}
	return fmt.Sprintf("sql/schema: fingerprint: %s (at %s)", e.Err.Error(), e.Path)
}

// Unwrap returns the underlying error.
func (e *FingerprintError) Unwrap() error { return e.Err }

func fpError(path string, format string, args ...any) error {
	return &FingerprintError{Path: path, Err: fmt.Errorf(format, args...)}
}

// Fingerprint computes a versioned [RealmFingerprint] for the given normalized
// Realm and dialect. It is a pure operation: it does not query a database and
// does not modify r or any of its elements.
func Fingerprint(r *Realm, dialect string) (*RealmFingerprint, error) {
	b, err := MarshalCanonical(r, dialect)
	if err != nil {
		return nil, err
	}
	return &RealmFingerprint{
		Version: CanonicalVersion,
		Dialect: dialect,
		Sum:     sha256.Sum256(b),
	}, nil
}

// MarshalCanonical returns the deterministic canonical byte representation of a
// normalized Realm of the given dialect.
//
// Determinism rules:
//
//   - Schemas and other named object collections are sorted by their structured
//     identity (kind plus fully-qualified name). Column order, index-part order,
//     composite foreign-key column pairing and enum value order are preserved.
//   - Strings are length-prefixed, so names containing dots, separators or any
//     byte value cannot collide with field boundaries. SQL text, expressions
//     and identifiers are taken verbatim: whitespace, casing and quoting are
//     never normalized.
//   - Literal and RawExpr values carry distinct type tags even when their text
//     is identical. A missing default and an explicit NULL default are distinct.
//   - Pos attributes, pointer back-links and derived reverse Refs are excluded.
//     References are encoded as fully-qualified identities, which also makes
//     cyclic foreign-key graphs terminate.
//   - nil and empty collections are normalized only where they are synonymous
//     (collection fields). Optional scalar/interface fields keep an explicit
//     presence marker, so nil is never confused with a zero or explicit value.
//   - Types, attributes, expressions and objects outside the explicitly
//     supported set produce a [FingerprintError] with a resource path instead
//     of being silently ignored.
func MarshalCanonical(r *Realm, dialect string) ([]byte, error) {
	if dialect == "" {
		return nil, fpError("realm", "dialect identifier is required")
	}
	if r == nil {
		return nil, fpError("realm", "nil realm")
	}
	w := &canonWriter{}
	w.writeString(canonicalMagic)
	w.writeUint64(uint64(CanonicalVersion))
	w.writeString(dialect)
	if err := encodeRealm(w, r, "realm"); err != nil {
		return nil, err
	}
	return w.b, nil
}

// Type tags of the canonical records. Every structured record starts with one
// of these tags, which lets an independent decoder dispatch on the record kind
// without looking ahead or relying on separators.
const (
	tagRealm byte = iota + 1
	tagSchema
	tagTable
	tagColumn
	tagColumnType
	tagIndex
	tagIndexPart
	tagForeignKey
	tagCheck
	tagEnumObject
	tagLiteral
	tagRawExpr
	tagNamedDefault
	tagBoolType
	tagEnumType
	tagTimeType
	tagJSONType
	tagFloatType
	tagStringType
	tagBinaryType
	tagSpatialType
	tagUUIDType
	tagIntegerType
	tagDecimalType
	tagUnsupportedType
	tagComment
	tagCharset
	tagCollation
	tagGeneratedExpr
)

// canonWriter is a minimal length-prefixed binary writer. All multi-byte
// integers use big-endian order and all strings/byte slices are prefixed with
// their length, making every field boundary self-describing.
type canonWriter struct {
	b []byte
}

func (w *canonWriter) writeByte(c byte) { w.b = append(w.b, c) }

func (w *canonWriter) writeBool(v bool) {
	if v {
		w.writeByte(1)
	} else {
		w.writeByte(0)
	}
}

func (w *canonWriter) writeUint64(v uint64) {
	var buf [8]byte
	binary.BigEndian.PutUint64(buf[:], v)
	w.b = append(w.b, buf[:]...)
}

// writeInt encodes a non-negative integer. Every integer field of the
// canonical representation (counts, sizes, precision, scale, SeqNo) is
// non-negative by definition; the guard documents and enforces that invariant.
func (w *canonWriter) writeInt(v int) {
	if v < 0 {
		panic("schema: canonical representation cannot encode a negative integer")
	}
	w.writeUint64(uint64(v))
}

func (w *canonWriter) writeCount(n int) {
	if n < 0 {
		panic("schema: canonical representation cannot encode a negative collection length")
	}
	w.writeUint64(uint64(n))
}

func (w *canonWriter) writeString(s string) {
	w.writeUint64(uint64(len(s)))
	w.b = append(w.b, s...)
}

func (w *canonWriter) writeBytes(p []byte) {
	w.writeUint64(uint64(len(p)))
	w.b = append(w.b, p...)
}

// namedChunk is an independently encoded element of a named collection.
type namedChunk struct {
	key []byte // structured identity used for deterministic ordering
	b   []byte // canonical record of the element
}

// writeNamedChunks sorts the chunks by structured identity (with the encoded
// record as a deterministic tie-breaker) and writes them as a count-prefixed
// list. Input slices are never reordered in place, so the fingerprinted Realm
// is not modified.
func writeNamedChunks(w *canonWriter, chunks []namedChunk) {
	sort.SliceStable(chunks, func(i, j int) bool {
		if c := bytes.Compare(chunks[i].key, chunks[j].key); c != 0 {
			return c < 0
		}
		return bytes.Compare(chunks[i].b, chunks[j].b) < 0
	})
	w.writeCount(len(chunks))
	for _, c := range chunks {
		w.writeBytes(c.b)
	}
}

// identityKey builds an unambiguous structured identity key from a kind tag
// and a name. The name is length-prefixed, therefore names containing dots or
// separator characters cannot make two identities compare ambiguously.
func identityKey(tag byte, name string) []byte {
	w := &canonWriter{}
	w.writeByte(tag)
	w.writeString(name)
	return w.b
}

func encodeRealm(w *canonWriter, r *Realm, path string) error {
	w.writeByte(tagRealm)
	if err := encodeAttrs(w, r.Attrs, path+".attrs"); err != nil {
		return err
	}
	if err := encodeObjects(w, r.Objects, path+".objects"); err != nil {
		return err
	}
	// Schemas are a named collection sorted by name.
	chunks := make([]namedChunk, 0, len(r.Schemas))
	for i, s := range r.Schemas {
		if s == nil {
			return fpError(fmt.Sprintf("%s.schemas[%d]", path, i), "nil schema")
		}
		cw := &canonWriter{}
		p := fmt.Sprintf("%s.schema[%q]", path, s.Name)
		if err := encodeSchema(cw, s, p); err != nil {
			return err
		}
		chunks = append(chunks, namedChunk{key: identityKey(tagSchema, s.Name), b: cw.b})
	}
	writeNamedChunks(w, chunks)
	return nil
}

func encodeSchema(w *canonWriter, s *Schema, path string) error {
	w.writeByte(tagSchema)
	w.writeString(s.Name)
	// Views and forward dependencies are structural fields that version 1 does
	// not represent. Refuse instead of silently dropping potentially relevant
	// structure.
	if len(s.Views) > 0 {
		return fpError(path+".views", "views are not supported by canonical representation version %d", CanonicalVersion)
	}
	if len(s.Tables) > 0 {
		chunks := make([]namedChunk, 0, len(s.Tables))
		for i, t := range s.Tables {
			if t == nil {
				return fpError(fmt.Sprintf("%s.tables[%d]", path, i), "nil table")
			}
			cw := &canonWriter{}
			p := fmt.Sprintf("%s.table[%q]", path, t.Name)
			if err := encodeTable(cw, t, s.Name, p); err != nil {
				return err
			}
			chunks = append(chunks, namedChunk{key: identityKey(tagTable, t.Name), b: cw.b})
		}
		writeNamedChunks(w, chunks)
	} else {
		w.writeCount(0)
	}
	if err := encodeObjects(w, s.Objects, path+".objects"); err != nil {
		return err
	}
	return encodeAttrs(w, s.Attrs, path+".attrs")
}

func encodeTable(w *canonWriter, t *Table, schemaName, path string) error {
	w.writeByte(tagTable)
	w.writeString(t.Name)
	if len(t.Deps) > 0 {
		return fpError(path+".deps", "table dependencies are not supported by canonical representation version %d", CanonicalVersion)
	}
	// Column order is significant and is preserved as-is.
	w.writeCount(len(t.Columns))
	for i, c := range t.Columns {
		if c == nil {
			return fpError(fmt.Sprintf("%s.columns[%d]", path, i), "nil column")
		}
		if err := encodeColumn(w, c, fmt.Sprintf("%s.column[%q]", path, c.Name)); err != nil {
			return err
		}
	}
	// Indexes are a named collection sorted by name.
	chunks := make([]namedChunk, 0, len(t.Indexes))
	for i, idx := range t.Indexes {
		if idx == nil {
			return fpError(fmt.Sprintf("%s.indexes[%d]", path, i), "nil index")
		}
		cw := &canonWriter{}
		p := fmt.Sprintf("%s.index[%q]", path, idx.Name)
		if err := encodeIndex(cw, idx, schemaName, t.Name, p); err != nil {
			return err
		}
		chunks = append(chunks, namedChunk{key: identityKey(tagIndex, idx.Name), b: cw.b})
	}
	writeNamedChunks(w, chunks)
	// Primary key is an optional field: presence is marked explicitly.
	if t.PrimaryKey == nil {
		w.writeBool(false)
	} else {
		w.writeBool(true)
		if err := encodeIndex(w, t.PrimaryKey, schemaName, t.Name, path+".primary_key"); err != nil {
			return err
		}
	}
	// Foreign keys are a named collection sorted by constraint symbol.
	fkChunks := make([]namedChunk, 0, len(t.ForeignKeys))
	for i, fk := range t.ForeignKeys {
		if fk == nil {
			return fpError(fmt.Sprintf("%s.foreign_keys[%d]", path, i), "nil foreign-key")
		}
		cw := &canonWriter{}
		p := fmt.Sprintf("%s.foreign_key[%q]", path, fk.Symbol)
		if err := encodeForeignKey(cw, fk, schemaName, t.Name, p); err != nil {
			return err
		}
		fkChunks = append(fkChunks, namedChunk{key: identityKey(tagForeignKey, fk.Symbol), b: cw.b})
	}
	writeNamedChunks(w, fkChunks)
	return encodeAttrs(w, t.Attrs, path+".attrs")
}

func encodeColumn(w *canonWriter, c *Column, path string) error {
	w.writeByte(tagColumn)
	w.writeString(c.Name)
	// Column type is optional and keeps an explicit presence marker.
	if c.Type == nil {
		w.writeBool(false)
	} else {
		w.writeBool(true)
		w.writeByte(tagColumnType)
		if c.Type.Type == nil {
			return fpError(path+".type", "nil column type")
		}
		if err := encodeType(w, c.Type.Type, path+".type"); err != nil {
			return err
		}
		// Raw is the verbatim database type text and must not be normalized.
		w.writeString(c.Type.Raw)
		w.writeBool(c.Type.Null)
	}
	// A missing default (nil) is different from an explicit NULL literal.
	if err := encodeExpr(w, c.Default, path+".default"); err != nil {
		return err
	}
	return encodeAttrs(w, c.Attrs, path+".attrs")
}

func encodeIndex(w *canonWriter, idx *Index, schemaName, tableName, path string) error {
	w.writeByte(tagIndex)
	w.writeString(idx.Name)
	w.writeBool(idx.Unique)
	// Index-part order is significant and is preserved.
	w.writeCount(len(idx.Parts))
	for i, p := range idx.Parts {
		if p == nil {
			return fpError(fmt.Sprintf("%s.parts[%d]", path, i), "nil index part")
		}
		if err := encodeIndexPart(w, p, schemaName, tableName, fmt.Sprintf("%s.part[%d]", path, i)); err != nil {
			return err
		}
	}
	return encodeAttrs(w, idx.Attrs, path+".attrs")
}

func encodeIndexPart(w *canonWriter, p *IndexPart, schemaName, tableName, path string) error {
	w.writeByte(tagIndexPart)
	w.writeInt(p.SeqNo)
	w.writeBool(p.Desc)
	// Column references are fully-qualified identities, never embedded records.
	if p.C == nil {
		w.writeBool(false)
	} else {
		w.writeBool(true)
		writeColumnRef(w, schemaName, tableName, p.C.Name)
	}
	if err := encodeExpr(w, p.X, path+".expr"); err != nil {
		return err
	}
	return encodeAttrs(w, p.Attrs, path+".attrs")
}

func encodeForeignKey(w *canonWriter, fk *ForeignKey, schemaName, tableName, path string) error {
	w.writeByte(tagForeignKey)
	w.writeString(fk.Symbol)
	// Child columns: order is the composite-key pairing and is preserved.
	w.writeCount(len(fk.Columns))
	for i, c := range fk.Columns {
		if c == nil {
			return fpError(fmt.Sprintf("%s.columns[%d]", path, i), "nil foreign-key column")
		}
		writeColumnRef(w, schemaName, tableName, c.Name)
	}
	// Referenced table is an optional fully-qualified identity. Encoding
	// identities instead of embedded tables guarantees that cyclic foreign
	// keys (including cross-schema cycles) terminate.
	if fk.RefTable == nil {
		w.writeBool(false)
	} else {
		w.writeBool(true)
		refSchema := schemaName
		if fk.RefTable.Schema != nil {
			refSchema = fk.RefTable.Schema.Name
		}
		w.writeString(refSchema)
		w.writeString(fk.RefTable.Name)
		// Referenced columns: pairing order with the child columns is preserved.
		w.writeCount(len(fk.RefColumns))
		for i, c := range fk.RefColumns {
			if c == nil {
				return fpError(fmt.Sprintf("%s.ref_columns[%d]", path, i), "nil referenced column")
			}
			writeColumnRef(w, refSchema, fk.RefTable.Name, c.Name)
		}
	}
	w.writeString(string(fk.OnUpdate))
	w.writeString(string(fk.OnDelete))
	return encodeAttrs(w, fk.Attrs, path+".attrs")
}

// writeColumnRef encodes a fully-qualified column identity as three
// length-prefixed strings. Pointer identity is intentionally irrelevant.
func writeColumnRef(w *canonWriter, schemaName, tableName, columnName string) {
	w.writeString(schemaName)
	w.writeString(tableName)
	w.writeString(columnName)
}

// encodeExpr encodes an optional expression. nil writes only a presence byte;
// every non-nil expression carries its own type tag.
func encodeExpr(w *canonWriter, x Expr, path string) error {
	if x == nil {
		w.writeBool(false)
		return nil
	}
	w.writeBool(true)
	switch e := x.(type) {
	case *Literal:
		w.writeByte(tagLiteral)
		w.writeString(e.V)
	case *RawExpr:
		w.writeByte(tagRawExpr)
		w.writeString(e.X)
	case *NamedDefault:
		w.writeByte(tagNamedDefault)
		w.writeString(e.Name)
		if err := encodeAttrs(w, e.Attrs, path+".attrs"); err != nil {
			return err
		}
		if err := encodeExpr(w, e.Expr, path+".expr"); err != nil {
			return err
		}
	default:
		return fpError(path, "unsupported expression type %T: only Literal, RawExpr and NamedDefault are supported", x)
	}
	return nil
}

// encodeType dispatches on the explicitly supported concrete Type set. Wrapper
// types are not unwrapped automatically, because their additional fields would
// otherwise be silently dropped.
func encodeType(w *canonWriter, t Type, path string) error {
	switch e := t.(type) {
	case *BoolType:
		w.writeByte(tagBoolType)
		w.writeString(e.T)
	case *EnumType:
		w.writeByte(tagEnumType)
		if err := writeEnumBody(w, e, path); err != nil {
			return err
		}
	case *TimeType:
		w.writeByte(tagTimeType)
		w.writeString(e.T)
		writeOptionalInt(w, e.Precision)
		writeOptionalInt(w, e.Scale)
		if err := encodeAttrs(w, e.Attrs, path+".attrs"); err != nil {
			return err
		}
	case *JSONType:
		w.writeByte(tagJSONType)
		w.writeString(e.T)
	case *FloatType:
		w.writeByte(tagFloatType)
		w.writeString(e.T)
		w.writeBool(e.Unsigned)
		w.writeInt(e.Precision)
	case *StringType:
		w.writeByte(tagStringType)
		w.writeString(e.T)
		w.writeInt(e.Size)
		if err := encodeAttrs(w, e.Attrs, path+".attrs"); err != nil {
			return err
		}
	case *BinaryType:
		w.writeByte(tagBinaryType)
		w.writeString(e.T)
		writeOptionalInt(w, e.Size)
	case *SpatialType:
		w.writeByte(tagSpatialType)
		w.writeString(e.T)
	case *UUIDType:
		w.writeByte(tagUUIDType)
		w.writeString(e.T)
	case *IntegerType:
		w.writeByte(tagIntegerType)
		w.writeString(e.T)
		w.writeBool(e.Unsigned)
		if err := encodeAttrs(w, e.Attrs, path+".attrs"); err != nil {
			return err
		}
	case *DecimalType:
		w.writeByte(tagDecimalType)
		w.writeString(e.T)
		w.writeInt(e.Precision)
		w.writeInt(e.Scale)
		w.writeBool(e.Unsigned)
	case *UnsupportedType:
		w.writeByte(tagUnsupportedType)
		w.writeString(e.T)
	default:
		return fpError(path, "unsupported column type %T: only the schema package core types are supported", t)
	}
	return nil
}

// writeOptionalInt preserves the distinction between an unset pointer and an
// explicit zero value.
func writeOptionalInt(w *canonWriter, v *int) {
	if v == nil {
		w.writeBool(false)
	} else {
		w.writeBool(true)
		w.writeInt(*v)
	}
}

// writeEnumBody writes the fields shared by inline enum types and named enum
// objects. Enum value order is significant and preserved.
func writeEnumBody(w *canonWriter, e *EnumType, path string) error {
	w.writeString(e.T)
	w.writeCount(len(e.Values))
	for _, v := range e.Values {
		w.writeString(v)
	}
	return encodeAttrs(w, e.Attrs, path+".attrs")
}

// encodeObjects encodes Realm/Schema object collections. Only named objects of
// the explicitly supported kinds are represented; anything else is an error.
func encodeObjects(w *canonWriter, objs []Object, path string) error {
	chunks := make([]namedChunk, 0, len(objs))
	for i, o := range objs {
		p := fmt.Sprintf("%s[%d]", path, i)
		if o == nil {
			return fpError(p, "nil object")
		}
		cw := &canonWriter{}
		switch e := o.(type) {
		case *EnumType:
			cw.writeByte(tagEnumObject)
			if err := writeEnumBody(cw, e, p); err != nil {
				return err
			}
			chunks = append(chunks, namedChunk{key: identityKey(tagEnumObject, e.T), b: cw.b})
		default:
			return fpError(p, "unsupported object type %T: only named EnumType objects are supported", o)
		}
	}
	writeNamedChunks(w, chunks)
	return nil
}

// encodeAttrs encodes an attribute collection. Each attribute is encoded
// independently and the resulting records are sorted canonically (by bytes),
// which makes attribute order irrelevant while keeping every record
// self-delimiting. Pos is intentionally excluded: source positions are not
// structural identity. Unknown attributes produce an error instead of being
// dropped.
func encodeAttrs(w *canonWriter, attrs []Attr, path string) error {
	chunks := make([][]byte, 0, len(attrs))
	for i, a := range attrs {
		if a == nil {
			return fpError(fmt.Sprintf("%s[%d]", path, i), "nil attribute")
		}
		if _, ok := a.(*Pos); ok {
			continue
		}
		cw := &canonWriter{}
		p := fmt.Sprintf("%s[%d]", path, i)
		if err := encodeAttr(cw, a, p); err != nil {
			return err
		}
		chunks = append(chunks, cw.b)
	}
	sort.SliceStable(chunks, func(i, j int) bool {
		return bytes.Compare(chunks[i], chunks[j]) < 0
	})
	w.writeCount(len(chunks))
	for _, c := range chunks {
		w.writeBytes(c)
	}
	return nil
}

func encodeAttr(w *canonWriter, a Attr, path string) error {
	switch e := a.(type) {
	case *Comment:
		w.writeByte(tagComment)
		w.writeString(e.Text)
	case *Charset:
		w.writeByte(tagCharset)
		w.writeString(e.V)
	case *Collation:
		w.writeByte(tagCollation)
		w.writeString(e.V)
	case *GeneratedExpr:
		w.writeByte(tagGeneratedExpr)
		w.writeString(e.Expr)
		w.writeString(e.Type)
	case *Check:
		w.writeByte(tagCheck)
		w.writeString(e.Name)
		// The CHECK expression text is preserved verbatim.
		w.writeString(e.Expr)
		if err := encodeAttrs(w, e.Attrs, path+".attrs"); err != nil {
			return err
		}
	default:
		return fpError(path, "unsupported attribute type %T: only Comment, Charset, Collation, GeneratedExpr, Check and Pos(excluded) are handled", a)
	}
	return nil
}
