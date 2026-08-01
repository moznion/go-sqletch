package cache

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

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

type EntryColumn struct {
	Name     string `json:"name"`
	OID      uint32 `json:"oid"`
	TypeName string `json:"type_name"`
	SrcRel   uint32 `json:"src_rel,omitempty"`
	SrcAtt   int16  `json:"src_att,omitempty"`
	Nullable bool   `json:"nullable"`
}

// OracleEntry is one cached Describe result, self-describing with its
// full keys.
type OracleEntry struct {
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

func (s *Store) catalogPath(fp string) string {
	return filepath.Join(s.dir, "catalog-"+fp[:min(24, len(fp))]+".json")
}

func (s *Store) oraclePath(qh string) string {
	return filepath.Join(s.dir, "oracle", qh+".json")
}

// LoadCatalog returns the snapshot for fp, or ok=false on miss or
// key mismatch.
func (s *Store) LoadCatalog(fp string) (*Catalog, bool) {
	data, err := os.ReadFile(s.catalogPath(fp))
	if err != nil {
		return nil, false
	}
	var cat Catalog
	if err := json.Unmarshal(data, &cat); err != nil || cat.SchemaFP != fp {
		return nil, false
	}
	return &cat, true
}

func (s *Store) SaveCatalog(cat *Catalog) error {
	if cat.SchemaFP == "" {
		return fmt.Errorf("catalog snapshot has no schema fingerprint")
	}
	return s.writeJSON(s.catalogPath(cat.SchemaFP), cat)
}

// LoadOracle returns the cached Describe result for (fp, renderedSQL),
// comparing the stored full keys (never trusting the filename hash).
func (s *Store) LoadOracle(fp, renderedSQL string) (*OracleEntry, bool) {
	data, err := os.ReadFile(s.oraclePath(queryHash(fp, renderedSQL)))
	if err != nil {
		return nil, false
	}
	var e OracleEntry
	if err := json.Unmarshal(data, &e); err != nil {
		return nil, false
	}
	if e.SchemaFP != fp || e.RenderedSQL != renderedSQL {
		return nil, false // hash collision or stale file: treat as miss
	}
	return &e, true
}

func (s *Store) SaveOracle(e *OracleEntry) error {
	return s.writeJSON(s.oraclePath(queryHash(e.SchemaFP, e.RenderedSQL)), e)
}

// writeJSON writes canonical, diff-friendly JSON atomically.
func (s *Store) writeJSON(path string, v any) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
