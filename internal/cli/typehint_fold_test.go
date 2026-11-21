package cli

import (
	"context"
	"strings"
	"testing"

	"github.com/moznion/go-sqletch/internal/diagnostics"
)

// A `-- @param` type name folds ASCII-only, like MySQL's own keyword
// lookup (`bİgint` is ERROR 1064 on MySQL 8.4): it is an unknown type,
// SQLETCH311, never a silent BIGINT via strings.ToLower's `İ`→`i`.
func TestRun_NonASCIITypeHintIsUnknownType(t *testing.T) {
	dir := t.TempDir()
	query := strings.Replace(nativeCLIQuery, "-- @param limit: bigint", "-- @param limit: bİgint", 1)
	cfg := writeNativeMySQLProject(t, dir, nativeCLISchema, query)
	res, err := Run(context.Background(), cfg, ModeCheck, RunOptions{})
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range res.Diags {
		if d.Code == diagnostics.CodeUnsupportedType && strings.Contains(d.Message, "bİgint") {
			return
		}
	}
	t.Fatalf("want %s for the non-ASCII type name, got %+v", diagnostics.CodeUnsupportedType, res.Diags)
}
