//go:build devdb

package corpus

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/moznion/go-sqletch/internal/cache"
	"github.com/moznion/go-sqletch/internal/devdb"
	"github.com/moznion/go-sqletch/internal/dialect"
	"github.com/moznion/go-sqletch/internal/dialect/mysql"
)

// adversarialDDL sweeps the type surface the native backend claims to
// support, plus the ones it refuses (ENUM/SET) — the differential
// below adjudicates every claim against a real MySQL.
const adversarialDDL = `
CREATE TABLE every_type (
    id      BIGINT AUTO_INCREMENT PRIMARY KEY,
    c_int   INT NOT NULL,
    c_uint  INT UNSIGNED NOT NULL,
    c_tiny  TINYINT NOT NULL,
    c_bool  TINYINT(1) NOT NULL,
    c_small SMALLINT NOT NULL,
    c_med   MEDIUMINT NOT NULL,
    c_ubig  BIGINT UNSIGNED NOT NULL,
    c_vchar VARCHAR(100),
    c_char  CHAR(8),
    c_text  TEXT,
    c_ttext TINYTEXT,
    c_mtext MEDIUMTEXT,
    c_ltext LONGTEXT,
    c_blob  BLOB,
    c_tblob TINYBLOB,
    c_mblob MEDIUMBLOB,
    c_lblob LONGBLOB,
    c_vbin  VARBINARY(16),
    c_bin   BINARY(4),
    c_dec   DECIMAL(10,2) NOT NULL,
    c_float FLOAT,
    c_dbl   DOUBLE,
    c_date  DATE,
    c_dt    DATETIME(3),
    c_ts    TIMESTAMP(6) NULL,
    c_time  TIME,
    c_year  YEAR,
    c_json  JSON,
    c_bit   BIT(5),
    c_mood  ENUM('a','b') NOT NULL,
    c_tags  SET('x','y') NOT NULL
);
CREATE TABLE second (
    id    BIGINT PRIMARY KEY,
    ref   BIGINT NOT NULL,
    label VARCHAR(32) NOT NULL
);
`

// Statements both backends must accept, byte-identically.
var agreeSQL = []string{
	"SELECT t.id, t.c_int, t.c_uint, t.c_tiny, t.c_bool, t.c_small, t.c_med, t.c_ubig FROM every_type AS t",
	"SELECT t.c_vchar, t.c_char, t.c_text, t.c_ttext, t.c_mtext, t.c_ltext FROM every_type AS t",
	"SELECT t.c_blob, t.c_tblob, t.c_mblob, t.c_lblob, t.c_vbin, t.c_bin FROM every_type AS t",
	"SELECT t.c_dec, t.c_float, t.c_dbl FROM every_type AS t",
	"SELECT t.c_date, t.c_dt, t.c_ts, t.c_time, t.c_year FROM every_type AS t",
	"SELECT t.c_json, t.c_bit FROM every_type AS t",
	"SELECT s.* FROM second AS s",
	"SELECT label FROM second",
	"SELECT s.label AS tag, e.c_int FROM second AS s JOIN every_type AS e ON e.id = s.ref",
	"SELECT s.id FROM second AS s WHERE s.ref = ? AND s.label IN (?)",
	"SELECT s.id FROM second AS s WHERE s.label IN (SELECT NULL FROM DUAL WHERE FALSE)",
	"-- @column total: bigint\nSELECT count(*) AS total FROM second",
	"-- @column mx: varchar(32)\nSELECT max(label) AS mx FROM second GROUP BY ref HAVING mx > ? ORDER BY mx",
	"INSERT INTO second (id, ref, label) VALUES (?, ?, ?)",
	"INSERT INTO second (id, ref, label) VALUES (?, ?, ?) ON DUPLICATE KEY UPDATE label = ?",
	"UPDATE second AS s SET s.label = ? WHERE s.id = ?",
	"DELETE FROM second WHERE id = ? ORDER BY id LIMIT 3",
}

// Statements both backends must reject (engine parity: OracleError).
var bothRejectSQL = []string{
	"SELECT nope FROM second",
	"SELECT s.nope FROM second AS s",
	"SELECT id FROM missing_table",
	"SELECT id FROM every_type, second",
	"INSERT INTO second (id, ref) VALUES (?, ?, ?)",
	"UPDATE second SET ghost = ?",
}

// Statements the server accepts but the native backend refuses — the
// tolerable direction, allowed ONLY as deliberate subset exclusions.
var nativeRefusesSQL = []string{
	"SELECT c_mood FROM every_type",
	"SELECT c_tags FROM every_type",
	"SELECT count(*) FROM second",
	"SELECT (SELECT max(id) FROM second) AS m FROM second",
	"SELECT x.id FROM (SELECT id FROM second) AS x",
}

