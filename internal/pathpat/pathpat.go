// Package pathpat compiles and expands sqletch's query-file patterns:
// project-relative globs with recursive `**` segments and capture
// groups whose matched text is substituted into a target's output
// path (docs/design/19-multi-target-output.md §3).
//
// It replaces filepath.Glob for `targets[].queries` for two reasons:
// filepath.Glob has no `**` (its `**` is just `*` inside one segment,
// so design 07's advertised `queries/**/*.sql` never worked), and no
// glob library returns CAPTURES — which would force two engines, one
// to enumerate and one to capture, that could disagree. One engine
// both enumerates and captures, so it cannot drift from itself.
package pathpat

import (
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
)

// maxWalkEntries bounds the directory entries one Walk may examine. A
// pattern is a self-authored, trusted artifact (spec §"Threat model /
// trust boundary"), so an expensive `**` is a user mistake and not a
// vulnerability; the cap exists so a stray `**` at a filesystem root
// cannot hang `sqletch lsp`, which re-expands patterns as the
// workspace changes.
// It is a var only so the test suite can exercise the refusal without
// materializing 200k directory entries.
var maxWalkEntries = 200_000

// prunedDirs are directory names the `**` expansion never descends
// into. They hold no templates and dominate the walk cost of a real
// repository. Pruning applies ONLY to `**` expansion: a pattern that
// names such a directory in a literal segment still matches it, so
// the rule cannot make a deliberately-targeted file unreachable.
var prunedDirs = map[string]bool{
	".git":         true,
	"node_modules": true,
	"vendor":       true,
}

// Pattern is a compiled query-file pattern.
type Pattern struct {
	raw  string
	segs []segment
	caps []capture
}

type segment struct {
	glob      string // path.Match syntax, capture parens stripped
	literal   bool   // no glob metacharacter: can be looked up by name
	recursive bool   // the segment is exactly "**"
}

// capture is a half-open range of segment indices. Captures align to
// segment boundaries by design (§3): the fan-out use case captures
// directories, and an intra-segment capture would force a
// character-level matcher for no gain.
type capture struct{ start, end int }

// Match is one file the pattern matched.
type Match struct {
	// Path is project-relative and always slash-separated, so a
	// substituted output path is byte-identical on every platform.
	Path string
	// Captures holds one entry per capture group, in order.
	Captures []string
}

// Result is one expansion of a pattern against a project directory.
type Result struct {
	// Matches is sorted by Path.
	Matches []Match
	// Dirs lists every directory the walk consulted, project-relative
	// and slash-separated. It is the LSP's memo signature (§6): a
	// directory's mtime changes when an entry is added or removed, so
	// re-stating these detects a file appearing or disappearing
	// without re-walking the tree.
	Dirs []string
}

// Compile parses a pattern. Errors are user-facing config messages:
// the caller wraps them in SQLETCH301 without rephrasing.
func Compile(pat string) (*Pattern, error) {
	if pat == "" {
		return nil, errors.New("pattern is empty")
	}
	if filepath.IsAbs(pat) || strings.HasPrefix(pat, "/") {
		return nil, fmt.Errorf("pattern %q is absolute: query patterns are project-relative so a cloned repository cannot read files outside it", pat)
	}
	p := &Pattern{raw: pat}
	open := -1
	for i, raw := range strings.Split(path.Clean(pat), "/") {
		seg := raw
		if strings.HasPrefix(seg, "(") && open >= 0 {
			return nil, fmt.Errorf("pattern %q nests capture groups: `(`…`)` may not contain another `(`", pat)
		}
		if strings.Contains(strings.TrimPrefix(seg, "("), "(") ||
			strings.Contains(strings.TrimSuffix(seg, ")"), ")") {
			return nil, fmt.Errorf("pattern %q puts a capture boundary inside a path segment: `(`…`)` must span whole segments, e.g. (a/b)/*.sql", pat)
		}
		if strings.HasPrefix(seg, "(") {
			open = i
			seg = seg[1:]
		}
		closes := false
		if strings.HasSuffix(seg, ")") {
			if open < 0 {
				return nil, fmt.Errorf("pattern %q has a `)` with no matching `(`", pat)
			}
			closes = true
			seg = seg[:len(seg)-1]
		}
		if strings.ContainsAny(seg, "()") {
			return nil, fmt.Errorf("pattern %q puts a capture boundary inside a path segment: `(`…`)` must span whole segments, e.g. (a/b)/*.sql", pat)
		}
		if seg == "" {
			return nil, fmt.Errorf("pattern %q has an empty path segment", pat)
		}
		if seg == "." || seg == ".." {
			return nil, fmt.Errorf("pattern %q contains a %q segment: query patterns must stay inside the project directory", pat, seg)
		}
		if strings.Contains(seg, "**") && seg != "**" {
			return nil, fmt.Errorf("pattern %q mixes `**` with other characters in one segment: `**` must occupy a whole segment (it matches zero or more of them); use `*` to match within a segment", pat)
		}
		if _, err := path.Match(seg, "x"); err != nil {
			return nil, fmt.Errorf("pattern %q has a malformed segment %q: %v", pat, raw, err)
		}
		p.segs = append(p.segs, segment{
			glob:      seg,
			literal:   !strings.ContainsAny(seg, "*?["),
			recursive: seg == "**",
		})
		if closes {
			p.caps = append(p.caps, capture{start: open, end: i + 1})
			open = -1
		}
	}
	if open >= 0 {
		return nil, fmt.Errorf("pattern %q has an unclosed capture group", pat)
	}
	return p, nil
}

