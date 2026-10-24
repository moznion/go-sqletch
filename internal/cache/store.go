package cache

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
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
	// OracleDir holds one file per verified rendering.
	OracleDir = "oracle"
)

// legacyRootFile reports whether a cache-root file is one a SUPERSEDED
// layout wrote, which Sweep removes on sight.
//
// It lives next to FormatVersion on purpose: "what did older layouts
// write" belongs where "what layout am I" is declared, so the next
// version bump finds it. v1 named the catalog and the env sidecar by
// fingerprint.
func legacyRootFile(name string) bool {
	return (strings.HasPrefix(name, "catalog-") || strings.HasPrefix(name, "env-")) &&
		strings.HasSuffix(name, ".json")
}

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
	parts := []string{OracleDir}
	for _, seg := range strings.Split(r.Target, "/") {
		parts = append(parts, safeSegment(seg))
	}
	parts = append(parts, safeSegment(r.Query), safeSegment(r.Shape)+".json")
	return path.Join(parts...)
}

// parseOracleRef is OracleFileName's inverse for a store-relative
// path: it is how Walk names what it finds. It reports false for
// anything that is not an entry path, including the flat
// `oracle/<hash>.json` files v1 wrote.
func parseOracleRef(rel string) (OracleRef, bool) {
	parts := strings.Split(filepath.ToSlash(rel), "/")
	if len(parts) < 4 || parts[0] != OracleDir {
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

// Walk calls fn for every oracle entry in the store, with the ref that
// names it and its absolute path, in a deterministic order (lexical,
// parents before children — filepath.WalkDir's).
//
// The store owns its own tree: anything that discovers entries rather
// than addressing one — the oracle corpus, a test — goes through here
// instead of re-deriving the layout. Files that are not entry paths
// (a README, an older layout's leftovers) are skipped; an absent tree
// is not an error.
func (s *Store) Walk(fn func(ref OracleRef, path string) error) error {
	return s.walkEntryFiles(nil, func(rel, p string) error {
		ref, ok := parseOracleRef(rel)
		if !ok {
			return nil
		}
		return fn(ref, p)
	})
}

// Sweep deletes everything in the store that live does not account
// for: entries whose ref is absent, and the root files superseded
// layouts wrote. The two singletons are the store's own and are never
// removed.
//
// It touches only files the store itself writes — regular `.json`
// files under OracleDir, plus legacyRootFile matches at the root — so
// a `.gitignore`, a README, a sibling tool's data file, and every
// other subdirectory survive. A planted symlink is left alone rather
// than followed. That discipline matters because the cache directory
// is configured: pointed at a directory somebody else also owns, a
// broader sweep would eat their files.
//
// Directories emptied by the sweep are removed, so a renamed target or
// query leaves no husk.
func (s *Store) Sweep(live map[OracleRef]bool) error {
	// The root: superseded layouts' catalog/env files. Not recursive —
	// a sibling subdirectory in there is not the store's.
	ents, err := os.ReadDir(s.dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil // nothing written yet: nothing to sweep
	}
	if err != nil {
		return err
	}
	for _, e := range ents {
		if e.IsDir() || !e.Type().IsRegular() || !legacyRootFile(e.Name()) {
			continue
		}
		if err := os.Remove(filepath.Join(s.dir, e.Name())); err != nil {
			return err
		}
	}

	var dirs []string
	err = s.walkEntryFiles(&dirs, func(rel, p string) error {
		if ref, ok := parseOracleRef(rel); ok && live[ref] {
			return nil
		}
		return os.Remove(p)
	})
	if err != nil {
		return err
	}
	// WalkDir yields a parent before its children, so walking dirs
	// backwards is deepest-first. A directory that still holds
	// something fails to remove, which is the intended outcome.
	for i := len(dirs) - 1; i >= 0; i-- {
		_ = os.Remove(dirs[i])
	}
	return nil
}

// walkEntryFiles visits every regular `.json` file under OracleDir,
// passing its store-relative slash path and its absolute path. When
// dirs is non-nil it collects the subdirectories it descended, in
// visit order (Sweep needs them to remove the ones it empties).
func (s *Store) walkEntryFiles(dirs *[]string, fn func(rel, path string) error) error {
	root := filepath.Join(s.dir, OracleDir)
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if p != root && dirs != nil {
				*dirs = append(*dirs, p)
			}
			return nil
		}
		if !d.Type().IsRegular() || !strings.HasSuffix(d.Name(), ".json") {
			return nil
		}
		rel, err := filepath.Rel(s.dir, p)
		if err != nil {
			return err
		}
		return fn(filepath.ToSlash(rel), p)
	})
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	return err
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
//
// The user-defined-type fields (design 26 §4.2) are omitempty: an entry
// whose types are all built-in serializes exactly as before they
// existed, and no MySQL/SQLite entry ever sets them.
type EntryType struct {
	OID    uint32   `json:"oid"`
	Name   string   `json:"name"`
	Schema string   `json:"schema,omitempty"`
	Kind   string   `json:"kind,omitempty"`
	Labels []string `json:"labels,omitempty"`
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
	// TypeSchema/TypeKind/TypeLabels mirror EntryType's
	// user-defined-type fields for the column's type.
	TypeSchema string   `json:"type_schema,omitempty"`
	TypeKind   string   `json:"type_kind,omitempty"`
	TypeLabels []string `json:"type_labels,omitempty"`
	SrcRel     uint32   `json:"src_rel,omitempty"`
	SrcAtt     int16    `json:"src_att,omitempty"`
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
	return s.writeFile(CatalogFile, data)
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
	return s.writeFile(OracleFileName(r), data)
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

// SymlinkError is the store's refusal to write through a symbolic link
// that sits on a DIRECTORY between the cache root and an entry. The
// cache tree is committed, hence repo-controlled: a planted
// `oracle -> /elsewhere` (or a symlinked slug/query directory) would
// otherwise have every miss-fill create and overwrite files outside the
// project, which Walk and Sweep — they never follow links — could not
// even see to clean up. The cli layer reports it as SQLETCH306.
type SymlinkError struct {
	Dir string // the cache directory
	Rel string // the offending component, store-relative, slash form
}

func (e *SymlinkError) Error() string {
	return fmt.Sprintf("cache path %s in %s is a symbolic link; sqletch does not write cache entries through links", e.Rel, e.Dir)
}

// CheckWritable reports, without writing anything, the first symlinked
// directory on the way to OracleDir or to any of refs' entries (a
// *SymlinkError), so a caller can refuse BEFORE the first write and
// leave the committed tree untouched. Missing directories are fine (the
// write creates them), and so is a symlink at an entry's own path: the
// atomic rename replaces it rather than writing through it.
//
// The writes enforce the same rule themselves; this is the early,
// all-or-nothing form of it.
func (s *Store) CheckWritable(refs []OracleRef) error {
	seen := map[string]bool{}
	check := func(rel string) error {
		if seen[rel] {
			return nil
		}
		seen[rel] = true
		fi, err := os.Lstat(filepath.Join(s.dir, filepath.FromSlash(rel)))
		if err != nil {
			return nil // absent (or unreadable): the write decides
		}
		if fi.Mode()&fs.ModeSymlink != 0 {
			return &SymlinkError{Dir: s.dir, Rel: rel}
		}
		return nil
	}
	if err := check(OracleDir); err != nil {
		return err
	}
	for _, r := range refs {
		segs := strings.Split(path.Dir(OracleFileName(r)), "/")
		for i := range segs {
			if err := check(strings.Join(segs[:i+1], "/")); err != nil {
				return err
			}
		}
	}
	return nil
}

// writeFile writes the store-relative file rel atomically, creating
// parent directories. Every operation goes through an os.Root opened on
// the cache directory, so nothing can resolve outside it, and every
// directory component is Lstat'ed first so a committed symlink — even
// one that stays inside the cache — is refused (*SymlinkError) instead
// of followed: the tree the store writes is exactly the tree Walk and
// Sweep see. The leaf keeps WriteFileAtomic's guarantees (random
// O_EXCL temp name, rename replaces a link at the destination).
func (s *Store) writeFile(rel string, data []byte) error {
	// The cache directory itself is the configured path; its ancestry
	// is config.Load's SQLETCH306 check, not the store's.
	if err := os.MkdirAll(s.dir, 0o755); err != nil {
		return err
	}
	root, err := os.OpenRoot(s.dir)
	if err != nil {
		return err
	}
	defer func() { _ = root.Close() }()
	if dir := path.Dir(rel); dir != "." {
		if err := s.mkdirNoFollow(root, dir); err != nil {
			return err
		}
	}
	return writeAtomicIn(root, filepath.FromSlash(rel), data, 0o644)
}

// mkdirNoFollow creates the slash-separated directory dir under root
// one component at a time, refusing any component that is a symlink.
func (s *Store) mkdirNoFollow(root *os.Root, dir string) error {
	cur := ""
	for seg := range strings.SplitSeq(dir, "/") {
		cur = path.Join(cur, seg)
		name := filepath.FromSlash(cur)
		fi, err := root.Lstat(name)
		if errors.Is(err, fs.ErrNotExist) {
			if err := root.Mkdir(name, 0o755); err != nil && !errors.Is(err, fs.ErrExist) {
				return err
			}
			fi, err = root.Lstat(name)
		}
		if err != nil {
			return err
		}
		if fi.Mode()&fs.ModeSymlink != 0 {
			return &SymlinkError{Dir: s.dir, Rel: cur}
		}
		if !fi.IsDir() {
			return fmt.Errorf("cache path %s in %s is not a directory", cur, s.dir)
		}
	}
	return nil
}

// writeAtomicIn is WriteFileAtomic confined to root: the temp file is
// created O_CREATE|O_EXCL under a random name (never follows or
// collides with a planted link) and renamed over name, which replaces
// a symlink sitting there instead of writing through it.
func writeAtomicIn(root *os.Root, name string, data []byte, perm os.FileMode) error {
	tmp := filepath.Join(filepath.Dir(name), "."+filepath.Base(name)+"."+rand.Text()+".tmp")
	f, err := root.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	defer func() {
		if tmp != "" {
			_ = root.Remove(tmp)
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
	if err := root.Rename(tmp, name); err != nil {
		return err
	}
	tmp = "" // rename consumed it; skip the deferred cleanup
	return nil
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
