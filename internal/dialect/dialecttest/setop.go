// Package dialecttest holds cross-dialect conformance helpers shared by
// the dialect packages' tests.
package dialecttest

import (
	"slices"
	"strings"
	"testing"

	"github.com/moznion/go-sqletch/internal/dialect"
)

// SetOpCase describes one statement's expected set-operation branches:
// per branch, its own FROM relations ("table" or "table alias") and its
// sorted DeepTables names. A nil Branches means "not a set operation".
type SetOpCase struct {
	SQL      string
	Branches [][]string
	Deep     [][]string
}

// CheckSetOpBranches asserts Tree.SetOpBranches against the cases:
// branch count and order, each branch's Kind, Relations (with every
// Loc pointing at the relation's name in the original SQL), and
// DeepTables (design 14 §13).
func CheckSetOpBranches(t *testing.T, fe dialect.Frontend, cases []SetOpCase) {
	t.Helper()
	for _, tc := range cases {
		tree, err := fe.Parse(tc.SQL)
		if err != nil {
			t.Fatalf("parse %q: %v", tc.SQL, err)
		}
		bs := tree.SetOpBranches()
		if tc.Branches == nil {
			if bs != nil {
				t.Errorf("%q: SetOpBranches = %d branches, want nil (not a set operation)", tc.SQL, len(bs))
			}
			continue
		}
		if len(bs) != len(tc.Branches) {
			t.Errorf("%q: %d branches, want %d", tc.SQL, len(bs), len(tc.Branches))
			continue
		}
		for i, b := range bs {
			if b.Kind() != dialect.StmtSelect {
				t.Errorf("%q branch %d: Kind = %v, want StmtSelect", tc.SQL, i, b.Kind())
			}
			got := []string{}
			for _, r := range b.Relations() {
				s := r.Table
				if r.Alias != "" {
					s += " " + r.Alias
				}
				got = append(got, s)
				if r.Table != "" && (r.Loc < 0 || r.Loc > len(tc.SQL) || !strings.HasPrefix(strings.ToLower(tc.SQL[r.Loc:]), strings.ToLower(r.Table))) {
					t.Errorf("%q branch %d: relation %q Loc=%d does not point at its name", tc.SQL, i, r.Table, r.Loc)
				}
			}
			if !slices.Equal(got, tc.Branches[i]) {
				t.Errorf("%q branch %d: Relations = %v, want %v", tc.SQL, i, got, tc.Branches[i])
			}
			var deep []string
			for _, d := range b.DeepTables() {
				deep = append(deep, d.Name)
			}
			slices.Sort(deep)
			if !slices.Equal(deep, tc.Deep[i]) {
				t.Errorf("%q branch %d: DeepTables = %v, want %v", tc.SQL, i, deep, tc.Deep[i])
			}
		}
	}
}
