package template

import (
	"strings"
	"testing"
	"time"

	"github.com/moznion/go-sqletch/internal/diagnostics"
)

func TestScan_Timeout(t *testing.T) {
	cases := map[string]time.Duration{
		"500ms":  500 * time.Millisecond,
		"2s":     2 * time.Second,
		"1m30s":  90 * time.Second,
		"1.5s":   1500 * time.Millisecond,
		"250us":  250 * time.Microsecond,
		"1h":     time.Hour,
		"10ns":   10 * time.Nanosecond,
		"  3s  ": 3 * time.Second,
	}
	for arg, want := range cases {
		src := "-- name: Q :many\n-- @timeout " + arg + "\nSELECT id FROM orders\n"
		q := scanClean(t, src).Queries[0]
		if q.Timeout == nil {
			t.Fatalf("%q: no timeout recorded", arg)
		}
		if q.Timeout.None || q.Timeout.Duration != want {
			t.Errorf("%q: got %+v, want %v", arg, *q.Timeout, want)
		}
		if q.Timeout.Span.End <= q.Timeout.Span.Start {
			t.Errorf("%q: span not recorded: %+v", arg, q.Timeout.Span)
		}
	}
}

// `none` opts the query out of the config-level default timeout
// (query_timeout.default); it is distinct from an absent directive.
func TestScan_TimeoutNone(t *testing.T) {
	q := scanClean(t, "-- name: Q :many\n-- @timeout none\nSELECT id FROM orders\n").Queries[0]
	if q.Timeout == nil || !q.Timeout.None || q.Timeout.Duration != 0 {
		t.Fatalf("got %+v", q.Timeout)
	}
	q = scanClean(t, "-- name: Q :many\nSELECT id FROM orders\n").Queries[0]
	if q.Timeout != nil {
		t.Fatalf("absent directive recorded: %+v", q.Timeout)
	}
}

// Every malformed form is SQLETCH017 and records nothing: a typo that
// silently fell back to "no timeout" (or to the config default) would
// leave a query unbounded that its author believes is bounded.
func TestScan_TimeoutMalformed(t *testing.T) {
	cases := []string{
		"-- @timeout",
		"-- @timeout ",
		"-- @timeout 500",        // unitless
		"-- @timeout 0s",         // non-positive
		"-- @timeout 0",          // non-positive
		"-- @timeout -1s",        // negative
		"-- @timeout 1 s",        // two words
		"-- @timeout: 500ms",     // colon form belongs to other directives
		"-- @timeout 500ms 1s",   // trailing words
		"-- @timeout NONE",       // keyword is lowercase only
		"-- @timeout 9999999h",   // overflows time.Duration
		"-- @timeout fast",       // not a duration
		"-- @timeout 500ms -- x", // trailing comment text
	}
	for _, line := range cases {
		src := "-- name: Q :many\n" + line + "\nSELECT id FROM orders\n"
		f, diags := scan(t, src)
		if !hasCode(diags, diagnostics.CodeBadTimeout) {
			t.Errorf("no SQLETCH017 for %q: %+v", line, diags)
		}
		if len(f.Queries) == 1 && f.Queries[0].Timeout != nil {
			t.Errorf("malformed timeout recorded for %q: %+v", line, f.Queries[0].Timeout)
		}
	}
}

// A second @timeout on one query is SQLETCH017 at the second
// directive: which one wins would otherwise depend on source order,
// and a reviewer reading either line would be misled.
func TestScan_TimeoutDuplicate(t *testing.T) {
	src := "-- name: Q :many\n-- @timeout 1s\n-- @timeout 2s\nSELECT id FROM orders\n"
	f, diags := scan(t, src)
	var found bool
	for _, d := range diags {
		if d.Code == diagnostics.CodeBadTimeout {
			found = true
			if got := src[d.Span.Start:d.Span.End]; got != "-- @timeout 2s" {
				t.Errorf("duplicate diagnostic points at %q", got)
			}
		}
	}
	if !found {
		t.Fatalf("no SQLETCH017: %+v", diags)
	}
	if q := f.Queries[0]; q.Timeout == nil || q.Timeout.Duration != time.Second {
		t.Errorf("first directive should stand: %+v", q.Timeout)
	}
	// `none` plus a duration is the same contradiction.
	_, diags = scan(t, "-- name: Q :many\n-- @timeout none\n-- @timeout 2s\nSELECT 1\n")
	if !hasCode(diags, diagnostics.CodeBadTimeout) {
		t.Errorf("none + duration not rejected: %+v", diags)
	}
}

// Like every directive, @timeout in the gap before a header annotates
// the FOLLOWING query.
func TestScan_TimeoutGapAttachesToFollowingQuery(t *testing.T) {
	src := "-- name: A :many\n" +
		"SELECT id FROM orders\n" +
		"-- @timeout 1s\n" +
		"-- name: B :many\n" +
		"SELECT id FROM orders\n"
	f := scanClean(t, src)
	if f.Queries[0].Timeout != nil {
		t.Errorf("gap timeout leaked onto A: %+v", f.Queries[0].Timeout)
	}
	if f.Queries[1].Timeout == nil || f.Queries[1].Timeout.Duration != time.Second {
		t.Errorf("gap timeout did not attach to B: %+v", f.Queries[1].Timeout)
	}
}

// The comment stays in the skeleton verbatim, like every directive —
// so editing a query's timeout re-keys its oracle entries (design 23).
func TestScan_TimeoutStaysInSkeleton(t *testing.T) {
	f := scanClean(t, "-- name: Q :many\n-- @timeout 500ms\nSELECT id FROM orders\n")
	var text string
	for _, it := range f.Queries[0].Items {
		if s, ok := it.(*Skeleton); ok {
			text += s.Text
		}
	}
	if !strings.Contains(text, "-- @timeout 500ms") {
		t.Errorf("directive excised from skeleton:\n%s", text)
	}
}

// `-- @timeouts` and similar are not the directive (word boundary):
// they stay ordinary comments rather than tripping SQLETCH017.
func TestScan_TimeoutWordBoundary(t *testing.T) {
	q := scanClean(t, "-- name: Q :many\n-- @timeouts are handled elsewhere\nSELECT 1\n").Queries[0]
	if q.Timeout != nil {
		t.Errorf("non-directive comment recorded: %+v", q.Timeout)
	}
}
