package mysql

import (
	"strings"
	"testing"
)

// TestBuildCatalogMixedCaseByteOrder pins that synthetic OIDs are
// assigned in byte order (Go sort.Strings), NOT under a case-insensitive
// collation. This is the ordering the server snapshot must match for
// byte-identity across oracle backends (design 15 §3): the server query
// orders table_name as BINARY for exactly this reason. Under the
// information_schema default (case-insensitive) collation the two
// backends disagreed on mixed-case schemas.
//
// Byte order of the four names below (first byte): 'M'=0x4D < 'Z'=0x5A <
// 'a'=0x61 < 0xC3 (leading byte of "Ärger"). A case-insensitive
// collation would instead yield apple/Ärger (A), Mango (M), Zebra (Z) —
// a different OID assignment, which is the bug this guards against.
func TestBuildCatalogMixedCaseByteOrder(t *testing.T) {
	cat := buildOne(t, "CREATE TABLE `apple` (id BIGINT);\n"+
		"CREATE TABLE `Zebra` (id BIGINT);\n"+
		"CREATE TABLE `Mango` (id BIGINT);\n"+
		"CREATE TABLE `Ärger` (id BIGINT);\n")

	want := []string{"Mango", "Zebra", "apple", "Ärger"}
	if len(cat.Tables) != len(want) {
		t.Fatalf("want %d tables, got %d: %+v", len(want), len(cat.Tables), cat.Tables)
	}
	for i, name := range want {
		if cat.Tables[i].Name != name {
			t.Errorf("table %d: want %q, got %q (OID order must be byte-wise)", i, name, cat.Tables[i].Name)
		}
		if cat.Tables[i].OID != uint32(i+1) {
			t.Errorf("table %q: want OID %d, got %d", name, i+1, cat.Tables[i].OID)
		}
	}
}

// TestSnapshotQueryOrdersBinary is the fail-before/pass-after guard for
// the fix: the server snapshot must sort table_name as BINARY so its
// synthetic OID assignment is byte-identical to the native builder's
// sort.Strings. A regression back to the collation default would
// silently re-order mixed-case catalogs and break byte-identity.
func TestSnapshotQueryOrdersBinary(t *testing.T) {
	if !strings.Contains(snapshotQuery, "ORDER BY BINARY c.table_name") {
		t.Fatalf("snapshotQuery must ORDER BY BINARY c.table_name (byte order, matching the native builder); got:\n%s", snapshotQuery)
	}
}
