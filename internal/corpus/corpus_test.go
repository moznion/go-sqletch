package corpus

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/moznion/go-sqletch/internal/cache"
	"github.com/moznion/go-sqletch/internal/dialect"
)

// fakeOracle answers from the case's own committed data, optionally
// warped: it is what a perfectly-agreeing backend looks like.
type fakeOracle struct {
	c *Case
	// warpDescribe, when set, may replace the answer for one SQL.
	warpDescribe func(sql string, d dialect.Desc) (dialect.Desc, error)
}

func (f *fakeOracle) Describe(_ context.Context, sql string) (dialect.Desc, error) {
	for _, e := range f.c.Entries {
		if e.E.RenderedSQL == sql {
			d := dialect.DescFromEntry(e.E)
			if f.warpDescribe != nil {
				return f.warpDescribe(sql, d)
			}
			return d, nil
		}
	}
	return dialect.Desc{}, &dialect.OracleError{Pos: -1, Msg: "unknown SQL"}
}

func (f *fakeOracle) Plan(context.Context, string) error { return nil }

func (f *fakeOracle) Snapshot(context.Context) (*cache.Catalog, error) {
	// Real backends answer for a schema, not a fingerprint: return a
	// copy with the identity fields cleared to prove Replay stamps
	// them the way the pipeline does.
	cp := *f.c.Catalog
	cp.SchemaFP = ""
	cp.Format = 0
	return &cp, nil
}

func (f *fakeOracle) ServerVersion(context.Context) (string, error) {
	return f.c.ServerVersion, nil
}

func backendFor(o dialect.Oracle) Backend {
	return func(context.Context, *Case) (dialect.Oracle, func(), error) { return o, nil, nil }
}

func loadCommitted(t *testing.T) []*Case {
	t.Helper()
	cases, err := LoadAll("testdata")
	if err != nil {
		t.Fatalf("LoadAll: %v", err)
	}
	if len(cases) == 0 {
		t.Fatal("committed corpus is empty; at least one case is required")
	}
	return cases
}

// TestCommittedCorpusIntegrity is the always-on gate over the
// committed ground truth: every case loads through store-and-compare,
// in canonical bytes, with a recomputable fingerprint.
func TestCommittedCorpusIntegrity(t *testing.T) {
	for _, c := range loadCommitted(t) {
		if len(c.Entries) == 0 {
			t.Errorf("%s: no oracle entries", c.Name)
		}
		if len(c.Catalog.Tables) == 0 {
			t.Errorf("%s: catalog has no tables", c.Name)
		}
		for _, e := range c.Entries {
			if e.E.RenderedSQL == "" {
				t.Errorf("%s: %s: empty rendered_sql", c.Name, e.Path)
			}
		}
	}
}

// TestReplayAgreeingBackend pins the zero-mismatch path: a backend
// that answers exactly what the corpus recorded replays clean.
func TestReplayAgreeingBackend(t *testing.T) {
	for _, c := range loadCommitted(t) {
		ms, err := Replay(context.Background(), c, backendFor(&fakeOracle{c: c}))
		if err != nil {
			t.Fatalf("%s: %v", c.Name, err)
		}
		for _, m := range ms {
			t.Errorf("%s: unexpected mismatch: %s", c.Name, m)
		}
	}
}

// TestReplayDetectsTypeDrift: one wrong column type on one entry must
// surface as exactly one diff mismatch naming that entry's file.
func TestReplayDetectsTypeDrift(t *testing.T) {
	c := loadCommitted(t)[0]
	target := c.Entries[0]
	o := &fakeOracle{c: c, warpDescribe: func(sql string, d dialect.Desc) (dialect.Desc, error) {
		if sql == target.E.RenderedSQL && len(d.Columns) > 0 {
			d.Columns[0].Type.Name = "drifted"
		}
		return d, nil
	}}
	ms, err := Replay(context.Background(), c, backendFor(o))
	if err != nil {
		t.Fatal(err)
	}
	if len(ms) != 1 {
		t.Fatalf("want exactly 1 mismatch, got %d: %v", len(ms), ms)
	}
	m := ms[0]
	if m.Kind != MismatchDiff || m.Path != target.Path {
		t.Fatalf("want diff on %s, got %s", target.Path, m)
	}
	if !strings.Contains(m.Detail, "drifted") {
		t.Fatalf("detail should show the differing line, got %q", m.Detail)
	}
}

// TestReplayReportsRefusal: a backend that errors on an accepted
// input is the dangerous direction and must be reported per entry,
// not swallowed and not fatal to the rest of the replay.
func TestReplayReportsRefusal(t *testing.T) {
	c := loadCommitted(t)[0]
	target := c.Entries[0]
	o := &fakeOracle{c: c, warpDescribe: func(sql string, d dialect.Desc) (dialect.Desc, error) {
		if sql == target.E.RenderedSQL {
			return dialect.Desc{}, &dialect.OracleError{Pos: -1, Msg: "outside modeled subset"}
		}
		return d, nil
	}}
	ms, err := Replay(context.Background(), c, backendFor(o))
	if err != nil {
		t.Fatal(err)
	}
	if len(ms) != 1 || ms[0].Kind != MismatchError || ms[0].Path != target.Path {
		t.Fatalf("want one error mismatch on %s, got %v", target.Path, ms)
	}
}

