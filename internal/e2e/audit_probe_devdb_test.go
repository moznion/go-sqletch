//go:build devdb

package e2e_test

import (
	"testing"

	"github.com/moznion/go-sqletch/internal/ast"
	"github.com/moznion/go-sqletch/internal/dialect/postgres"
	"github.com/moznion/go-sqletch/internal/nullability"
)

const auditProbeSchemaSQL = `
DROP SCHEMA IF EXISTS auditp CASCADE;
CREATE SCHEMA auditp;
CREATE TABLE auditp.orgs (
    id   bigint PRIMARY KEY,
    name text NOT NULL
);
CREATE TABLE auditp.bin (
    id     bigint PRIMARY KEY,
    org_id bigint,
    data   bytea NOT NULL
);
CREATE TABLE auditp.g (
    id   bigint PRIMARY KEY,
    a    bigint NOT NULL,
    gen  bigint NOT NULL GENERATED ALWAYS AS (a + 1) STORED
);
CREATE TABLE auditp.j1 (id bigint PRIMARY KEY, k bigint NOT NULL);
CREATE TABLE auditp.j2 (id bigint PRIMARY KEY, k bigint NOT NULL);
CREATE FUNCTION auditp.bin_of(p_org bigint)
  RETURNS TABLE(data bytea) LANGUAGE sql AS
  $$ SELECT b.data FROM auditp.bin b WHERE b.org_id = p_org $$;
`

const auditProbeSeedSQL = `
INSERT INTO auditp.orgs VALUES (1, 'has-bin'), (2, 'no-bin');
INSERT INTO auditp.bin VALUES (1, 1, '\xdeadbeef');
INSERT INTO auditp.g (id, a) VALUES (1, 10);
INSERT INTO auditp.j1 VALUES (1, 100), (2, 200);
INSERT INTO auditp.j2 VALUES (1, 100);
`

var auditProbeCases = []struct{ name, src, note string }{
	{
		name: "lateral_left_join_bytea",
		src: `-- name: LatLeft :many
SELECT s.data FROM auditp.orgs o
LEFT JOIN LATERAL (SELECT b.data FROM auditp.bin b WHERE b.org_id = o.id) s ON true;`,
		note: "org 2 has no bin row; LEFT JOIN LATERAL null-extends s.data (bytea)",
	},
	{
		name: "comma_lateral_bytea",
		src: `-- name: CommaLat :many
SELECT s.data FROM auditp.orgs o, LATERAL (SELECT b.data FROM auditp.bin b WHERE b.org_id = o.id) s;`,
		note: "CROSS JOIN LATERAL drops org 2; s.data should be genuinely non-null",
	},
	{
		name: "values_derived_bytea",
		src: `-- name: ValDer :many
SELECT v.data FROM (VALUES (NULL::bytea)) AS v(data);`,
		note: "VALUES column is NULL; must be nullable",
	},
	{
		name: "function_in_from_bytea",
		src: `-- name: FnFrom :many
SELECT o.id, f.data FROM auditp.orgs o LEFT JOIN LATERAL auditp.bin_of(o.id) f ON true;`,
		note: "SRF returns no row for org 2; f.data null-extended",
	},
	{
		name: "correlated_scalar_subquery_bytea",
		src: `-- name: CorrScalar :many
SELECT o.id, (SELECT b.data FROM auditp.bin b WHERE b.org_id = o.id LIMIT 1) AS d FROM auditp.orgs o;`,
		note: "scalar subquery is NULL for org 2",
	},
	{
		name: "full_join_using_notnull",
		src: `-- name: FullUsing :many
SELECT k FROM auditp.j1 FULL JOIN auditp.j2 USING (k);`,
		note: "FULL JOIN USING coalesces k; unmatched rows keep the present side",
	},
	{
		name: "right_join_using_notnull",
		src: `-- name: RightUsing :many
SELECT k FROM auditp.j1 RIGHT JOIN auditp.j2 USING (k);`,
		note: "RIGHT JOIN preserves j2; merged k = j2.k",
	},
	{
		name: "left_join_using_notnull_leftside",
		src: `-- name: LeftUsing :many
SELECT k FROM auditp.j1 LEFT JOIN auditp.j2 USING (k);`,
		note: "LEFT JOIN preserves j1; merged k = j1.k (non-null)",
	},
	{
		name: "generated_column_left_join",
		src: `-- name: GenLeft :many
SELECT g.gen FROM auditp.orgs o LEFT JOIN auditp.g ON g.id = o.id;`,
		note: "generated NOT NULL column null-extended by LEFT JOIN (org 2 has no g row)",
	},
	{
		name: "natural_full_join_notnull",
		src: `-- name: NatFull :many
SELECT k FROM auditp.j1 NATURAL FULL JOIN auditp.j2;`,
		note: "NATURAL FULL JOIN coalesces shared columns",
	},
}

func TestAuditProbeNullabilitySoundness(t *testing.T) {
	conn, ctx := acquireWithSchema(t, auditProbeSchemaSQL)
	oracle := postgres.NewOracle(conn)
	if _, err := conn.Exec(ctx, auditProbeSeedSQL); err != nil {
		t.Fatalf("seed: %v", err)
	}
	cat, err := oracle.Snapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range auditProbeCases {
		t.Run(tc.name, func(t *testing.T) {
			q := compile(t, tc.src)
			rs, err := ast.Renderings(postgres.Profile{}, q)
			if err != nil {
				t.Fatal(err)
			}
			desc, err := oracle.Describe(ctx, rs[0].SQL)
			if err != nil {
				t.Fatalf("describe: %v", err)
			}
			tree, err := postgres.Frontend{}.Parse(rs[0].SQL)
			if err != nil {
				t.Fatal(err)
			}
			verdict := nullability.Analyze(tree, rs[0], desc, cat, nil)
			for i, c := range desc.Columns {
				t.Logf("col %d %q SrcRel=%d SrcAtt=%d -> nullable=%v", i, c.Name, c.SrcRel, c.SrcAtt, verdict[i])
			}
			rows, err := conn.Query(ctx, rs[0].SQL)
			if err != nil {
				t.Fatalf("execute: %v", err)
			}
			sawNull := make([]bool, len(desc.Columns))
			nRows := 0
			for rows.Next() {
				vals, err := rows.Values()
				if err != nil {
					t.Fatal(err)
				}
				nRows++
				for i, v := range vals {
					if v == nil {
						sawNull[i] = true
					}
				}
			}
			rows.Close()
			if err := rows.Err(); err != nil {
				t.Fatal(err)
			}
			t.Logf("nRows=%d sawNull=%v", nRows, sawNull)
			for i := range verdict {
				if !verdict[i] && sawNull[i] {
					t.Errorf("SOUNDNESS VIOLATION: col %d %q non-nullable but NULL observed (%s)", i, desc.Columns[i].Name, tc.note)
				}
			}
		})
	}
}
