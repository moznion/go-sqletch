package sqlite

import (
	"strings"
	"testing"

	"github.com/moznion/go-sqletch/internal/dialect"
)

// byteIndex returns the byte offset of the first occurrence of sub in s,
// failing the test if it is absent or ambiguous for the intended anchor.
func byteIndex(t *testing.T, s, sub string) int {
	t.Helper()
	i := strings.Index(s, sub)
	if i < 0 {
		t.Fatalf("substring %q not found in %q", sub, s)
	}
	return i
}

func parseTree(t *testing.T, sql string) dialect.Tree {
	t.Helper()
	tr, err := Frontend{}.Parse(sql)
	if err != nil {
		t.Fatalf("parse %q: %v", sql, err)
	}
	return tr
}

// TestOffsetsAreBytesMultibyte pins the RelRef/ColRef/conjunct Loc
// contract ("byte offset in the parsed SQL") for SQL that contains
// multibyte runes before the anchored positions. rqlite/sql reports
// positions as rune counts; the facade must translate them to bytes,
// otherwise guarded-fragment position checks in nullability/rules/policy
// silently misfire (a left-shifted Loc reads as skeleton text and
// narrows a nullable column — a silent soundness hole).
func TestOffsetsAreBytesMultibyte(t *testing.T) {
	// 'あいう' is 3 runes / 9 bytes: every position after it shifts by 6.
	sql := "SELECT 'あいう' AS label, u.email FROM users u WHERE u.email IS NOT NULL"

	rels := parseTree(t, sql).Relations()
	if len(rels) != 1 || rels[0].Table != "users" {
		t.Fatalf("Relations() = %+v, want single 'users'", rels)
	}
	if want := byteIndex(t, sql, "users"); rels[0].Loc != want {
		t.Errorf("Relations()[0].Loc = %d, want byte offset %d", rels[0].Loc, want)
	}

	// The qualified ref u.email inside WHERE — anchored on the second
	// occurrence (the first is in the SELECT list).
	whereEmail := strings.LastIndex(sql, "u.email")
	var found bool
	for _, cr := range parseTree(t, sql).ColumnRefs() {
		if cr.Loc == whereEmail {
			found = true
		}
		if cr.Loc < 0 || cr.Loc > len(sql) {
			t.Errorf("ColRef.Loc = %d out of byte range [0,%d]", cr.Loc, len(sql))
		}
	}
	if !found {
		var got []int
		for _, cr := range parseTree(t, sql).ColumnRefs() {
			got = append(got, cr.Loc)
		}
		t.Errorf("no ColRef at WHERE u.email byte offset %d; got Locs %v", whereEmail, got)
	}

	nn := parseTree(t, sql).NotNullConjuncts()
	if len(nn) != 1 {
		t.Fatalf("NotNullConjuncts() = %+v, want one", nn)
	}
	if want := whereEmail; nn[0].Loc != want {
		t.Errorf("NotNullConjuncts()[0].Loc = %d, want byte offset %d", nn[0].Loc, want)
	}
}

// TestOffsetsByteAscii is the no-regression case: with only ASCII input,
// byte and rune offsets coincide, so translation must be a no-op.
func TestOffsetsByteAscii(t *testing.T) {
	sql := "SELECT u.email FROM users u WHERE u.email IS NOT NULL"
	rels := parseTree(t, sql).Relations()
	if len(rels) != 1 {
		t.Fatalf("Relations() = %+v", rels)
	}
	if want := byteIndex(t, sql, "users"); rels[0].Loc != want {
		t.Errorf("Relations()[0].Loc = %d, want %d", rels[0].Loc, want)
	}
	nn := parseTree(t, sql).NotNullConjuncts()
	if len(nn) != 1 || nn[0].Loc != strings.LastIndex(sql, "u.email") {
		t.Errorf("NotNullConjuncts() = %+v, want conjunct at %d", nn, strings.LastIndex(sql, "u.email"))
	}
}

// TestOffsetsMultibyteConjunctAndOrderBy exercises TopConjunctLocs and
// OrderByLocs, whose []int results are also byte offsets.
func TestOffsetsMultibyteConjunctAndOrderBy(t *testing.T) {
	sql := "SELECT id FROM t WHERE note = 'αβγδε' AND id > 0 ORDER BY id"
	tr := parseTree(t, sql)

	locs := tr.TopConjunctLocs()
	if len(locs) != 2 {
		t.Fatalf("TopConjunctLocs() = %v, want 2", locs)
	}
	// The second conjunct `id > 0` sits after the multibyte literal.
	wantSecond := byteIndex(t, sql, "id > 0")
	if locs[1] != wantSecond {
		t.Errorf("TopConjunctLocs()[1] = %d, want byte offset %d", locs[1], wantSecond)
	}

	ob := tr.OrderByLocs()
	if len(ob) != 1 {
		t.Fatalf("OrderByLocs() = %v, want 1", ob)
	}
	if want := strings.LastIndex(sql, "id"); ob[0] != want {
		t.Errorf("OrderByLocs()[0] = %d, want byte offset %d", ob[0], want)
	}
}

// TestParseErrorPosIsByte pins that a parse error's position is a byte
// offset even when multibyte runes precede the offending token.
func TestParseErrorPosIsByte(t *testing.T) {
	// A stray token after a multibyte string literal triggers a parse
	// error; its position must land in byte space.
	sql := "SELECT 'あいうえお' FROM t WHERE )"
	_, err := Frontend{}.Parse(sql)
	if err == nil {
		t.Fatalf("expected parse error for %q", sql)
	}
	pe, ok := err.(*dialect.ParseError)
	if !ok {
		t.Fatalf("err = %T, want *dialect.ParseError", err)
	}
	if pe.Pos < 0 || pe.Pos > len(sql) {
		t.Errorf("ParseError.Pos = %d out of byte range [0,%d]", pe.Pos, len(sql))
	}
}
