package template

import (
	"strings"
	"testing"

	"github.com/moznion/go-sqletch/internal/diagnostics"
)

// `-- @nolint` names performance-lint codes the query suppresses
// (design 24 §4): one entry per code, in declaration order, each
// carrying the directive's span and the optional reason.
func TestScan_NoLint(t *testing.T) {
	src := "-- name: Q :many\n" +
		"-- @nolint SQLETCHL003\n" +
		"-- @nolint SQLETCHL001, SQLETCHL002 (expression index on lower(email))\n" +
		"SELECT id FROM users WHERE lower(email) = :email\n"
	f := scanClean(t, src)
	got := f.Queries[0].NoLints
	if len(got) != 3 {
		t.Fatalf("nolints = %+v", got)
	}
	want := []diagnostics.Code{"SQLETCHL003", "SQLETCHL001", "SQLETCHL002"}
	for i, c := range want {
		if got[i].Code != c {
			t.Errorf("nolint[%d] = %s, want %s", i, got[i].Code, c)
		}
	}
	if got[0].Reason != "" || got[1].Reason != "expression index on lower(email)" || got[2].Reason != got[1].Reason {
		t.Errorf("reasons = %q %q %q", got[0].Reason, got[1].Reason, got[2].Reason)
	}
	line := "-- @nolint SQLETCHL003"
	if s := got[0].Span; src[s.Start:s.End] != line {
		t.Errorf("span covers %q, want %q", src[s.Start:s.End], line)
	}
	if got[1].Span != got[2].Span {
		t.Errorf("codes of one directive must share its span: %+v vs %+v", got[1].Span, got[2].Span)
	}
}

// The directive follows the shared attachment discipline: in the gap
// before the next header it belongs to the FOLLOWING query.
func TestScan_NoLintAttachesToFollowingQuery(t *testing.T) {
	src := "-- name: A :many\nSELECT id FROM users LIMIT 1;\n" +
		"-- @nolint SQLETCHL003\n" +
		"-- name: B :many\nSELECT id FROM users;\n"
	f := scanClean(t, src)
	if len(f.Queries[0].NoLints) != 0 || len(f.Queries[1].NoLints) != 1 {
		t.Errorf("A=%+v B=%+v", f.Queries[0].NoLints, f.Queries[1].NoLints)
	}
}

// @nolint may name ONLY performance-lint codes: suppressing a soundness
// diagnostic is not a per-query decision (owner decision 2026-10-02),
// and an unknown or malformed code would be a suppression that
// silently suppresses nothing.
func TestScan_NoLintRejected(t *testing.T) {
	cases := map[string]string{
		"soundness code":       "-- @nolint SQLETCH115\n",
		"unknown code":         "-- @nolint SQLETCH999\n",
		"unused-nolint itself": "-- @nolint SQLETCHL006\n",
		"no code":              "-- @nolint\n",
		"lowercase":            "-- @nolint sqletch128\n",
		"colon form":           "-- @nolint: SQLETCHL001\n",
		"trailing junk":        "-- @nolint SQLETCHL001 because\n",
		"empty reason":         "-- @nolint SQLETCHL001 ()\n",
		"dangling comma":       "-- @nolint SQLETCHL001,\n",
		"one bad among good":   "-- @nolint SQLETCHL001, SQLETCH101\n",
		"short code":           "-- @nolint SQLETCH12\n",
		"glued second keyword": "-- @nolintSQLETCH128\n",
	}
	for name, dir := range cases {
		t.Run(name, func(t *testing.T) {
			src := "-- name: Q :many\n" + dir + "SELECT id FROM users\n"
			f, diags := scan(t, src)
			if name == "glued second keyword" {
				// `@nolintX` is not the directive at all: it stays an
				// ordinary comment, exactly like `@paramx` would.
				if len(diags) != 0 || len(f.Queries[0].NoLints) != 0 {
					t.Errorf("diags=%+v nolints=%+v", diags, f.Queries[0].NoLints)
				}
				return
			}
			if !hasCode(diags, diagnostics.CodeBadNoLint) {
				t.Fatalf("no SQLETCH016: %+v", diags)
			}
			for _, d := range diags {
				if d.Code == diagnostics.CodeBadNoLint && d.Severity != diagnostics.Error {
					t.Errorf("SQLETCH016 must be an error: %+v", d)
				}
			}
			if len(f.Queries) == 1 && len(f.Queries[0].NoLints) != 0 {
				t.Errorf("rejected directive recorded: %+v", f.Queries[0].NoLints)
			}
		})
	}
}

// golangci-lint habits fail loudly with the sqletch spelling in the
// message: a bare `@nolint` (all lints there) must name its codes here,
// and `@nolint:CODE` is spelled with a space.
func TestScan_NoLintGolangciHabits(t *testing.T) {
	cases := map[string]string{
		"bare":             "-- @nolint\n",
		"bare with reason": "-- @nolint (generated report)\n",
		"glued reason":     "-- @nolint(generated report)\n",
		"glued code":       "-- @nolint(SQLETCHL003)\n",
		"colon":            "-- @nolint:SQLETCHL003\n",
		"colon list":       "-- @nolint:SQLETCHL001,SQLETCHL003 // reason\n",
	}
	for name, dir := range cases {
		t.Run(name, func(t *testing.T) {
			_, diags := scan(t, "-- name: Q :many\n"+dir+"SELECT id FROM users\n")
			var msg string
			for _, d := range diags {
				if d.Code == diagnostics.CodeBadNoLint {
					msg = d.Message
				}
			}
			if msg == "" {
				t.Fatalf("no SQLETCH016: %+v", diags)
			}
			if !strings.Contains(msg, "-- @nolint SQLETCH") {
				t.Errorf("message must spell the sqletch form: %q", msg)
			}
			if strings.HasPrefix(name, "bare") && !strings.Contains(msg, "name the codes") {
				t.Errorf("bare @nolint must say codes are required: %q", msg)
			}
		})
	}
}

// The rejection names what IS allowed, so the fix is in the message.
func TestScan_NoLintRejectedNamesTheVocabulary(t *testing.T) {
	_, diags := scan(t, "-- name: Q :many\n-- @nolint SQLETCH115\nSELECT id FROM users\n")
	for _, d := range diags {
		if d.Code != diagnostics.CodeBadNoLint {
			continue
		}
		if !strings.Contains(d.Message, "SQLETCH115") || !strings.Contains(d.Hint, "SQLETCHL001") {
			t.Errorf("message/hint not actionable: %q / %q", d.Message, d.Hint)
		}
		return
	}
	t.Fatal("no SQLETCH016")
}

// Like every directive, the comment stays in the skeleton verbatim.
func TestScan_NoLintStaysInSkeleton(t *testing.T) {
	f := scanClean(t, "-- name: Q :many\n-- @nolint SQLETCHL003\nSELECT id FROM users\n")
	var text string
	for _, it := range f.Queries[0].Items {
		if s, ok := it.(*Skeleton); ok {
			text += s.Text
		}
	}
	if !strings.Contains(text, "-- @nolint SQLETCHL003") {
		t.Errorf("directive excised:\n%s", text)
	}
}