// TestNativeDifferential is differential mode 1 (design 15 §7): both
// backends over an adversarial schema, byte-identical answers,
// direction-aware error agreement. Engine-rejects/native-accepts is
// the catastrophic direction and fails hard.
func TestNativeDifferential(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	schema := []cache.SchemaFile{{Path: "adversarial.sql", Content: []byte(adversarialDDL)}}
	conn, cleanup, err := devdb.AcquireMySQL(ctx, devdb.Config{
		DSN:              os.Getenv("SQLETCH_TEST_MYSQL_DSN"),
		AllowDestructive: true,
		ServerVersion:    "8.4",
		SchemaSQL:        []string{adversarialDDL},
	})
	if cleanup != nil {
		defer cleanup()
	}
	if err != nil {
		t.Fatal(err)
	}
	server := mysql.NewOracle(conn)
	native, err := mysql.NewNativeOracle(schema, "8.4")
	if err != nil {
		t.Fatal(err)
	}

	t.Run("catalog", func(t *testing.T) {
		want, err := server.Snapshot(ctx)
		if err != nil {
			t.Fatal(err)
		}
		got, err := native.Snapshot(ctx)
		if err != nil {
			t.Fatal(err)
		}
		want.SchemaFP, got.SchemaFP = "fp", "fp"
		wb, err1 := cache.EncodeCatalog(want)
		gb, err2 := cache.EncodeCatalog(got)
		if err1 != nil || err2 != nil {
			t.Fatal(err1, err2)
		}
		if !bytes.Equal(wb, gb) {
			t.Errorf("catalog drift:\nserver: %s\nnative: %s", wb, gb)
		}
	})

	entryBytes := func(t *testing.T, o dialect.Oracle, sql string) ([]byte, error) {
		t.Helper()
		desc, err := o.Describe(ctx, sql)
		if err != nil {
			return nil, err
		}
		return cache.EncodeOracle(dialect.EntryFromDesc("fp", sql, desc))
	}

	t.Run("agree", func(t *testing.T) {
		for _, sql := range agreeSQL {
			want, err := entryBytes(t, server, sql)
			if err != nil {
				t.Errorf("server rejects an agree-case: %s: %v", sql, err)
				continue
			}
			got, err := entryBytes(t, native, sql)
			if err != nil {
				t.Errorf("native rejects what the server accepts: %s: %v", sql, err)
				continue
			}
			if !bytes.Equal(want, got) {
				t.Errorf("answer drift on %s:\nserver: %s\nnative: %s", sql, want, got)
			}
		}
	})

	t.Run("both reject", func(t *testing.T) {
		for _, sql := range bothRejectSQL {
			if _, err := server.Describe(ctx, sql); err == nil {
				t.Errorf("server unexpectedly accepts: %s", sql)
				continue
			}
			_, err := native.Describe(ctx, sql)
			var oe *dialect.OracleError
			if !errors.As(err, &oe) {
				// err == nil here is the catastrophic direction:
				// native accepting what the engine rejects.
				t.Errorf("native must reject with engine parity: %s: got %v", sql, err)
			}
		}
	})

	t.Run("subset refusals", func(t *testing.T) {
		for _, sql := range nativeRefusesSQL {
			if _, err := server.Describe(ctx, sql); err != nil {
				t.Errorf("refusal case should be server-acceptable: %s: %v", sql, err)
			}
			_, err := native.Describe(ctx, sql)
			var ne *dialect.NativeUnsupportedError
			if !errors.As(err, &ne) {
				t.Errorf("want a deliberate NativeUnsupportedError for %s, got %v", sql, err)
			}
		}
	})

	t.Run("generative", func(t *testing.T) {
		for _, sql := range generativeSQL() {
			want, err := entryBytes(t, server, sql)
			if err != nil {
				t.Errorf("generator emitted server-rejected SQL (generator bug): %s: %v", sql, err)
				continue
			}
			got, err := entryBytes(t, native, sql)
			if err != nil {
				t.Errorf("native rejects a generated subset statement: %s: %v", sql, err)
				continue
			}
			if !bytes.Equal(want, got) {
				t.Errorf("generative drift on %s:\nserver: %s\nnative: %s", sql, want, got)
			}
		}
	})
}

// ---- seeded generative differential (design 15 §7.3) -----------------------
//
// A deterministic generator over the v1 subset grammar; every
// generated statement must be accepted by BOTH backends with
// byte-identical answers. Implemented as a seeded generator rather
// than a go-fuzz target because each verdict needs the live server —
// the offline robustness half is FuzzNativeDescribe.

type sqlGen struct{ r *rand.Rand }

// Projection-safe every_type columns (ENUM/SET excluded by design).
var etCols = []string{
	"id", "c_int", "c_uint", "c_tiny", "c_bool", "c_small", "c_med",
	"c_ubig", "c_vchar", "c_char", "c_text", "c_ttext", "c_mtext",
	"c_ltext", "c_blob", "c_tblob", "c_mblob", "c_lblob", "c_vbin",
	"c_bin", "c_dec", "c_float", "c_dbl", "c_date", "c_dt", "c_ts",
	"c_time", "c_year", "c_json", "c_bit",
}
var etNullable = []string{"c_vchar", "c_text", "c_blob", "c_date", "c_ts", "c_json"}
var secondCols = []string{"id", "ref", "label"}

