// Package corpus is the oracle ground-truth corpus harness
// (design 15 §7.2). A corpus case is a committed set of
// (schema, rendered SQL, Desc) triples captured from a real engine —
// stored byte-for-byte in the committed-cache format — and Replay
// runs an oracle backend over the case and reports every byte of
// disagreement. The native-inference backend ships only while replay
// is clean; the devdb capture test keeps the corpus itself truthful
// against the real engine.
package corpus

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/moznion/go-sqletch/internal/cache"
	"github.com/moznion/go-sqletch/internal/dialect"
)

// Manifest is corpus.json: the case's fingerprint inputs, spelled
// exactly as a sqletch.yaml-driven run would spell them (the schema
// paths enter the fingerprint verbatim).
type Manifest struct {
	Dialect       string   `json:"dialect"`
	ServerVersion string   `json:"server_version"`
	Schema        []string `json:"schema"` // ordered, case-dir-relative
}

// Entry is one committed oracle entry with its exact file bytes.
type Entry struct {
	Path  string // case-dir-relative, e.g. cache/oracle/<qh>.json
	Bytes []byte
	E     *cache.OracleEntry
}

// Case is one loaded corpus case. Load validates it the way the
// pipeline would: fingerprint recomputed from the schema inputs,
// every file store-and-compared, every byte canonical.
type Case struct {
	Name          string
	Dir           string
	Dialect       string
	ServerVersion string
	Schema        []cache.SchemaFile
	FP            string
	CatalogPath   string // case-dir-relative
	CatalogBytes  []byte
	Catalog       *cache.Catalog
	Entries       []Entry // sorted by Path
}

// Backend constructs the oracle under test for a case. The returned
// cleanup may be nil.
type Backend func(ctx context.Context, c *Case) (dialect.Oracle, func(), error)

// MismatchKind classifies a replay disagreement.
type MismatchKind string

const (
	// MismatchError: the backend refused an input the corpus records
	// as accepted. For a native backend this is the tolerable
	// direction only when the refusal is an intentional subset
	// exclusion (design 15 §7).
	MismatchError MismatchKind = "error"
	// MismatchDiff: the backend answered, but not byte-identically.
	// Never tolerable.
	MismatchDiff MismatchKind = "diff"
)

// Mismatch is one replay disagreement.
type Mismatch struct {
	Path   string // case-dir-relative file the disagreement is about
	Kind   MismatchKind
	Detail string
}

func (m Mismatch) String() string {
	return fmt.Sprintf("%s: %s: %s", m.Path, m.Kind, m.Detail)
}

// Load reads and validates one corpus case directory.
func Load(dir string) (*Case, error) {
	c := &Case{Name: filepath.Base(dir), Dir: dir}

	mf, err := os.ReadFile(filepath.Join(dir, "corpus.json"))
	if err != nil {
		return nil, err
	}
	var m Manifest
	dec := json.NewDecoder(bytes.NewReader(mf))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&m); err != nil {
		return nil, fmt.Errorf("%s: corpus.json: %w", c.Name, err)
	}
	if m.Dialect == "" || m.ServerVersion == "" || len(m.Schema) == 0 {
		return nil, fmt.Errorf("%s: corpus.json: dialect, server_version, and schema are required", c.Name)
	}
	c.Dialect, c.ServerVersion = m.Dialect, m.ServerVersion

	for _, rel := range m.Schema {
		content, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(rel)))
		if err != nil {
			return nil, fmt.Errorf("%s: schema input: %w", c.Name, err)
		}
		c.Schema = append(c.Schema, cache.SchemaFile{Path: rel, Content: content})
	}
	c.FP = cache.Fingerprint(c.Dialect, c.ServerVersion, c.Schema)

	store := cache.NewStore(filepath.Join(dir, "cache"))
	cat, ok := store.LoadCatalog(c.FP)
	if !ok {
		return nil, fmt.Errorf("%s: no catalog for fingerprint %s (schema inputs or pinned version drifted from the captured cache)", c.Name, c.FP[:12])
	}
	c.Catalog = cat
	c.CatalogPath = filepath.ToSlash(filepath.Join("cache", cache.CatalogFileName(c.FP)))
	c.CatalogBytes, err = os.ReadFile(filepath.Join(dir, filepath.FromSlash(c.CatalogPath)))
	if err != nil {
		return nil, err
	}

	entryPaths, err := filepath.Glob(filepath.Join(dir, "cache", "oracle", "*.json"))
	if err != nil {
		return nil, err
	}
	sort.Strings(entryPaths)
	for _, p := range entryPaths {
		raw, err := os.ReadFile(p)
		if err != nil {
			return nil, err
		}
		var e cache.OracleEntry
		if err := json.Unmarshal(raw, &e); err != nil {
			return nil, fmt.Errorf("%s: %s: %w", c.Name, filepath.Base(p), err)
		}
		// Store-and-compare through the real loader: proves the
		// filename matches the entry's own keys and the keys match
		// this case's fingerprint.
		if _, ok := store.LoadOracle(c.FP, e.RenderedSQL); !ok {
			return nil, fmt.Errorf("%s: %s: entry does not load for this case's fingerprint (misnamed, stale, or foreign)", c.Name, filepath.Base(p))
		}
		canon, err := cache.EncodeOracle(&e)
		if err != nil {
			return nil, err
		}
		if !bytes.Equal(canon, raw) {
			return nil, fmt.Errorf("%s: %s: not in canonical form (hand-edited?)", c.Name, filepath.Base(p))
		}
		c.Entries = append(c.Entries, Entry{
			Path:  filepath.ToSlash(filepath.Join("cache", "oracle", filepath.Base(p))),
			Bytes: raw,
			E:     &e,
		})
	}
	return c, nil
}

