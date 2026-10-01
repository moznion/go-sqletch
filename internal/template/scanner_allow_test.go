package template

import (
	"strings"
	"testing"

	"github.com/moznion/go-sqletch/internal/diagnostics"
)

// `-- @allow` names performance-lint codes the query suppresses
// (design 24 §4): one entry per code, in declaration order, each
// carrying the directive's span and the optional reason.
func TestScan_Allow(t *testing.T) {
	src := "-- name: Q :many\n" +
		"-- @allow SQLETCH130\n" +
		"-- @allow SQLETCH128, SQLETCH129 (expression index on lower(email))\n" +
		"SELECT id FROM users WHERE lower(email) = :email\n"
	f := scanClean(t, src)
	got := f.Queries[0].Allows
	if len(got) != 3 {
		t.Fatalf("allows = %+v", got)
	}
	want := []diagnostics.Code{"SQLETCH130", "SQLETCH128", "SQLETCH129"}
	for i, c := range want {
		if got[i].Code != c {
			t.Errorf("allow[%d] = %s, want %s", i, got[i].Code, c)
		}
	}
	if got[0].Reason != "" || got[1].Reason != "expression index on lower(email)" || got[2].Reason != got[1].Reason {
		t.Errorf("reasons = %q %q %q", got[0].Reason, got[1].Reason, got[2].Reason)
	}
	line := "-- @allow SQLETCH130"
	if s := got[0].Span; src[s.Start:s.End] != line {
		t.Errorf("span covers %q, want %q", src[s.Start:s.End], line)
	}
	if got[1].Span != got[2].Span {
		t.Errorf("codes of one directive must share its span: %+v vs %+v", got[1].Span, got[2].Span)
	}
}

// The directive follows the shared attachment discipline: in the gap
// before the next header it belongs to the FOLLOWING query.
func TestScan_AllowAttachesToFollowingQuery(t *testing.T) {
	src := "-- name: A :many\nSELECT id FROM users LIMIT 1;\n" +
		"-- @allow SQLETCH130\n" +
		"-- name: B :many\nSELECT id FROM users;\n"
	f := scanClean(t, src)
	if len(f.Queries[0].Allows) != 0 || len(f.Queries[1].Allows) != 1 {
		t.Errorf("A=%+v B=%+v", f.Queries[0].Allows, f.Queries[1].Allows)
	}
}

// @allow may name ONLY performance-lint codes: suppressing a soundness
// diagnostic is not a per-query decision (owner decision 2026-10-02),
// and an unknown or malformed code would be a suppression that
// silently suppresses nothing.
func TestScan_AllowRejected(t *testing.T) {
	cases := map[string]string{
		"soundness code":       "-- @allow SQLETCH115\n",
		"unknown code":         "-- @allow SQLETCH999\n",
		"unused-allow itself":  "-- @allow SQLETCH133\n",
		"no code":              "-- @allow\n",
		"lowercase":            "-- @allow sqletch128\n",
		"colon form":           "-- @allow: SQLETCH128\n",
		"trailing junk":        "-- @allow SQLETCH128 because\n",
		"empty reason":         "-- @allow SQLETCH128 ()\n",
		"dangling comma":       "-- @allow SQLETCH128,\n",
		"one bad among good":   "-- @allow SQLETCH128, SQLETCH101\n",
		"short code":           "-- @allow SQLETCH12\n",
		"glued second keyword": "-- @allowSQLETCH128\n",
	}
	for name, dir := range cases {
		t.Run(name, func(t *testing.T) {
			src := "-- name: Q :many\n" + dir + "SELECT id FROM users\n"
			f, diags := scan(t, src)
			if name == "glued second keyword" {
				// `@allowX` is not the directive at all: it stays an
				// ordinary comment, exactly like `@paramx` would.
				if len(diags) != 0 || len(f.Queries[0].Allows) != 0 {
					t.Errorf("diags=%+v allows=%+v", diags, f.Queries[0].Allows)
				}
				return
			}
			if !hasCode(diags, diagnostics.CodeBadAllow) {
				t.Fatalf("no SQLETCH016: %+v", diags)
			}
			for _, d := range diags {
				if d.Code == diagnostics.CodeBadAllow && d.Severity != diagnostics.Error {
					t.Errorf("SQLETCH016 must be an error: %+v", d)
				}
			}
			if len(f.Queries) == 1 && len(f.Queries[0].Allows) != 0 {
				t.Errorf("rejected directive recorded: %+v", f.Queries[0].Allows)
			}
		})
	}
}

// The rejection names what IS allowed, so the fix is in the message.
func TestScan_AllowRejectedNamesTheVocabulary(t *testing.T) {
	_, diags := scan(t, "-- name: Q :many\n-- @allow SQLETCH115\nSELECT id FROM users\n")
	for _, d := range diags {
		if d.Code != diagnostics.CodeBadAllow {
			continue
		}
		if !strings.Contains(d.Message, "SQLETCH115") || !strings.Contains(d.Hint, "SQLETCH128") {
			t.Errorf("message/hint not actionable: %q / %q", d.Message, d.Hint)
		}
		return
	}
	t.Fatal("no SQLETCH016")
}

// Like every directive, the comment stays in the skeleton verbatim.
func TestScan_AllowStaysInSkeleton(t *testing.T) {
	f := scanClean(t, "-- name: Q :many\n-- @allow SQLETCH130\nSELECT id FROM users\n")
	var text string
	for _, it := range f.Queries[0].Items {
		if s, ok := it.(*Skeleton); ok {
			text += s.Text
		}
	}
	if !strings.Contains(text, "-- @allow SQLETCH130") {
		t.Errorf("directive excised:\n%s", text)
	}
}
