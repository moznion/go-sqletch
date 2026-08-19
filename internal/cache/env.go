package cache

import (
	"encoding/json"
	"fmt"
	"path/filepath"
)

// Env is the sidecar record of the environment a committed cache was
// generated in.
//
// It is deliberately NOT a cache key. The schema fingerprint must stay
// offline-computable (spec requirement), so the version of a server we
// have not contacted cannot enter it; and the bytes of catalog and
// oracle entries are pinned byte-identical across oracle backends by
// internal/corpus, so nothing backend- or connection-specific may
// enter those files either. The sidecar therefore lives beside them,
// keyed by the same fingerprint, and is read only by runs that contact
// a server anyway — where it answers one question the cache otherwise
// cannot: "was what I am connected to the same thing that produced
// these entries?" (docs/design/04-type-oracle.md §3.1).
//
// Only facts that are semantically part of the oracle's answer belong
// here. Host names, user names, and timestamps do not: they would
// churn the committed diff on every developer's machine and break the
// project's determinism invariant.
type Env struct {
	Format   int    `json:"format"`
	SchemaFP string `json:"schema_fp"`
	// Dialect and OracleBackend are recorded for forensics only. The
	// dialect is already fingerprint input, and the backend is
	// deliberately NOT compared: server and native backends are
	// required to produce byte-identical output, so a backend
	// difference is either a no-op or a sqletch bug for the corpus
	// gates to catch — never a reason to fail a user's build.
	Dialect       string `json:"dialect"`
	OracleBackend string `json:"oracle_backend"`
	// ServerVersion is the compared value: the leading dotted-numeric
	// run of what the server reported (see NumericVersionPrefix).
	ServerVersion string `json:"server_version"`
	// ServerVersionRaw is the full reported string, kept so the
	// diagnostic can name the actual builds involved.
	ServerVersionRaw string `json:"server_version_raw"`
}

// NumericVersionPrefix reduces a server-reported version string to the
// value drift detection compares: its leading dotted-numeric run.
//
// Servers spell the same version differently depending on how they
// were built — PostgreSQL reports "16.4 (Debian 16.4-1.pgdg120+1)" on
// the Debian images and a bare "16.4" on Alpine, MySQL appends "-log".
// Comparing raw strings would report a base-image change as an
// environment drift, which is noise, not signal.
func NumericVersionPrefix(raw string) string {
	end := 0
	for end < len(raw) && (raw[end] == '.' || (raw[end] >= '0' && raw[end] <= '9')) {
		end++
	}
	for end > 0 && raw[end-1] == '.' {
		end--
	}
	return raw[:end]
}

// EnvFileName exposes the sidecar's dir-relative naming, mirroring
// CatalogFileName.
func EnvFileName(fp string) string {
	return "env-" + fp[:min(24, len(fp))] + ".json"
}

func (s *Store) envPath(fp string) string {
	return filepath.Join(s.dir, EnvFileName(fp))
}

// LoadEnv returns the recorded generation environment for fp.
//
// Every way of not having a usable record — absent file, unreadable
// file, format drift, key mismatch — returns ok=false, which callers
// must treat as "no record yet", never as a failure. Caches committed
// before this sidecar existed have no record, and adopting the
// connected server on the next write is the correct migration.
func (s *Store) LoadEnv(fp string) (*Env, bool) {
	data, err := ReadFileCapped(s.envPath(fp))
	if err != nil {
		return nil, false
	}
	var e Env
	if err := json.Unmarshal(data, &e); err != nil || e.Format != FormatVersion || e.SchemaFP != fp {
		return nil, false
	}
	return &e, true
}

func (s *Store) SaveEnv(e *Env) error {
	data, err := EncodeEnv(e)
	if err != nil {
		return err
	}
	return s.writeFile(s.envPath(e.SchemaFP), data)
}

// EncodeEnv returns the exact canonical bytes SaveEnv writes, stamping
// FormatVersion.
func EncodeEnv(e *Env) ([]byte, error) {
	if e.SchemaFP == "" {
		return nil, fmt.Errorf("env record has no schema fingerprint")
	}
	e.Format = FormatVersion
	return marshalCanonical(e)
}