func (g *sqlGen) pick(ss []string) string { return ss[g.r.IntN(len(ss))] }

func (g *sqlGen) next() string {
	switch g.r.IntN(10) {
	case 0:
		return g.insert()
	case 1:
		return g.update()
	case 2:
		return g.delete()
	case 3, 4:
		return g.aggregateSelect()
	default:
		return g.plainSelect()
	}
}

func (g *sqlGen) plainSelect() string {
	join := g.r.IntN(3) == 0
	var items, conds []string
	if join {
		for n := 1 + g.r.IntN(3); n > 0; n-- {
			if g.r.IntN(2) == 0 {
				items = append(items, "s."+g.pick(secondCols))
			} else {
				items = append(items, "t."+g.pick(etCols))
			}
		}
	} else {
		for n := 1 + g.r.IntN(3); n > 0; n-- {
			col := g.pick(secondCols)
			if g.r.IntN(3) == 0 {
				col = "s." + col
			}
			items = append(items, col)
		}
	}
	for n := g.r.IntN(3); n > 0; n-- {
		switch g.r.IntN(4) {
		case 0:
			conds = append(conds, "s.id = ?")
		case 1:
			conds = append(conds, "s.ref > ?")
		case 2:
			conds = append(conds, "s.label IN (?)")
		default:
			conds = append(conds, "s.label IN (SELECT NULL FROM DUAL WHERE FALSE)")
		}
	}
	if join && g.r.IntN(2) == 0 {
		conds = append(conds, "t."+g.pick(etNullable)+" IS NULL")
	}
	sql := "SELECT " + strings.Join(items, ", ") + " FROM second AS s"
	if join {
		sql += " JOIN every_type AS t ON t.id = s.ref"
	}
	if len(conds) > 0 {
		sql += " WHERE " + strings.Join(conds, " AND ")
	}
	if g.r.IntN(2) == 0 {
		sql += " ORDER BY s.id"
		if g.r.IntN(2) == 0 {
			sql += " DESC"
		}
	}
	if g.r.IntN(2) == 0 {
		sql += " LIMIT ?"
	}
	return sql
}

// aggregateSelect emits aggregate-only projections so
// ONLY_FULL_GROUP_BY (on by default) accepts every generated shape.
func (g *sqlGen) aggregateSelect() string {
	var hints, items []string
	n := 1 + g.r.IntN(2)
	for i := range n {
		alias := fmt.Sprintf("agg_%d", i)
		switch g.r.IntN(3) {
		case 0:
			hints = append(hints, fmt.Sprintf("-- @column %s: bigint", alias))
			items = append(items, "count(*) AS "+alias)
		case 1:
			hints = append(hints, fmt.Sprintf("-- @column %s: bigint", alias))
			items = append(items, "max(s.id) AS "+alias)
		default:
			hints = append(hints, fmt.Sprintf("-- @column %s: varchar(32)", alias))
			items = append(items, "min(s.label) AS "+alias)
		}
	}
	sql := strings.Join(hints, "\n") + "\nSELECT " + strings.Join(items, ", ") + " FROM second AS s"
	grouped := g.r.IntN(2) == 0
	if grouped {
		sql += " GROUP BY s.ref"
		if g.r.IntN(2) == 0 {
			sql += " HAVING agg_0 > ?"
		}
	}
	if g.r.IntN(2) == 0 {
		sql += " ORDER BY agg_0"
	}
	return sql
}

func (g *sqlGen) insert() string {
	if g.r.IntN(2) == 0 {
		return "INSERT INTO second (id, ref, label) VALUES (?, ?, ?)"
	}
	return "INSERT INTO second (id, ref, label) VALUES (?, ?, ?) ON DUPLICATE KEY UPDATE label = ?, ref = ref + 1"
}

func (g *sqlGen) update() string {
	if g.r.IntN(2) == 0 {
		return "UPDATE second AS s SET s.label = ?, s.ref = s.ref + 1 WHERE s.id = ?"
	}
	return "UPDATE second SET label = ? WHERE ref IN (?)"
}

func (g *sqlGen) delete() string {
	if g.r.IntN(2) == 0 {
		return "DELETE FROM second WHERE id = ?"
	}
	return "DELETE FROM second WHERE label IN (?) ORDER BY id LIMIT 5"
}

const generativeCases = 200

func generativeSQL() []string {
	g := &sqlGen{r: rand.New(rand.NewPCG(20260802, 15))}
	out := make([]string, 0, generativeCases)
	for range generativeCases {
		out = append(out, g.next())
	}
	return out
}