// NumCaptures reports how many capture groups the pattern declares.
func (p *Pattern) NumCaptures() int { return len(p.caps) }

// String returns the pattern as written.
func (p *Pattern) String() string { return p.raw }

// Walk expands the pattern against root, returning matches sorted by
// path. Directory symlinks are followed at most once each (a resolved
// path visited set), so a symlink cycle terminates; whether a matched
// path is ACCEPTABLE — inside the project, not reached through an
// escaping symlink — is the caller's SQLETCH306 check, not this
// package's.
func (p *Pattern) Walk(root string) (Result, error) {
	realRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		realRoot = filepath.Clean(root)
	}
	w := &walker{
		root:     root,
		realRoot: realRoot,
		pat:      p,
		visited:  map[string]bool{},
		seenDir:  map[string]bool{},
		emitted:  map[string]bool{},
		starts:   make([]int, len(p.segs)),
		ends:     make([]int, len(p.segs)),
	}
	if err := w.match(0, ""); err != nil {
		return Result{}, err
	}
	sort.Slice(w.matches, func(i, j int) bool { return w.matches[i].Path < w.matches[j].Path })
	sort.Strings(w.dirs)
	return Result{Matches: w.matches, Dirs: w.dirs}, nil
}

type walker struct {
	root string
	// realRoot is root with symlinks resolved, so the visited-set key
	// of an ordinary directory (built lexically from it) is comparable
	// with the EvalSymlinks result of a symlinked one. Keying ordinary
	// directories on the UNresolved path was a cycle-detection BYPASS:
	// a repo reached through a symlinked ancestor (macOS /var ->
	// /private/var, the temp dirs the tests run in) made the two
	// spellings differ, so `loop -> .` was walked twice.
	realRoot string
	pat      *Pattern

	names   []string // path segments consumed so far
	starts  []int    // per pattern segment: first index into names it consumed
	ends    []int    // per pattern segment: one past its last index into names
	matches []Match
	// emitted dedupes paths reachable by more than one derivation: two
	// `**` segments (`**/queries/**/*.sql` over a/queries/queries/x)
	// can split the same path in several ways. The FIRST derivation
	// wins, and traversal order is fixed (a `**` tries the shortest
	// expansion first, os.ReadDir is sorted), so which captures a
	// doubly-derivable path gets is deterministic.
	emitted map[string]bool

	dirs    []string        // directories consulted (project-relative)
	seenDir map[string]bool // dedupes dirs
	visited map[string]bool // resolved directory paths already descended
	budget  int
}

// abs joins the project-relative dir onto root for filesystem calls.
func (w *walker) abs(rel string) string {
	if rel == "" {
		return w.root
	}
	return filepath.Join(w.root, filepath.FromSlash(rel))
}

func (w *walker) join(dir, name string) string {
	if dir == "" {
		return name
	}
	return dir + "/" + name
}

// noteDir records that the walk depended on dir's contents.
func (w *walker) noteDir(rel string) {
	if !w.seenDir[rel] {
		w.seenDir[rel] = true
		w.dirs = append(w.dirs, rel)
	}
}

