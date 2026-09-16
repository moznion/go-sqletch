package cache

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"
)

// MaxFileBytes bounds every cache file sqletch reads. Cache file names
// are derived from the config and the templates, hence fully
// attacker-computable (doc 21 made them more predictable, not less): a
// cloned repo can plant a file at the exact hit path, so an unbounded
// os.ReadFile would OOM before any key check runs. 64 MiB dwarfs any real catalog/oracle
// entry while capping the blast radius (mirrors the LSP body cap).
const MaxFileBytes = 64 << 20

// FormatVersion is the on-disk cache format. Every written file
// carries it; loads treat any other value — including its absence in
// pre-1.0 caches — as a miss, so a format change can never misread an
// old entry: the pipeline falls back to the database and rewrites.
//
// v2 is the doc-21 layout: named paths instead of fingerprint-derived
// hashes. The bump is what migrates an existing committed cache — every
// v1 file is a miss, the first `generate` rewrites the tree, and the
// same run's sweep removes what the old layout left behind.
const FormatVersion = 2

// CatalogFile and EnvFile are the store's two singleton files. Neither
// name carries the schema fingerprint: it is compared from INSIDE the
// file, so a schema change modifies these files rather than renaming
// them, and one committed cache describes one schema state
// (docs/design/21-cache-layout.md D1).
const (
	CatalogFile = "catalog.json"
	EnvFile     = "env.json"
	oracleDir   = "oracle"
)

// OracleRef is an entry's place in the committed tree: the target slug,
// the query name, and the rendering's shape name (doc 21 §3).
//
// It is a NAME, never a key. LoadOracle still compares the fingerprint
// and the rendered SQL stored inside the file, so a file sitting at a
// ref's path that does not match them is a miss and gets rewritten —
// exactly as a stale hash-named entry was under v1. Nothing downstream
// may treat "found at this path" as evidence of what an entry is.
type OracleRef struct {
	Target string // config.ResolvedTarget.Slug(); may contain '/'
	Query  string
	Shape  string // ast.Rendering.Shape
}

// OracleFileName is the ref's store-relative, slash-separated path.
//
// Every component is folded to [A-Za-z0-9_-]. The grammar already
// restricts query names (SQLETCH002/003) and construct parameters to
// that alphabet and Slug folds the target, so the folding here is
// defence in depth: these strings become filesystem paths, and no
// spelling of them may climb out of the cache directory.
func OracleFileName(r OracleRef) string {
	parts := []string{oracleDir}
	for _, seg := range strings.Split(r.Target, "/") {
		parts = append(parts, safeSegment(seg))
	}
	parts = append(parts, safeSegment(r.Query), safeSegment(r.Shape)+".json")
	return path.Join(parts...)
}

// ParseOracleRef is OracleFileName's inverse for a store-relative path,
// used by anything that discovers entries by walking the tree (the
// oracle corpus, the generate sweep). It reports false for anything
// that is not an entry path.
func ParseOracleRef(rel string) (OracleRef, bool) {
	parts := strings.Split(filepath.ToSlash(rel), "/")
	if len(parts) < 4 || parts[0] != oracleDir {
		return OracleRef{}, false
	}
	last := parts[len(parts)-1]
	if !strings.HasSuffix(last, ".json") {
		return OracleRef{}, false
	}
	return OracleRef{
		Target: strings.Join(parts[1:len(parts)-2], "/"),
		Query:  parts[len(parts)-2],
		Shape:  strings.TrimSuffix(last, ".json"),
	}, true
}

// safeSegment folds one path component to the cache tree's alphabet.
// "." and ".." fold to "_" and "__", so no component can traverse.
func safeSegment(seg string) string {
	if seg == "" {
		return "_"
	}
	b := []byte(seg)
	for i, c := range b {
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9', c == '_', c == '-':
		default:
			b[i] = '_'
		}
	}
	return string(b)
}

// Store is the committed, offline-usable cache of oracle results and
// catalog snapshots. A path is an index, never identity: every entry
// stores its full inputs and loads compare them byte-wise
// (store-and-compare; design 04 §3, layout in design 21).
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

func (s *Store) catalogPath() string {
	return filepath.Join(s.dir, CatalogFile)
}

func (s *Store) oraclePath(r OracleRef) string {
	return filepath.Join(s.dir, filepath.FromSlash(OracleFileName(r)))
}

// LoadCatalog returns the snapshot for fp, or ok=false on miss or
// key mismatch.
func (s *Store) LoadCatalog(fp string) (*Catalog, bool) {
	data, err := ReadFileCapped(s.catalogPath())
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
	return s.writeFile(s.catalogPath(), data)
}

// LoadOracle returns the cached Describe result for (fp, renderedSQL)
// from the entry named by ref, comparing the stored full keys (the
// path is where to look, never what the file is).
func (s *Store) LoadOracle(r OracleRef, fp, renderedSQL string) (*OracleEntry, bool) {
	data, err := ReadFileCapped(s.oraclePath(r))
	if err != nil {
		return nil, false
	}
	var e OracleEntry
	if err := json.Unmarshal(data, &e); err != nil {
		return nil, false
	}
	if e.Format != FormatVersion || e.SchemaFP != fp || e.RenderedSQL != renderedSQL {
		return nil, false // format drift, renamed query, or stale file: miss
	}
	return &e, true
}

func (s *Store) SaveOracle(r OracleRef, e *OracleEntry) error {
	data, err := EncodeOracle(e)
	if err != nil {
		return err
	}
	return s.writeFile(s.oraclePath(r), data)
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
