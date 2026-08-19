package cache

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

// MaxFileBytes bounds every cache file sqletch reads. Cache file names
// are fingerprint-derived, hence attacker-computable: a cloned repo can
// plant a file at the exact hit path, so an unbounded os.ReadFile would
// OOM before any key check runs. 64 MiB dwarfs any real catalog/oracle
// entry while capping the blast radius (mirrors the LSP body cap).
const MaxFileBytes = 64 << 20

// FormatVersion is the on-disk cache format. Every written file
// carries it; loads treat any other value — including its absence in
// pre-1.0 caches — as a miss, so a format change can never misread an
// old entry: the pipeline falls back to the database and rewrites.
const FormatVersion = 1

// Store is the committed, offline-usable cache of oracle results and
// catalog snapshots. Hashes are an index, never identity: every entry
// stores its full inputs and loads compare them byte-wise
// (store-and-compare; design 04 §3).
type Store struct{ dir string }

func NewStore(dir string) *Store { return &Store{dir: dir} }

// SchemaFile is one ordered schema input contributing to the
// fingerprint.
type SchemaFile struct {
	Path    string
	Content []byte
}

// Fingerprint is the offline-computable schema identity:
// sha256 over (dialect, pinned server version, ordered schema inputs).
func Fingerprint(dialectName, serverVersion string, files []SchemaFile) string {
	h := sha256.New()
	write := func(s string) { h.Write([]byte(s)); h.Write([]byte{0}) }
	write(dialectName)
	write(serverVersion)
	for _, f := range files {
		write(f.Path)
		h.Write(f.Content)
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))
}

// EntryType / EntryColumn mirror dialect.TypeRef / dialect.ColumnDesc
// without importing the dialect package (cache is a leaf; dialect
// imports cache for the Catalog model).
type EntryType struct {
	OID  uint32 `json:"oid"`
	Name string `json:"name"`
}

// EntryColumn records what the ORACLE said about a result column —
// and nothing derived: nullability verdicts are recomputed from the
// catalog and the parse tree on every run (design 05 §4), so they
// never enter these byte-pinned files. (A vestigial always-false
// "nullable" field was removed 2026-08 without a FormatVersion bump:
// old entries still decode — v1 tolerates the extra field — and
// re-derivation gates were regenerated.)
type EntryColumn struct {
	Name     string `json:"name"`
	OID      uint32 `json:"oid"`
	TypeName string `json:"type_name"`
	SrcRel   uint32 `json:"src_rel,omitempty"`
	SrcAtt   int16  `json:"src_att,omitempty"`
}

// OracleEntry is one cached Describe result, self-describing with its
// full keys.
type OracleEntry struct {
	Format      int           `json:"format"`
	SchemaFP    string        `json:"schema_fp"`
	RenderedSQL string        `json:"rendered_sql"`
	Params      []EntryType   `json:"params"`
	Columns     []EntryColumn `json:"columns"`
}

func queryHash(fp, renderedSQL string) string {
	h := sha256.New()
	h.Write([]byte(fp))
	h.Write([]byte{0})
	h.Write([]byte(renderedSQL))
	return hex.EncodeToString(h.Sum(nil))[:24]
}

// CatalogFileName and OracleFileName expose the store's
// dir-relative file naming, so harnesses (the oracle corpus, entry
// pruning) can address files without duplicating the hashing scheme.
func CatalogFileName(fp string) string {
	return "catalog-" + fp[:min(24, len(fp))] + ".json"
}

func OracleFileName(fp, renderedSQL string) string {
	return filepath.Join("oracle", queryHash(fp, renderedSQL)+".json")
}

func (s *Store) catalogPath(fp string) string {
	return filepath.Join(s.dir, CatalogFileName(fp))
}

func (s *Store) oraclePath(qh string) string {
	return filepath.Join(s.dir, "oracle", qh+".json")
}

