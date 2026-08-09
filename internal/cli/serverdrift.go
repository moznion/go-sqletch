package cli

import (
	"errors"
	"fmt"

	"github.com/moznion/go-sqletch/internal/cache"
	"github.com/moznion/go-sqletch/internal/config"
	"github.com/moznion/go-sqletch/internal/diagnostics"
)

// errServerDrift is the acquire path's internal signal that it already
// appended a SQLETCH203 error and the run must stop. It is a
// diagnostic, not an environment failure, so it must never surface as
// a bare error to the user — every acquireOracle caller converts it
// back into a plain diagnostics return.
var errServerDrift = errors.New("committed cache generated against a different server")

// envRecord builds the sidecar record for a run that just connected.
// actualRaw is the server's own version string, empty when the backend
// never contacted one (the native oracle) — the record is still worth
// writing, because it says which backend produced the entries.
func envRecord(cfg config.Config, fp, actualRaw string) *cache.Env {
	backend := config.OracleServer
	if cfg.NativeOracle() {
		backend = config.OracleNative
	}
	return &cache.Env{
		SchemaFP:         fp,
		Dialect:          cfg.Dialect,
		OracleBackend:    backend,
		ServerVersion:    cache.NumericVersionPrefix(actualRaw),
		ServerVersionRaw: actualRaw,
	}
}

// serverDriftDiag compares the environment recorded beside a committed
// cache with the server this run just connected to (SQLETCH203).
//
// Without --allow-server-drift a disagreement is an error: the entries
// already in the tree were typed by a different server, and filling the
// remaining misses from this one would commit a cache that no single
// environment ever produced. With the flag it is a warning and the
// caller adopts the connected server.
//
// Anything short of a confirmed disagreement is silence. No record yet
// (a cache committed before the sidecar existed, or the first generate)
// means adopt. An empty version on either side means a backend that
// never contacted a server, and the backend itself is deliberately not
// compared: server and native are required to produce byte-identical
// output, so a backend difference is either a no-op or a sqletch bug
// for the corpus gates to catch — never a reason to fail a build.
func serverDriftDiag(cfg config.Config, recorded *cache.Env, actualRaw string, allow bool) (diagnostics.Diagnostic, bool) {
	if recorded == nil {
		return diagnostics.Diagnostic{}, false
	}
	actual := cache.NumericVersionPrefix(actualRaw)
	if recorded.ServerVersion == "" || actual == "" || recorded.ServerVersion == actual {
		return diagnostics.Diagnostic{}, false
	}

	span := diagnostics.Span{File: cfg.Path}
	msg := "the committed oracle cache was generated against server version %s but this run connected to %s"
	if allow {
		d := diagnostics.Warnf(diagnostics.CodeCacheServerDrift, span, msg,
			describeVersion(recorded.ServerVersion, recorded.ServerVersionRaw),
			describeVersion(actual, actualRaw))
		d.Hint = fmt.Sprintf("--allow-server-drift: recording %s; entries typed by %s are kept as they are",
			actual, recorded.ServerVersion)
		return d, true
	}
	d := diagnostics.Errorf(diagnostics.CodeCacheServerDrift, span, msg,
		describeVersion(recorded.ServerVersion, recorded.ServerVersionRaw),
		describeVersion(actual, actualRaw))
	d.Hint = fmt.Sprintf("regenerate the whole cache against one server (delete %s and re-run), or pass --allow-server-drift to accept a cache built from both",
		cfg.Cache.Path)
	return d, true
}

// describeVersion spells a version as the compared value, naming the
// full reported string too when the two differ — the build suffix is
// often the only thing that identifies which server is which.
func describeVersion(compared, raw string) string {
	if raw == "" || raw == compared {
		return compared
	}
	return fmt.Sprintf("%s (%s)", compared, raw)
}