// TestReplayCancellation: context cancellation is environmental, not
// a finding.
func TestReplayCancellation(t *testing.T) {
	c := loadCommitted(t)[0]
	ctx, cancel := context.WithCancel(context.Background())
	o := &fakeOracle{c: c, warpDescribe: func(sql string, d dialect.Desc) (dialect.Desc, error) {
		cancel()
		return dialect.Desc{}, ctx.Err()
	}}
	if _, err := Replay(ctx, c, backendFor(o)); !errors.Is(err, context.Canceled) {
		t.Fatalf("want context.Canceled, got %v", err)
	}
}

// synthCase writes a minimal valid case into dir and returns its
// pieces for doctoring.
func synthCase(t *testing.T, dir string) (fp string, store *cache.Store) {
	t.Helper()
	schema := []byte("CREATE TABLE t (id BIGINT NOT NULL);\n")
	if err := os.WriteFile(filepath.Join(dir, "schema.sql"), schema, 0o644); err != nil {
		t.Fatal(err)
	}
	manifest := []byte("{\n  \"dialect\": \"mysql\",\n  \"server_version\": \"8.4\",\n  \"schema\": [\n    \"schema.sql\"\n  ]\n}\n")
	if err := os.WriteFile(filepath.Join(dir, "corpus.json"), manifest, 0o644); err != nil {
		t.Fatal(err)
	}
	fp = cache.Fingerprint("mysql", "8.4", []cache.SchemaFile{{Path: "schema.sql", Content: schema}})
	store = cache.NewStore(filepath.Join(dir, "cache"))
	if err := store.SaveCatalog(&cache.Catalog{
		SchemaFP: fp,
		Tables: []cache.Table{{Schema: "test", Name: "t", OID: 1, Cols: []cache.Column{
			{Name: "id", Att: 1, TypeOID: 8, TypeName: "bigint", NotNull: true},
		}}},
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.SaveOracle(&cache.OracleEntry{
		SchemaFP:    fp,
		RenderedSQL: "SELECT id FROM t",
		Params:      []cache.EntryType{},
		Columns:     []cache.EntryColumn{{Name: "id", OID: 8, TypeName: "bigint", SrcRel: 1, SrcAtt: 1}},
	}); err != nil {
		t.Fatal(err)
	}
	return fp, store
}

func TestLoadSynthesizedCase(t *testing.T) {
	dir := t.TempDir()
	fp, _ := synthCase(t, dir)
	c, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if c.FP != fp || len(c.Entries) != 1 || len(c.Catalog.Tables) != 1 {
		t.Fatalf("unexpected case: fp=%s entries=%d", c.FP, len(c.Entries))
	}
}

func TestLoadRejections(t *testing.T) {
	t.Run("unknown manifest key", func(t *testing.T) {
		dir := t.TempDir()
		synthCase(t, dir)
		bad := []byte("{\n  \"dialect\": \"mysql\",\n  \"server_version\": \"8.4\",\n  \"schema\": [\"schema.sql\"],\n  \"typo\": true\n}\n")
		if err := os.WriteFile(filepath.Join(dir, "corpus.json"), bad, 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := Load(dir); err == nil || !strings.Contains(err.Error(), "typo") {
			t.Fatalf("want unknown-key error, got %v", err)
		}
	})
	t.Run("schema drift orphans the catalog", func(t *testing.T) {
		dir := t.TempDir()
		synthCase(t, dir)
		if err := os.WriteFile(filepath.Join(dir, "schema.sql"), []byte("CREATE TABLE t (id BIGINT);\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := Load(dir); err == nil || !strings.Contains(err.Error(), "no catalog") {
			t.Fatalf("want fingerprint-miss error, got %v", err)
		}
	})
	t.Run("foreign entry", func(t *testing.T) {
		dir := t.TempDir()
		synthCase(t, dir)
		other := cache.NewStore(filepath.Join(dir, "cache"))
		if err := other.SaveOracle(&cache.OracleEntry{
			SchemaFP:    "deadbeef",
			RenderedSQL: "SELECT 1",
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := Load(dir); err == nil || !strings.Contains(err.Error(), "does not load") {
			t.Fatalf("want foreign-entry error, got %v", err)
		}
	})
	t.Run("non-canonical entry bytes", func(t *testing.T) {
		dir := t.TempDir()
		fp, _ := synthCase(t, dir)
		p := filepath.Join(dir, "cache", filepath.FromSlash(cache.OracleFileName(fp, "SELECT id FROM t")))
		raw, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, append(raw, '\n'), 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := Load(dir); err == nil || !strings.Contains(err.Error(), "canonical") {
			t.Fatalf("want canonical-form error, got %v", err)
		}
	})
}

// TestLoadAllDeterministic: case order is name-sorted and stable.
func TestLoadAllDeterministic(t *testing.T) {
	a := loadCommitted(t)
	b := loadCommitted(t)
	if len(a) != len(b) {
		t.Fatal("unstable case count")
	}
	for i := range a {
		if a[i].Name != b[i].Name {
			t.Fatalf("unstable order: %s vs %s", a[i].Name, b[i].Name)
		}
		if i > 0 && a[i-1].Name >= a[i].Name {
			t.Fatalf("not name-sorted: %s before %s", a[i-1].Name, a[i].Name)
		}
	}
}
