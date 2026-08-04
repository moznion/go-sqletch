package mysql

import (
	"bytes"
	"context"
	"errors"
	"testing"

	"github.com/moznion/go-sqletch/internal/cache"
	"github.com/moznion/go-sqletch/internal/dialect"
)

// FuzzNativeDescribe is the offline half of the native backend's fuzz
// coverage (design 15 §7.3): on arbitrary input, Describe must return
// either an answer or one of the two typed errors — never panic, and
// never answer non-deterministically. Verdict agreement with the real
// engine is the devdb generative differential's job; this target
// hardens the surface that runs on every contributor's machine.
func FuzzNativeDescribe(f *testing.F) {
	oracle, err := NewNativeOracle(
		[]cache.SchemaFile{{Path: "s.sql", Content: []byte(nativeTestDDL)}}, "8.4")
	if err != nil {
		f.Fatal(err)
	}

	seeds := []string{
		"SELECT u.id, u.email FROM users AS u WHERE u.id = ? LIMIT ?",
		"SELECT o.*, u.id FROM users AS u JOIN orgs AS o ON o.id = u.org_id",
		"-- @column total: bigint\nSELECT count(*) AS total FROM users GROUP BY org_id HAVING total > ?",
		"SELECT u.id FROM users AS u WHERE u.email IN (SELECT NULL FROM DUAL WHERE FALSE)",
		"INSERT INTO users (email, org_id) VALUES (?, ?) ON DUPLICATE KEY UPDATE email = ?",
		"UPDATE users AS u SET u.email = ? WHERE u.id = ?",
		"DELETE FROM users WHERE id = ? ORDER BY id LIMIT 3",
		"SELECT (SELECT max(id) FROM users) AS m FROM users",
		"SELECT count(*) FROM users",
		"SELECT mood FROM users",
		"SELECT nope FROM users; SELECT 1",
		"SELECT * FROM",
		"-- @column x: varchar(9999)\nSELECT concat(email, '?') AS x FROM users -- ?",
	}
	for _, s := range seeds {
		f.Add(s)
	}

	f.Fuzz(func(t *testing.T, sql string) {
		ctx := context.Background()
		desc, err := oracle.Describe(ctx, sql)
		if err != nil {
			var oe *dialect.OracleError
			var ne *dialect.NativeUnsupportedError
			if !errors.As(err, &oe) && !errors.As(err, &ne) {
				t.Fatalf("Describe error must be typed, got %T: %v", err, err)
			}
			return
		}
		// Accepted input: the answer must be deterministic — the
		// cache byte-identity contract collapses otherwise.
		again, err := oracle.Describe(ctx, sql)
		if err != nil {
			t.Fatalf("accept then reject on identical input: %v", err)
		}
		a, err1 := cache.EncodeOracle(dialect.EntryFromDesc("fp", sql, desc))
		b, err2 := cache.EncodeOracle(dialect.EntryFromDesc("fp", sql, again))
		if err1 != nil || err2 != nil {
			t.Fatal(err1, err2)
		}
		if !bytes.Equal(a, b) {
			t.Fatalf("non-deterministic Describe for %q", sql)
		}
	})
}