var errWalkBudget = errors.New("pattern expansion examined too many directory entries")

func (w *walker) spend(n int) error {
	w.budget += n
	if w.budget > maxWalkEntries {
		return fmt.Errorf("%w (cap %d): narrow the pattern — a `**` near the project root walks the whole tree", errWalkBudget, maxWalkEntries)
	}
	return nil
}

// readDir lists dir, tolerating an unreadable or missing directory (a
// pattern naming a directory that does not exist yet is a zero-match,
// which resolution reports as a warning, not a read failure).
func (w *walker) readDir(rel string) ([]os.DirEntry, error) {
	w.noteDir(rel)
	entries, err := os.ReadDir(w.abs(rel))
	if err != nil {
		return nil, nil //nolint:nilerr // unreadable directory == no matches
	}
	if err := w.spend(len(entries)); err != nil {
		return nil, err
	}
	return entries, nil
}

// enterDir reports whether rel names a directory the walk may descend
// into, and returns the key under which it is marked visited. A
// symlinked directory is keyed on its RESOLVED path so a cycle
// terminates after one visit; an ordinary directory is keyed on its
// own path, which costs no EvalSymlinks call on the common path.
func (w *walker) enterDir(rel string) (string, bool) {
	abs := w.abs(rel)
	fi, err := os.Lstat(abs)
	if err != nil {
		return "", false
	}
	key := filepath.Join(w.realRoot, filepath.FromSlash(rel))
	if fi.Mode()&os.ModeSymlink != 0 {
		target, err := os.Stat(abs)
		if err != nil || !target.IsDir() {
			return "", false
		}
		if real, err := filepath.EvalSymlinks(abs); err == nil {
			key = real
		}
	} else if !fi.IsDir() {
		return "", false
	}
	if w.visited[key] {
		return "", false
	}
	w.visited[key] = true
	return key, true
}

func (w *walker) leaveDir(key string) { delete(w.visited, key) }

// isDir reports whether rel resolves to a directory. Everything else
// — a regular file, a symlink to one, and a BROKEN symlink — is a
// candidate match: an unreadable template file must reach the caller
// so it can be reported (the LSP's SQLETCH308 degradation), not
// silently vanish from the query set.
func (w *walker) isDir(rel string) bool {
	fi, err := os.Stat(w.abs(rel))
	return err == nil && fi.IsDir()
}

// match consumes pattern segment si inside the project-relative
// directory dir.
func (w *walker) match(si int, dir string) error {
	s := w.pat.segs[si]
	last := si == len(w.pat.segs)-1
	if s.recursive {
		w.starts[si] = len(w.names)
		return w.matchStar(si, dir, last)
	}
	w.starts[si] = len(w.names)
	defer func() { w.names = w.names[:w.starts[si]] }()

	// A literal segment is looked up by name rather than by listing
	// the directory: a project's template directory can sit under a
	// directory with many thousands of entries, and the parent is
	// still recorded as a dependency (its mtime moves when the child
	// appears), so the LSP memo stays sound.
	if s.literal {
		w.noteDir(dir)
		if err := w.spend(1); err != nil {
			return err
		}
		rel := w.join(dir, s.glob)
		w.names = append(w.names, s.glob)
		w.ends[si] = len(w.names)
		if last {
			if !w.isDir(rel) {
				w.emit(rel)
			}
			return nil
		}
		key, ok := w.enterDir(rel)
		if !ok {
			return nil
		}
		defer w.leaveDir(key)
		return w.match(si+1, rel)
	}

	entries, err := w.readDir(dir)
	if err != nil {
		return err
	}
	for _, e := range entries {
		ok, err := path.Match(s.glob, e.Name())
		if err != nil || !ok {
			continue
		}
		rel := w.join(dir, e.Name())
		w.names = append(w.names[:w.starts[si]], e.Name())
		w.ends[si] = len(w.names)
		if last {
			if e.Type().IsRegular() || (!e.IsDir() && !w.isDir(rel)) {
				w.emit(rel)
			}
			continue
		}
		key, ok := w.enterDir(rel)
		if !ok {
			continue
		}
		err = w.match(si+1, rel)
		w.leaveDir(key)
		if err != nil {
			return err
		}
	}
	return nil
}

