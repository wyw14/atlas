// Copyright 2021-present The Atlas Authors. All rights reserved.
// This source code is licensed under the Apache 2.0 license found
// in the LICENSE file in the root directory of this source tree.

package migrate

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestAssertValue(t *testing.T) {
	tests := []struct {
		name       string
		value      any
		wantOK     bool
		wantReason string
	}{
		{name: "bool true", value: true, wantOK: true},
		{name: "bool false", value: false, wantReason: "returned false"},
		{name: "nil", value: nil, wantReason: "returned NULL"},
		{name: "int64 one", value: int64(1), wantOK: true},
		{name: "int64 zero", value: int64(0), wantReason: "returned 0"},
		{name: "int64 negative", value: int64(-1), wantReason: "returned -1"},
		{name: "int one", value: int(1), wantOK: true},
		{name: "uint64 one", value: uint64(1), wantOK: true},
		{name: "uint64 two", value: uint64(2), wantReason: "returned 2"},
		{name: "float", value: float64(1), wantReason: "type"},
		{name: "string", value: "1", wantReason: "type"},
		{name: "bytes", value: []byte("1"), wantReason: "type"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ok, reason := assertValue(tt.value)
			require.Equal(t, tt.wantOK, ok)
			if tt.wantOK {
				require.Empty(t, reason)
			} else {
				require.Contains(t, reason, tt.wantReason)
			}
		})
	}
}

func TestAssertDirectives(t *testing.T) {
	f := NewLocalFile("1.sql", []byte(`-- atlas:assert SELECT 1;
-- atlas:assert SELECT 2;

CREATE TABLE t(c int);
`))
	require.Equal(t, []string{"SELECT 1;", "SELECT 2;"}, f.AssertDirectives())
	require.False(t, f.sumIgnored())

	ignored := NewLocalFile("2.sql", []byte("-- atlas:sum ignore\n-- atlas:assert SELECT 1;\n\nSELECT 1;\n"))
	require.True(t, ignored.sumIgnored())
	require.Equal(t, []string{"SELECT 1;"}, ignored.AssertDirectives())

	// Statement-associated directives are not file-level directives.
	attached := NewLocalFile("3.sql", []byte("-- atlas:assert SELECT 1;\nCREATE TABLE t(c int);\n"))
	require.Nil(t, attached.AssertDirectives())
}