// LoadCatalog returns the snapshot for fp, or ok=false on miss or
// key mismatch.
func (s *Store) LoadCatalog(fp string) (*Catalog, bool) {
	data, err := ReadFileCapped(s.catalogPath(fp))
	if err != nil {
		return nil, false
	}
	var cat Catalog
	if err := json.Unmarshal(data, &cat); err != nil || cat.Format != FormatVersion || cat.SchemaFP != fp {
		return nil, false
	}
	return &cat, true
}

func (s *Store) SaveCatalog(cat *Catalog) error {
	data, err := EncodeCatalog(cat)
	if err != nil {
		return err
	}
	return s.writeFile(s.catalogPath(cat.SchemaFP), data)
}

// LoadOracle returns the cached Describe result for (fp, renderedSQL),
// comparing the stored full keys (never trusting the filename hash).
func (s *Store) LoadOracle(fp, renderedSQL string) (*OracleEntry, bool) {
	data, err := ReadFileCapped(s.oraclePath(queryHash(fp, renderedSQL)))
	if err != nil {
		return nil, false
	}
	var e OracleEntry
	if err := json.Unmarshal(data, &e); err != nil {
		return nil, false
	}
	if e.Format != FormatVersion || e.SchemaFP != fp || e.RenderedSQL != renderedSQL {
		return nil, false // format drift, hash collision, or stale file: miss
	}
	return &e, true
}

func (s *Store) SaveOracle(e *OracleEntry) error {
	data, err := EncodeOracle(e)
	if err != nil {
		return err
	}
	return s.writeFile(s.oraclePath(queryHash(e.SchemaFP, e.RenderedSQL)), data)
}

// EncodeCatalog returns the exact canonical bytes SaveCatalog writes.
// It stamps FormatVersion. Anything that compares against a committed
// catalog file byte-wise (the oracle corpus harness) must serialize
// through here, never through its own marshaling.
func EncodeCatalog(cat *Catalog) ([]byte, error) {
	if cat.SchemaFP == "" {
		return nil, fmt.Errorf("catalog snapshot has no schema fingerprint")
	}
	cat.Format = FormatVersion
	return marshalCanonical(cat)
}

// EncodeOracle returns the exact canonical bytes SaveOracle writes,
// stamping FormatVersion — the byte form the corpus harness compares.
func EncodeOracle(e *OracleEntry) ([]byte, error) {
	e.Format = FormatVersion
	return marshalCanonical(e)
}

// marshalCanonical is the store's single serializer: indented JSON,
// LF, trailing newline (v1 API — this output is byte-pinned).
func marshalCanonical(v any) ([]byte, error) {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(data, '\n'), nil
}

// writeFile writes atomically, creating parent directories.
func (s *Store) writeFile(path string, data []byte) error {
	return WriteFileAtomic(path, data, 0o644)
}

// ReadFileCapped reads path but refuses more than MaxFileBytes, so an
// attacker-planted giant file at a computable cache path cannot OOM the
// process. It reads at most MaxFileBytes+1 and rejects if that ceiling
// is reached, so the bound holds even if the file grows after an
// initial stat (no size TOCTOU).
func ReadFileCapped(path string) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	data, err := io.ReadAll(io.LimitReader(f, MaxFileBytes+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > MaxFileBytes {
		return nil, fmt.Errorf("cache file %s exceeds the %d-byte cap", path, MaxFileBytes)
	}
	return data, nil
}

// WriteFileAtomic writes data to path atomically without ever following
// a symlink at path or at a predictable temp name. A cloned repo can
// pre-plant `<path>.tmp` (a computable name) as a symlink to a secret
// or config file; writing through it and renaming over the target would
// corrupt an arbitrary location. os.CreateTemp opens with
// O_CREATE|O_EXCL and a RANDOM suffix, so it neither follows nor
// collides with any planted link; the final os.Rename replaces a
// symlink sitting at path with our regular file rather than writing
// through it.
func WriteFileAtomic(path string, data []byte, perm os.FileMode) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, "."+filepath.Base(path)+".*.tmp")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer func() {
		if tmp != "" {
			_ = os.Remove(tmp)
		}
	}()
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Chmod(perm); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		return err
	}
	tmp = "" // rename consumed it; skip the deferred cleanup
	return nil
}
