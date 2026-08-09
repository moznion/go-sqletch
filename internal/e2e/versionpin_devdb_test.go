//go:build devdb

package e2e_test

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/moznion/go-sqletch/internal/cache"
	"github.com/moznion/go-sqletch/internal/devdb"
)

// The pin is a dotted prefix against a REAL PostgreSQL, whose reported
// version carries a build suffix on some images. PostgreSQL compared
// majors only until v0.5, so a minor pin was accepted and then ignored;
// this pins the fix against the server itself rather than a string
// fixture.
func TestVersionPinIsADottedPrefix(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	// Acquire once with a major pin (the common case, must keep working)
	// and learn what this server actually calls itself.
	var det devdb.Detected
	dsn, cleanup, err := devdb.AcquireDSN(ctx, devdb.Config{
		DSN:           os.Getenv("SQLETCH_TEST_DSN"),
		ServerVersion: "16",
		Detected:      &det,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()

	actual := cache.NumericVersionPrefix(det.ServerVersion)
	if !strings.HasPrefix(actual, "16.") {
		t.Fatalf("expected a 16.x server, got %q", det.ServerVersion)
	}

	// The server's own exact version is accepted...
	if _, _, err := devdb.Acquire(ctx, devdb.Config{DSN: dsn, ServerVersion: actual}); err != nil {
		t.Errorf("pinning the server's exact version must be accepted: %v", err)
	}

	// ...and any other minor is not. Before v0.5 this was accepted,
	// because only "16" was ever compared.
	wrong := "16.0"
	if actual == wrong {
		wrong = "16.1"
	}
	_, _, err = devdb.Acquire(ctx, devdb.Config{DSN: dsn, ServerVersion: wrong})
	var vme *devdb.VersionMismatchError
	if !errors.As(err, &vme) {
		t.Fatalf("pinning %q against a %s server must fail, got %v", wrong, actual, err)
	}
	if vme.Server != "PostgreSQL" || vme.Pinned != wrong {
		t.Errorf("unexpected mismatch error: %+v", vme)
	}
}
