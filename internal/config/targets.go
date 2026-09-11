package config

import (
	"fmt"
	"go/token"
	"path/filepath"
	"sort"
	"strings"

	"github.com/moznion/go-sqletch/internal/diagnostics"
	"github.com/moznion/go-sqletch/internal/pathpat"
)

// ResolvedTarget is one generated Go package: the files that feed it
// and where it is written. It is what `targets` resolves to once
// patterns are expanded and captures substituted
// (docs/design/19-multi-target-output.md §4).
type ResolvedTarget struct {
	// Package is the generated package name (captures substituted).
	Package string
	// Path is the output directory as written in the config (captures
	// substituted): project-relative and slash-separated unless the
	// user spelled an absolute path. Use Config.Abs for filesystem
	// access and Slug for a derived-output namespace.
	Path string
	// Files are the template files, absolute and sorted.
	Files []string
}

// Abs is the target's output directory as a filesystem path.
func (t ResolvedTarget) Abs(c Config) string { return c.Abs(filepath.FromSlash(t.Path)) }

// Slug is the target's identity inside the derived-output trees
// (.sqletch/explain/<slug>/…, .sqletch/expanded/<slug>/…). It mirrors
// the output path so the tree is readable, with the escapes a path
// component cannot carry folded away — an absolute or climbing path
// (only reachable via the absolute-output warning) must not send a
// derived write out of .sqletch/.
func (t ResolvedTarget) Slug() string {
	slug := filepath.ToSlash(t.Path)
	slug = strings.TrimPrefix(slug, "/")
	parts := strings.Split(slug, "/")
	out := parts[:0]
	for _, p := range parts {
		switch p {
		case "", ".", "..":
			p = "_"
		}
		p = strings.ReplaceAll(p, ":", "_")
		out = append(out, p)
	}
	slug = strings.Join(out, "/")
	if slug == "" {
		slug = "_"
	}
	return slug
}

// Resolution is one expansion of `targets` against the project
// directory.
type Resolution struct {
	// Targets are ordered by output path, and each target's Files are
	// sorted: determinism is never conditional on directory order.
	Targets []ResolvedTarget
	// Files is the union of every target's files, sorted. It is what
	// the whole-workspace commands (fmt, explain) and the LSP iterate.
	Files []string
	// Dirs are the directories the expansion consulted, absolute. A
	// directory's mtime moves when an entry is added or removed, so
	// re-stating these is a sound memo signature for the LSP (§6).
	Dirs []string
}

// validateTarget checks one `targets` entry without touching the
// filesystem: shape, pattern syntax, and that every `$n` the output
// references exists in EVERY pattern of the entry (so the entry's
// grouping is well defined however it expands). A `$`-free output is
// additionally path/identifier-checked here, which keeps the common
// config failing early — the substituted forms are checked in
// ResolveTargets, where they first exist.
func (c Config) validateTarget(span diagnostics.Span, i int, t Target) []diagnostics.Diagnostic {
	var diags []diagnostics.Diagnostic
	invalid := func(format string, args ...any) {
		diags = append(diags, diagnostics.Errorf(diagnostics.CodeConfigInvalid, span, format, args...))
	}
	id := fmt.Sprintf("targets[%d]", i)
	if len(t.Queries) == 0 {
		invalid("%s.queries is required (patterns of template .sql/.go files)", id)
	}
	if t.Output.Package == "" {
		invalid("%s.output.package is required", id)
	}
	if t.Output.Path == "" {
		invalid("%s.output.path is required", id)
	}

	need := 0
	for field, s := range map[string]string{"path": t.Output.Path, "package": t.Output.Package} {
		n, err := pathpat.MaxRef(s)
		if err != nil {
			invalid("%s.output.%s: %v", id, field, err)
			continue
		}
		if n > need {
			need = n
		}
	}
	for _, pat := range t.Queries {
		p, err := pathpat.Compile(pat)
		if err != nil {
			invalid("%s.queries: %v", id, err)
			continue
		}
		if p.NumCaptures() < need {
			invalid("%s.output references $%d but the pattern %q declares %d capture group(s): every pattern of a target must supply every capture its output substitutes, or the target's grouping would depend on which pattern matched",
				id, need, pat, p.NumCaptures())
		}
	}

	if !t.Output.HasCaptureRefs() {
		diags = append(diags, c.pathEscapeDiags(id+".output.path", t.Output.Path, true)...)
		if d, bad := packageDiag(span, id, t.Output.Package); bad {
			diags = append(diags, d)
		}
	}
	return diags
}

// packageDiag refuses an output package name that is not a plain Go
// identifier. The name is emitted verbatim as `package <name>`, so an
// unchecked value is text spliced into every generated file of the
// package — a typo makes gofmt fail with no span to point at, and a
// crafted one would be an injection point.
func packageDiag(span diagnostics.Span, id, pkg string) (diagnostics.Diagnostic, bool) {
	if token.IsIdentifier(pkg) && !token.Lookup(pkg).IsKeyword() && pkg != "_" {
		return diagnostics.Diagnostic{}, false
	}
	return diagnostics.Errorf(diagnostics.CodeConfigInvalid, span,
		"%s.output.package %q is not a Go package name: it is emitted verbatim as `package %s`",
		id, pkg, pkg).
		WithHint("use a lowercase Go identifier, e.g. gen"), true
}

