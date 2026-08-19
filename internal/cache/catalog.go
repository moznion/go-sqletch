// Package cache holds the committed oracle cache and the catalog
// snapshot model. In P3 only the catalog data model is needed (the
// resolver and star expansion consume it); the oracle fills it from a
// live database in P4. See docs/design/04-type-oracle.md §3.
package cache

// Catalog is an offline snapshot of the schema portions sqletch needs:
// relation and column existence, types, NOT NULL, and defaults.
type Catalog struct {
	Format   int     `json:"format"`
	SchemaFP string  `json:"schema_fp"`
	Tables   []Table `json:"tables"`
}

type Table struct {
	Schema string `json:"schema"`
	Name   string `json:"name"`
	OID    uint32 `json:"oid"`
	// HasChildren marks a plain-inheritance parent (PostgreSQL:
	// relhassubclass on relkind 'r'). Children may DROP an inherited
	// NOT NULL (proven on PG 16), so a parent scan can return NULL
	// where attnotnull says otherwise — the analyzer must not narrow
	// such tables unless the reference is `FROM ONLY`. Partitioned
	// parents ('p') are exempt: partitions cannot drop inherited NOT
	// NULL (42P16). omitempty keeps every inheritance-free catalog
	// byte-identical.
	HasChildren bool `json:"has_children,omitempty"`
	// IsView marks a relation that is a VIEW rather than a base table.
	// SQLite's column-origin attribution (sqlite3_column_origin_name)
	// resolves a view's result columns THROUGH to the view's base
	// tables, whose declared NOT NULL the view's (invisible, possibly
	// null-extending) body need not preserve — so a base table appearing
	// directly in FROM must not be allowed to vouch for a column that
	// actually flows through a view. The nullability analyzer treats any
	// view in play as a wholesale narrowing kill-switch. PostgreSQL and
	// MySQL report the view's own identity (never the base table) and so
	// never set this; omitempty keeps their catalogs byte-identical.
	IsView bool     `json:"is_view,omitempty"`
	Cols   []Column `json:"cols"`
}

type Column struct {
	Name       string `json:"name"`
	Att        int16  `json:"att"`
	TypeOID    uint32 `json:"type_oid"`
	TypeName   string `json:"type_name"`
	NotNull    bool   `json:"not_null"`
	HasDefault bool   `json:"has_default"`
}

// Lookup finds a table by unqualified name, preferring the "public"
// schema on ties. Returns nil when absent.
func (c *Catalog) Lookup(name string) *Table {
	var found *Table
	for i := range c.Tables {
		t := &c.Tables[i]
		if t.Name != name {
			continue
		}
		if t.Schema == "public" {
			return t
		}
		if found == nil {
			found = t
		}
	}
	return found
}

// LookupQualified finds a table by an explicit schema qualifier, or
// falls back to Lookup's unqualified resolution when schema is empty.
// An explicitly qualified name never falls back: resolving it to a
// same-named table of another schema is exactly the confusion the
// nullability analysis must not inherit.
func (c *Catalog) LookupQualified(schema, name string) *Table {
	if schema == "" {
		return c.Lookup(name)
	}
	for i := range c.Tables {
		t := &c.Tables[i]
		if t.Schema == schema && t.Name == name {
			return t
		}
	}
	return nil
}

// LookupOID finds a table by OID. Returns nil when absent.
func (c *Catalog) LookupOID(oid uint32) *Table {
	for i := range c.Tables {
		if c.Tables[i].OID == oid {
			return &c.Tables[i]
		}
	}
	return nil
}

func (t *Table) Col(name string) *Column {
	for i := range t.Cols {
		if t.Cols[i].Name == name {
			return &t.Cols[i]
		}
	}
	return nil
}

func (t *Table) ColByAtt(att int16) *Column {
	for i := range t.Cols {
		if t.Cols[i].Att == att {
			return &t.Cols[i]
		}
	}
	return nil
}