// LoadAll loads every case directory (one per subdirectory holding a
// corpus.json) under root, in sorted name order.
func LoadAll(root string) ([]*Case, error) {
	dirents, err := os.ReadDir(root)
	if err != nil {
		return nil, err
	}
	var cases []*Case
	for _, d := range dirents {
		if !d.IsDir() {
			continue
		}
		dir := filepath.Join(root, d.Name())
		if _, err := os.Stat(filepath.Join(dir, "corpus.json")); err != nil {
			continue
		}
		c, err := Load(dir)
		if err != nil {
			return nil, err
		}
		cases = append(cases, c)
	}
	return cases, nil
}

// Replay runs the backend over the case and reports every
// disagreement with the committed ground truth, in corpus order
// (catalog first, then entries by path). The error return is
// environmental (backend construction, context cancellation);
// disagreements are data, not errors.
func Replay(ctx context.Context, c *Case, backend Backend) ([]Mismatch, error) {
	oracle, cleanup, err := backend(ctx, c)
	if err != nil {
		return nil, err
	}
	if cleanup != nil {
		defer cleanup()
	}

	var ms []Mismatch
	snap, err := oracle.Snapshot(ctx)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		ms = append(ms, Mismatch{Path: c.CatalogPath, Kind: MismatchError, Detail: err.Error()})
	} else {
		snap.SchemaFP = c.FP // backends answer for a schema, not a fingerprint; the pipeline stamps it too
		got, err := cache.EncodeCatalog(snap)
		if err != nil {
			return nil, err
		}
		if !bytes.Equal(got, c.CatalogBytes) {
			ms = append(ms, Mismatch{Path: c.CatalogPath, Kind: MismatchDiff, Detail: firstDiff(c.CatalogBytes, got)})
		}
	}

	for _, e := range c.Entries {
		desc, err := oracle.Describe(ctx, e.E.RenderedSQL)
		if err != nil {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			ms = append(ms, Mismatch{Path: e.Path, Kind: MismatchError, Detail: err.Error()})
			continue
		}
		got, err := cache.EncodeOracle(dialect.EntryFromDesc(c.FP, e.E.RenderedSQL, desc))
		if err != nil {
			return nil, err
		}
		if !bytes.Equal(got, e.Bytes) {
			ms = append(ms, Mismatch{Path: e.Path, Kind: MismatchDiff, Detail: firstDiff(e.Bytes, got)})
		}
	}
	return ms, nil
}

// firstDiff renders the first differing line of two canonical JSON
// documents, want-then-got, bounded for diagnostics.
func firstDiff(want, got []byte) string {
	wl := strings.Split(string(want), "\n")
	gl := strings.Split(string(got), "\n")
	n := max(len(wl), len(gl))
	for i := range n {
		var w, g string
		if i < len(wl) {
			w = wl[i]
		}
		if i < len(gl) {
			g = gl[i]
		}
		if w != g {
			return fmt.Sprintf("line %d: want %s, got %s", i+1, clip(w), clip(g))
		}
	}
	return "documents differ" // unreachable when want != got line-wise
}

func clip(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return "<missing>"
	}
	if len(s) > 120 {
		return s[:117] + "..."
	}
	return s
}