// ResolveTargets expands every target's patterns against the project
// directory. It is the single resolution seam: pipeline.Run and the
// LSP's OfflineChecker both call it, so the two can never disagree
// about which file belongs to which generated package.
//
// A pattern matching nothing is a WARNING, not an error (design 19
// §4): in a monorepo a captured directory may not exist yet, and a
// target with no files simply emits nothing. `schema.files` keeps the
// stricter rule — an empty schema silently fingerprints nothing.
func (c Config) ResolveTargets() (Resolution, []diagnostics.Diagnostic) {
	span := diagnostics.Span{File: c.Path}
	var diags []diagnostics.Diagnostic

	type group struct {
		pkg   string
		path  string
		files []string
		seen  map[string]bool
	}
	groups := map[string]*group{}
	owner := map[string]string{} // file -> output path that claimed it
	collided := map[string]bool{}
	// rejected remembers an output that failed its post-substitution
	// checks, so a bad capture is reported ONCE and not once per file
	// that expands to it.
	rejected := map[string]bool{}
	dirSeen := map[string]bool{}
	var dirs []string

	for i, t := range c.Targets {
		id := fmt.Sprintf("targets[%d]", i)
		for _, pat := range t.Queries {
			p, err := pathpat.Compile(pat)
			if err != nil {
				continue // already reported by validateTarget
			}
			res, err := p.Walk(c.Dir)
			if err != nil {
				diags = append(diags, diagnostics.Errorf(diagnostics.CodeConfigInvalid, span,
					"%s.queries: %v", id, err))
				continue
			}
			for _, d := range res.Dirs {
				abs := c.Abs(filepath.FromSlash(d))
				if !dirSeen[abs] {
					dirSeen[abs] = true
					dirs = append(dirs, abs)
				}
			}
			if len(res.Matches) == 0 {
				diags = append(diags, diagnostics.Warnf(diagnostics.CodeTargetNoMatch, span,
					"%s.queries pattern %q matched no file", id, pat).
					WithHint("check the pattern for a typo; a target whose patterns match nothing generates no package"))
				continue
			}
			for _, m := range res.Matches {
				abs := c.Abs(filepath.FromSlash(m.Path))
				if msg, escapes := c.matchEscapes(pat, abs); escapes {
					diags = append(diags, diagnostics.Errorf(diagnostics.CodePathEscape, span, "%s.queries: %s", id, msg))
					continue
				}
				outPath, err := pathpat.Substitute(t.Output.Path, m.Captures)
				if err != nil {
					diags = append(diags, diagnostics.Errorf(diagnostics.CodeConfigInvalid, span,
						"%s.output.path: %v", id, err))
					continue
				}
				pkg, err := pathpat.Substitute(t.Output.Package, m.Captures)
				if err != nil {
					diags = append(diags, diagnostics.Errorf(diagnostics.CodeConfigInvalid, span,
						"%s.output.package: %v", id, err))
					continue
				}
				if prev, ok := owner[abs]; ok && prev != outPath {
					if !collided[abs] {
						collided[abs] = true
						diags = append(diags, diagnostics.Errorf(diagnostics.CodeTargetFileOverlap, span,
							"template file %q is claimed by two targets (output %q and %q): a query belongs to exactly one generated package",
							c.rel(abs), prev, outPath).
							WithHint("narrow one target's patterns so each template file matches exactly one target"))
					}
					continue
				}
				if rejected[outPath] {
					continue
				}
				g := groups[outPath]
				if g == nil {
					// The substituted forms are what sqletch actually
					// writes, so they get the checks a `$`-free output
					// already got at load.
					if t.Output.HasCaptureRefs() {
						if ds := c.pathEscapeDiags(id+".output.path", outPath, true); len(ds) > 0 {
							diags = append(diags, ds...)
							if diagnostics.HasErrors(ds) {
								rejected[outPath] = true
								continue
							}
						}
						if d, bad := packageDiag(span, id, pkg); bad {
							diags = append(diags, d)
							rejected[outPath] = true
							continue
						}
					}
					g = &group{pkg: pkg, path: outPath, seen: map[string]bool{}}
					groups[outPath] = g
				}
				if g.pkg != pkg {
					if !collided[outPath] {
						collided[outPath] = true
						diags = append(diags, diagnostics.Errorf(diagnostics.CodeTargetCollision, span,
							"two targets generate into %q with different package names (%q and %q): one directory is one Go package",
							outPath, g.pkg, pkg).
							WithHint("give the two targets distinct output paths, or the same package name"))
					}
					continue
				}
				owner[abs] = outPath
				if !g.seen[abs] {
					g.seen[abs] = true
					g.files = append(g.files, abs)
				}
			}
		}
	}

	var res Resolution
	paths := make([]string, 0, len(groups))
	for p := range groups {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	for _, p := range paths {
		g := groups[p]
		sort.Strings(g.files)
		res.Targets = append(res.Targets, ResolvedTarget{Package: g.pkg, Path: g.path, Files: g.files})
		res.Files = append(res.Files, g.files...)
	}
	sort.Strings(res.Files)
	sort.Strings(dirs)
	res.Dirs = dirs
	return res, diags
}

// rel spells an absolute path relative to the project directory for a
// diagnostic message, falling back to the absolute form.
func (c Config) rel(abs string) string {
	if r, err := filepath.Rel(c.Dir, abs); err == nil {
		return filepath.ToSlash(r)
	}
	return abs
}

// legacyQueriesHint renders the old `queries` list back into the
// migration hint so the user can paste the rewrite.
func legacyQueriesHint(queries []string) string {
	if len(queries) == 0 {
		return "queries/*.sql"
	}
	return strings.Join(queries, ", ")
}

func legacyOutputHint(o *Output, dflt string) string {
	if o == nil || o.Package == "" {
		return dflt
	}
	return o.Package
}

func legacyOutputPathHint(o *Output, dflt string) string {
	if o == nil || o.Path == "" {
		return dflt
	}
	return o.Path
}
