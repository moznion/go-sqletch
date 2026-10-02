package rules

import (
	"github.com/moznion/go-sqletch/internal/ast"
	"github.com/moznion/go-sqletch/internal/cache"
	"github.com/moznion/go-sqletch/internal/dialect"
	"github.com/moznion/go-sqletch/internal/template"
)

// ColumnResolver binds a bare column reference against a statement's
// top-level relations with the same resolver R3 uses (dialect-correct
// name folding, catalog lookup). It is exported for internal/lint
// (SQLETCHL005), so the lint never forks name resolution.
type ColumnResolver struct {
	res *resolver
}

// NewColumnResolver builds a resolver over the maximal rendering's
// top-level relations.
func NewColumnResolver(profile dialect.LexerProfile, q *template.QueryTemplate, maxR ast.Rendering,
	maxTree dialect.Tree, cat *cache.Catalog) *ColumnResolver {
	return &ColumnResolver{res: newResolver(profile, q, maxR, maxTree, cat)}
}

// Column returns the catalog column `qualifier.name`. An empty
// qualifier means an unqualified reference, which must match exactly
// one relation's catalog columns. nil when unresolved.
func (c *ColumnResolver) Column(qualifier, name string) *cache.Column {
	var rel *relInfo
	if qualifier == "" {
		cands := c.res.columnCandidates(name)
		if len(cands) != 1 {
			return nil
		}
		rel = cands[0]
	} else {
		rel = c.res.byName[c.res.fold(qualifier)]
	}
	if rel == nil || rel.table == nil {
		return nil
	}
	return c.res.col(rel.table, name)
}
