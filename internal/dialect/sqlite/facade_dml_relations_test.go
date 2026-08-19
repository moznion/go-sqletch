package sqlite

import (
	"slices"
	"testing"
)

// M10: the SQLite facade must expose WITH clauses on DML statements and
// UPDATE … FROM sources, so the policy weaver (which compares
// Relations()/DeepTables() against each other) can see — and scope or
// reject — a designated table hiding in those positions. Before the
// fix CTEs() read only the SELECT statement (nil for DML) and neither
// DeepTables() nor Relations() walked a DML WITH body or the UPDATE
// FROM source, so a designated table there was silently invisible.

func deepTableNames(t *testing.T, sql string) []string {
	t.Helper()
	tree, err := Frontend{}.Parse(sql)
	if err != nil {
		t.Fatalf("parse %q: %v", sql, err)
	}
	var got []string
	for _, tr := range tree.DeepTables() {
		got = append(got, tr.Name)
	}
	slices.Sort(got)
	return got
}

func cteNames(t *testing.T, sql string) []string {
	t.Helper()
	tree, err := Frontend{}.Parse(sql)
	if err != nil {
		t.Fatalf("parse %q: %v", sql, err)
	}
	var got []string
	for _, c := range tree.CTEs() {
		got = append(got, c.Name)
	}
	slices.Sort(got)
	return got
}

func relTableNames(t *testing.T, sql string) []string {
	t.Helper()
	tree, err := Frontend{}.Parse(sql)
	if err != nil {
		t.Fatalf("parse %q: %v", sql, err)
	}
	var got []string
	for _, r := range tree.Relations() {
		if r.Table != "" {
			got = append(got, r.Table)
		}
	}
	slices.Sort(got)
	return got
}

func TestCTEs_OnDMLStatements(t *testing.T) {
	cases := []struct {
		name string
		sql  string
	}{
		{"update", "WITH x AS (SELECT id FROM tenants) UPDATE audit SET n = 1 WHERE audit.id IN (SELECT id FROM x)"},
		{"delete", "WITH x AS (SELECT id FROM tenants) DELETE FROM audit WHERE audit.id IN (SELECT id FROM x)"},
		{"insert", "WITH x AS (SELECT id FROM tenants) INSERT INTO audit (id) SELECT id FROM x"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := cteNames(t, tc.sql); !slices.Equal(got, []string{"x"}) {
				t.Errorf("CTEs() names = %v, want [x] — a WITH on a DML statement must be visible", got)
			}
		})
	}
}

func TestDeepTables_DMLWithBodyReachesDesignatedTable(t *testing.T) {
	cases := []struct {
		name string
		sql  string
		want []string
	}{
		{
			"update-with",
			"WITH x AS (SELECT id FROM tenants) UPDATE audit SET n = 1 WHERE audit.id IN (SELECT id FROM x)",
			[]string{"audit", "tenants", "x"},
		},
		{
			"delete-with",
			"WITH x AS (SELECT id FROM tenants) DELETE FROM audit WHERE audit.id IN (SELECT id FROM x)",
			[]string{"audit", "tenants", "x"},
		},
		{
			"insert-with",
			"WITH x AS (SELECT id FROM tenants) INSERT INTO audit (id) SELECT id FROM x",
			[]string{"audit", "tenants", "x"},
		},
		{
			"update-from",
			"UPDATE audit SET n = n + 1 FROM tenants WHERE audit.tid = tenants.id",
			[]string{"audit", "tenants"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := deepTableNames(t, tc.sql); !slices.Equal(got, tc.want) {
				t.Errorf("DeepTables() = %v, want %v — a designated table in a DML WITH/UPDATE…FROM must not be invisible", got, tc.want)
			}
		})
	}
}

func TestRelations_UpdateFromSource(t *testing.T) {
	// UPDATE … FROM sources are ordinary FROM relations: the weaver's
	// Relations()/DeepTables() comparison needs to see them, and
	// R3 needs them declared so FROM-column refs resolve.
	got := relTableNames(t, "UPDATE audit SET n = n + 1 FROM tenants JOIN regions r ON r.id = tenants.rid WHERE audit.tid = tenants.id")
	want := []string{"audit", "regions", "tenants"}
	if !slices.Equal(got, want) {
		t.Errorf("Relations() tables = %v, want %v", got, want)
	}
}
