package pathpat

import (
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// tree materializes files (and the directories they need) under a new
// temp directory, one entry per slash-separated path.
func tree(t *testing.T, files ...string) string {
	t.Helper()
	root := t.TempDir()
	for _, f := range files {
		p := filepath.Join(root, filepath.FromSlash(f))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("-- x\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

func walk(t *testing.T, root, pat string) []Match {
	t.Helper()
	p, err := Compile(pat)
	if err != nil {
		t.Fatalf("Compile(%q): %v", pat, err)
	}
	res, err := p.Walk(root)
	if err != nil {
		t.Fatalf("Walk(%q): %v", pat, err)
	}
	return res.Matches
}

func paths(ms []Match) []string {
	out := make([]string, len(ms))
	for i, m := range ms {
		out[i] = m.Path
	}
	return out
}

func TestCompile_Errors(t *testing.T) {
	cases := []struct {
		pat  string
		want string // substring of the message
	}{
		{"", "empty"},
		{"/abs/queries/*.sql", "absolute"},
		{"../outside/*.sql", `".." segment`},
		{"a/**x/*.sql", "whole segment"},
		{"a/x**/*.sql", "whole segment"},
		{"(a/(b))/*.sql", "nests capture"},
		{"a/b)/*.sql", "no matching"},
		{"(a/b/*.sql", "unclosed capture"},
		{"a(b)/*.sql", "inside a path segment"},
		{"(a)b/*.sql", "inside a path segment"},
		{"()/*.sql", "empty path segment"},
		{"a/[/*.sql", "malformed segment"},
	}
	for _, c := range cases {
		_, err := Compile(c.pat)
		if err == nil {
			t.Errorf("Compile(%q) = nil error, want one", c.pat)
			continue
		}
		if !strings.Contains(err.Error(), c.want) {
			t.Errorf("Compile(%q) error = %v, want it to mention %q", c.pat, err, c.want)
		}
	}
}

func TestCompile_Accepts(t *testing.T) {
	cases := []struct {
		pat  string
		caps int
	}{
		{"queries/*.sql", 0},
		{"queries/**/*.sql", 0},
		{"(api/**/queries)/*.sql", 1},
		{"(a)/(b)/*.sql", 2},
		{"(**)/*.sql", 1},
		{"a/b.sql", 0},
		{"a/[abc]*.sql", 0},
	}
	for _, c := range cases {
		p, err := Compile(c.pat)
		if err != nil {
			t.Errorf("Compile(%q): %v", c.pat, err)
			continue
		}
		if p.NumCaptures() != c.caps {
			t.Errorf("Compile(%q).NumCaptures() = %d, want %d", c.pat, p.NumCaptures(), c.caps)
		}
		if p.String() != c.pat {
			t.Errorf("String() = %q, want %q", p.String(), c.pat)
		}
	}
}

func TestWalk_PlainGlob(t *testing.T) {
	root := tree(t, "queries/a.sql", "queries/b.sql", "queries/c.go", "queries/sub/d.sql", "other/e.sql")
	got := paths(walk(t, root, "queries/*.sql"))
	want := []string{"queries/a.sql", "queries/b.sql"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

// `**` matches zero or more segments — the property filepath.Glob's
// `**` never had (there it is just `*` inside one segment).
func TestWalk_DoubleStarSpansZeroOrMoreSegments(t *testing.T) {
	root := tree(t,
		"q/a.sql",
		"q/x/b.sql",
		"q/x/y/c.sql",
		"q/x/y/z/d.sql",
	)
	got := paths(walk(t, root, "q/**/*.sql"))
	want := []string{"q/a.sql", "q/x/b.sql", "q/x/y/c.sql", "q/x/y/z/d.sql"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

func TestWalk_DoubleStarLast(t *testing.T) {
	root := tree(t, "q/a.sql", "q/x/b.txt", "q/x/y/c.sql")
	got := paths(walk(t, root, "q/**"))
	want := []string{"q/a.sql", "q/x/b.txt", "q/x/y/c.sql"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

func TestWalk_Captures(t *testing.T) {
	root := tree(t,
		"api/app/signin/db/queries/a.sql",
		"api/app/signup/db/queries/b.sql",
		"api/app/signup/db/queries/c.sql",
		"api/app/other/notqueries/d.sql",
	)
	ms := walk(t, root, "(api/app/**/queries)/*.sql")
	var got []string
	for _, m := range ms {
		if len(m.Captures) != 1 {
			t.Fatalf("%q: got %d captures, want 1", m.Path, len(m.Captures))
		}
		got = append(got, m.Path+" -> "+m.Captures[0])
	}
	want := []string{
		"api/app/signin/db/queries/a.sql -> api/app/signin/db/queries",
		"api/app/signup/db/queries/b.sql -> api/app/signup/db/queries",
		"api/app/signup/db/queries/c.sql -> api/app/signup/db/queries",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

func TestWalk_MultipleCaptures(t *testing.T) {
	root := tree(t, "api/signin/db/q/a.sql")
	ms := walk(t, root, "(api)/(signin/db)/q/*.sql")
	if len(ms) != 1 {
		t.Fatalf("got %d matches, want 1", len(ms))
	}
	if want := []string{"api", "signin/db"}; !reflect.DeepEqual(ms[0].Captures, want) {
		t.Errorf("captures = %v, want %v", ms[0].Captures, want)
	}
}

// A capture whose `**` matches zero segments yields the text of the
// segments it did consume, never an empty element or a stray slash.
func TestWalk_CaptureWithEmptyDoubleStar(t *testing.T) {
	root := tree(t, "api/queries/a.sql", "api/x/queries/b.sql")
	ms := walk(t, root, "(api/**/queries)/*.sql")
	got := map[string]string{}
	for _, m := range ms {
		got[m.Path] = m.Captures[0]
	}
	want := map[string]string{
		"api/queries/a.sql":   "api/queries",
		"api/x/queries/b.sql": "api/x/queries",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

// The capture may include the file segment itself.
func TestWalk_CaptureIncludingFile(t *testing.T) {
	root := tree(t, "q/a.sql")
	ms := walk(t, root, "(q/*.sql)")
	if len(ms) != 1 || ms[0].Captures[0] != "q/a.sql" {
		t.Fatalf("got %+v, want one match capturing q/a.sql", ms)
	}
}

func TestWalk_NoMatchIsNotAnError(t *testing.T) {
	root := tree(t, "queries/a.sql")
	if got := paths(walk(t, root, "nope/**/*.sql")); len(got) != 0 {
		t.Errorf("got %v, want none", got)
	}
	if got := paths(walk(t, root, "queries/*.go")); len(got) != 0 {
		t.Errorf("got %v, want none", got)
	}
}

// A directory is never a match, and `**` never descends into a match.
func TestWalk_DirectoriesAreNotMatches(t *testing.T) {
	root := tree(t, "q/a.sql/keep.txt")
	if got := paths(walk(t, root, "q/*.sql")); len(got) != 0 {
		t.Errorf("got %v, want none (a directory named a.sql is not a file)", got)
	}
}

// A broken symlink that the pattern matches is still a match: the
// caller reports "cannot read template file" (SQLETCH308). Dropping it
// here would make a query silently vanish from the package.
func TestWalk_BrokenSymlinkIsAMatch(t *testing.T) {
	root := tree(t, "q/a.sql")
	if err := os.Symlink(filepath.Join(root, "q", "nope"), filepath.Join(root, "q", "b.sql")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	got := paths(walk(t, root, "q/*.sql"))
	if want := []string{"q/a.sql", "q/b.sql"}; !reflect.DeepEqual(got, want) {
		t.Errorf("glob segment got %v, want %v", got, want)
	}
	if got := paths(walk(t, root, "q/b.sql")); !reflect.DeepEqual(got, []string{"q/b.sql"}) {
		t.Errorf("literal segment got %v, want [q/b.sql]", got)
	}
	if got := paths(walk(t, root, "q/**")); !reflect.DeepEqual(got, []string{"q/a.sql", "q/b.sql"}) {
		t.Errorf("`**` got %v, want [q/a.sql q/b.sql]", got)
	}
}

// A directory symlink is descended, never reported as a match.
func TestWalk_DirectorySymlinkIsNotAMatch(t *testing.T) {
	root := tree(t, "real/x.sql", "q/keep.sql")
	if err := os.Symlink(filepath.Join(root, "real"), filepath.Join(root, "q", "link.sql")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if got := paths(walk(t, root, "q/*.sql")); !reflect.DeepEqual(got, []string{"q/keep.sql"}) {
		t.Errorf("got %v, want [q/keep.sql]", got)
	}
	if got := paths(walk(t, root, "q/**")); !reflect.DeepEqual(got, []string{"q/keep.sql", "q/link.sql/x.sql"}) {
		t.Errorf("`**` got %v, want [q/keep.sql q/link.sql/x.sql]", got)
	}
}

func TestWalk_PrunesUnderDoubleStarButNotLiteralSegments(t *testing.T) {
	root := tree(t,
		"q/a.sql",
		"q/.git/b.sql",
		"q/node_modules/c.sql",
		"q/vendor/d.sql",
		"q/.hidden/e.sql",
		"q/keep/f.sql",
	)
	got := paths(walk(t, root, "q/**/*.sql"))
	want := []string{"q/a.sql", "q/keep/f.sql"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("`**` walk got %v, want %v", got, want)
	}
	// Naming the pruned directory explicitly still reaches it: pruning
	// bounds the recursive expansion, it does not hide files.
	got = paths(walk(t, root, "q/vendor/*.sql"))
	if want := []string{"q/vendor/d.sql"}; !reflect.DeepEqual(got, want) {
		t.Errorf("literal segment got %v, want %v", got, want)
	}
	got = paths(walk(t, root, "q/.hidden/*.sql"))
	if want := []string{"q/.hidden/e.sql"}; !reflect.DeepEqual(got, want) {
		t.Errorf("literal hidden segment got %v, want %v", got, want)
	}
}

func TestWalk_SymlinkCycleTerminates(t *testing.T) {
	root := tree(t, "q/a.sql")
	if err := os.Symlink(filepath.Join(root, "q"), filepath.Join(root, "q", "loop")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	got := paths(walk(t, root, "q/**/*.sql"))
	// The loop is entered once (it is a distinct path), never twice.
	want := []string{"q/a.sql"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

// A symlinked directory is walked (the caller's SQLETCH306 check, not
// this package, decides whether an escaping target is acceptable).
func TestWalk_FollowsDirectorySymlinkOnce(t *testing.T) {
	root := tree(t, "real/a.sql", "q/keep.sql")
	if err := os.Symlink(filepath.Join(root, "real"), filepath.Join(root, "q", "link")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	got := paths(walk(t, root, "q/**/*.sql"))
	want := []string{"q/keep.sql", "q/link/a.sql"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

func TestWalk_RecordsConsultedDirectories(t *testing.T) {
	root := tree(t, "api/app/queries/a.sql")
	p, err := Compile("api/app/queries/*.sql")
	if err != nil {
		t.Fatal(err)
	}
	res, err := p.Walk(root)
	if err != nil {
		t.Fatal(err)
	}
	// Every ancestor is a dependency: creating api/app/queries/b.sql
	// changes the last one's mtime, and creating a whole new app
	// changes an outer one's.
	for _, want := range []string{"", "api", "api/app", "api/app/queries"} {
		found := false
		for _, d := range res.Dirs {
			if d == want {
				found = true
			}
		}
		if !found {
			t.Errorf("Dirs = %v, want it to contain %q", res.Dirs, want)
		}
	}
}

func TestWalk_BudgetRefusal(t *testing.T) {
	root := tree(t, "q/a.sql", "q/b.sql", "q/c.sql", "q/d/e.sql")
	old := maxWalkEntries
	maxWalkEntries = 2
	defer func() { maxWalkEntries = old }()
	p, err := Compile("q/**/*.sql")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.Walk(root); err == nil {
		t.Fatal("want a budget refusal, got nil")
	} else if !strings.Contains(err.Error(), "too many directory entries") {
		t.Errorf("error = %v, want the budget message", err)
	}
}

func TestMaxRef(t *testing.T) {
	cases := []struct {
		in   string
		want int
		err  bool
	}{
		{"gen", 0, false},
		{"$1/gen", 1, false},
		{"${1}/gen", 1, false},
		{"$1/$3/x", 3, false},
		{"$$1", 0, false},
		{"a$$b$2", 2, false},
		{"$", 0, true},
		{"$x", 0, true},
		{"$0", 0, true},
		{"${1", 0, true},
		{"${x}", 0, true},
	}
	for _, c := range cases {
		got, err := MaxRef(c.in)
		if (err != nil) != c.err {
			t.Errorf("MaxRef(%q) error = %v, want error: %v", c.in, err, c.err)
			continue
		}
		if err == nil && got != c.want {
			t.Errorf("MaxRef(%q) = %d, want %d", c.in, got, c.want)
		}
	}
}

func TestSubstitute(t *testing.T) {
	caps := []string{"api/signin", "db"}
	cases := []struct{ in, want string }{
		{"gen", "gen"},
		{"$1/gen", "api/signin/gen"},
		{"${1}gen", "api/signingen"},
		{"$2/$1", "db/api/signin"},
		{"$$1", "$1"},
	}
	for _, c := range cases {
		got, err := Substitute(c.in, caps)
		if err != nil {
			t.Errorf("Substitute(%q): %v", c.in, err)
			continue
		}
		if got != c.want {
			t.Errorf("Substitute(%q) = %q, want %q", c.in, got, c.want)
		}
	}
	if _, err := Substitute("$3", caps); err == nil {
		t.Error("Substitute with an out-of-range reference: want an error")
	}
}

// refMatch is an independent, brute-force implementation of the
// matching rules: it is the property test's oracle, so a bug in the
// directory-descending walker cannot hide behind the walker's own
// notion of matching.
func refMatch(segs []string, parts []string) bool {
	if len(segs) == 0 {
		return len(parts) == 0
	}
	if segs[0] == "**" {
		for i := 0; i <= len(parts); i++ {
			if refMatch(segs[1:], parts[i:]) {
				return true
			}
		}
		return false
	}
	if len(parts) == 0 {
		return false
	}
	ok, err := filepath.Match(segs[0], parts[0])
	if err != nil || !ok {
		return false
	}
	return refMatch(segs[1:], parts[1:])
}

// Walk must agree with the brute-force matcher on every file of a
// randomly shaped tree, for every pattern — the walker prunes the
// search space, and a pruning bug is exactly a silently missing query
// file.
func TestWalk_AgreesWithBruteForce(t *testing.T) {
	rng := rand.New(rand.NewSource(20260911))
	names := []string{"a", "b", "app", "db", "queries"}
	var files []string
	for i := 0; i < 60; i++ {
		depth := 1 + rng.Intn(4)
		parts := make([]string, 0, depth+1)
		for d := 0; d < depth; d++ {
			parts = append(parts, names[rng.Intn(len(names))])
		}
		ext := ".sql"
		if rng.Intn(3) == 0 {
			ext = ".go"
		}
		parts = append(parts, fmt.Sprintf("f%d%s", rng.Intn(4), ext))
		files = append(files, strings.Join(parts, "/"))
	}
	root := tree(t, files...)

	// The set of files that actually exist (the tree may collide on
	// names; a file cannot exist where a directory does).
	var existing []string
	if err := filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		rel, err := filepath.Rel(root, p)
		if err != nil {
			return err
		}
		existing = append(existing, filepath.ToSlash(rel))
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	pats := []string{
		"**/*.sql",
		"*/**/*.sql",
		"app/**/queries/*.sql",
		"**/queries/**/*.go",
		"a/b/*.sql",
		"**",
		"**/**/*.sql",
		"(**)/*.sql",
		"?/*.sql",
	}
	for _, pat := range pats {
		want := map[string]bool{}
		segs := strings.Split(pat, "/")
		for i, s := range segs {
			segs[i] = strings.Trim(s, "()")
		}
		for _, f := range existing {
			if refMatch(segs, strings.Split(f, "/")) {
				want[f] = true
			}
		}
		got := map[string]bool{}
		for _, m := range walk(t, root, pat) {
			if got[m.Path] {
				t.Errorf("pattern %q: %q matched twice", pat, m.Path)
			}
			got[m.Path] = true
		}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("pattern %q:\n got  %v\n want %v", pat, keys(got), keys(want))
		}
	}
}

func keys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