// matchStar expands a `**` segment: it has already consumed the names
// in w.names[starts[si]:], and may consume more, or stop here.
func (w *walker) matchStar(si int, dir string, last bool) error {
	w.ends[si] = len(w.names)
	if last {
		// `**` in final position consumes the file name too, so it
		// matches every file at any depth below the anchor.
		entries, err := w.readDir(dir)
		if err != nil {
			return err
		}
		for _, e := range entries {
			if e.IsDir() {
				continue
			}
			rel := w.join(dir, e.Name())
			if !e.Type().IsRegular() && w.isDir(rel) {
				continue
			}
			w.names = append(w.names, e.Name())
			w.ends[si] = len(w.names)
			w.emit(rel)
			w.names = w.names[:len(w.names)-1]
			w.ends[si] = len(w.names)
		}
	} else if err := w.match(si+1, dir); err != nil {
		return err
	}

	entries, err := w.readDir(dir)
	if err != nil {
		return err
	}
	for _, e := range entries {
		if !e.IsDir() && e.Type()&os.ModeSymlink == 0 {
			continue
		}
		if prunedDirs[e.Name()] || strings.HasPrefix(e.Name(), ".") {
			continue
		}
		rel := w.join(dir, e.Name())
		key, ok := w.enterDir(rel)
		if !ok {
			continue
		}
		w.names = append(w.names, e.Name())
		err := w.matchStar(si, rel, last)
		w.names = w.names[:len(w.names)-1]
		w.ends[si] = len(w.names)
		w.leaveDir(key)
		if err != nil {
			return err
		}
	}
	return nil
}

// emit records a match, resolving its capture texts from the names
// each capture's segment range consumed.
func (w *walker) emit(rel string) {
	if w.emitted[rel] {
		return
	}
	w.emitted[rel] = true
	m := Match{Path: rel}
	if len(w.pat.caps) > 0 {
		m.Captures = make([]string, len(w.pat.caps))
		for i, c := range w.pat.caps {
			lo, hi := w.starts[c.start], w.ends[c.end-1]
			if hi < lo {
				hi = lo
			}
			m.Captures[i] = strings.Join(w.names[lo:hi], "/")
		}
	}
	w.matches = append(w.matches, m)
}

// MaxRef returns the highest $n referenced in s, and 0 when it
// references none. A malformed reference is an error so a typo
// (`$x`, a trailing `$`) is not silently emitted as a literal.
func MaxRef(s string) (int, error) {
	max := 0
	for i := 0; i < len(s); i++ {
		if s[i] != '$' {
			continue
		}
		n, next, err := parseRef(s, i)
		if err != nil {
			return 0, err
		}
		if n > max {
			max = n
		}
		i = next - 1
	}
	return max, nil
}

// Substitute replaces $n / ${n} in s with captures[n-1]. It is only
// called after MaxRef validated the references against the pattern's
// capture count, so an out-of-range reference here is a programming
// error and reported as one.
func Substitute(s string, captures []string) (string, error) {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] != '$' {
			b.WriteByte(s[i])
			continue
		}
		n, next, err := parseRef(s, i)
		if err != nil {
			return "", err
		}
		if n == 0 { // "$$" — a literal dollar
			b.WriteByte('$')
		} else {
			if n > len(captures) {
				return "", fmt.Errorf("$%d has no capture group (the pattern declares %d)", n, len(captures))
			}
			b.WriteString(captures[n-1])
		}
		i = next - 1
	}
	return b.String(), nil
}

// parseRef reads the reference starting at s[i] == '$', returning its
// group number (0 for the "$$" escape) and the index just past it.
func parseRef(s string, i int) (int, int, error) {
	j := i + 1
	if j >= len(s) {
		return 0, 0, fmt.Errorf("%q ends with a bare `$`: write `$$` for a literal dollar sign", s)
	}
	if s[j] == '$' {
		return 0, j + 1, nil
	}
	braced := s[j] == '{'
	if braced {
		j++
	}
	if j >= len(s) || s[j] < '1' || s[j] > '9' {
		return 0, 0, fmt.Errorf("%q has a malformed capture reference: use $1…$9 (or ${1}), and `$$` for a literal dollar sign", s)
	}
	n := int(s[j] - '0')
	j++
	if braced {
		if j >= len(s) || s[j] != '}' {
			return 0, 0, fmt.Errorf("%q has an unclosed `${`", s)
		}
		j++
	}
	return n, j, nil
}
