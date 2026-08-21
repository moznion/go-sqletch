package mysql

import (
	"math"
	"strings"
	"testing"
)

// TestSnapshotAttRange pins the hostile-server hardening on the
// catalog snapshot: information_schema ordinal_position is 1-based, and
// the catalog stores it as an int16 attribute number. A corrupt or
// hostile server returning a value outside 1..32767 used to wrap
// silently in the int16 conversion (silent catalog corruption); it must
// be an explicit error naming the table, the column, and the bad value.
func TestSnapshotAttRange(t *testing.T) {
	for _, tt := range []struct {
		name string
		ord  int64
		want int16
		ok   bool
	}{
		{"first ordinal", 1, 1, true},
		{"max int16", math.MaxInt16, math.MaxInt16, true},
		{"zero", 0, 0, false},
		{"negative", -1, 0, false},
		{"min int16 wrap source", math.MinInt16, 0, false},
		{"one past max", math.MaxInt16 + 1, 0, false},
		{"int16 wraparound to 1", 65537, 0, false},
		{"max int64", math.MaxInt64, 0, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			att, err := snapshotAtt("users", "email", tt.ord)
			if tt.ok {
				if err != nil {
					t.Fatalf("ordinal %d must be accepted: %v", tt.ord, err)
				}
				if att != tt.want {
					t.Fatalf("ordinal %d: got att %d, want %d", tt.ord, att, tt.want)
				}
				return
			}
			if err == nil {
				t.Fatalf("ordinal %d must be refused, got att %d", tt.ord, att)
			}
			for _, frag := range []string{"users", "email", "ordinal_position"} {
				if !strings.Contains(err.Error(), frag) {
					t.Errorf("error %q should mention %q", err, frag)
				}
			}
		})
	}
}
