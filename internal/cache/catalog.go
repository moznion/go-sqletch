// Package cache holds the committed oracle cache and the catalog
// snapshot model. In P3 only the catalog data model is needed (the
// resolver and star expansion consume it); the oracle fills it from a
// live database in P4. See docs/design/04-type-oracle.md §3.
package cache

// Catalog is an offline snapshot of the schema portions sqletch needs:
// relation and column existence, types, NOT NULL, and defaults.
type Catalog struct {
	SchemaFP string  `json:"schema_fp"`
	Tables   []Table `json:"tables"`
}

type Table struct {
	Schema string   `json:"schema"`
	Name   string   `json:"name"`
	OID    uint32   `json:"oid"`
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
