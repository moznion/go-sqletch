package cache

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestTableIsViewOmitEmpty pins the byte-identity guarantee: a base
// table (IsView false) must marshal WITHOUT an is_view member, so
// PostgreSQL/MySQL catalogs — which never set it — stay byte-identical
// to before the field existed. A view carries is_view:true.
func TestTableIsViewOmitEmpty(t *testing.T) {
	base, err := json.Marshal(Table{Schema: "main", Name: "users", OID: 1})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(base), "is_view") {
		t.Errorf("base table must omit is_view, got %s", base)
	}
	view, err := json.Marshal(Table{Schema: "main", Name: "v", OID: 2, IsView: true})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(view), `"is_view":true`) {
		t.Errorf("view must carry is_view:true, got %s", view)
	}
}

func TestCatalogLookup(t *testing.T) {
	cat := &Catalog{Tables: []Table{
		{Schema: "audit", Name: "users", OID: 2},
		{Schema: "public", Name: "users", OID: 1, Cols: []Column{
			{Name: "id", Att: 1, NotNull: true},
			{Name: "email", Att: 2},
		}},
	}}

	u := cat.Lookup("users")
	if u == nil || u.Schema != "public" {
		t.Fatalf("Lookup must prefer public schema, got %+v", u)
	}
	if cat.Lookup("missing") != nil {
		t.Error("Lookup(missing) must be nil")
	}
	if cat.LookupOID(2) == nil || cat.LookupOID(2).Schema != "audit" {
		t.Error("LookupOID(2) must find the audit table")
	}
	if cat.LookupOID(99) != nil {
		t.Error("LookupOID(99) must be nil")
	}

	if c := u.Col("id"); c == nil || !c.NotNull {
		t.Errorf("Col(id) = %+v", c)
	}
	if u.Col("nope") != nil {
		t.Error("Col(nope) must be nil")
	}
	if c := u.ColByAtt(2); c == nil || c.Name != "email" {
		t.Errorf("ColByAtt(2) = %+v", c)
	}
}

func TestCatalogLookup_NonPublicOnly(t *testing.T) {
	cat := &Catalog{Tables: []Table{{Schema: "app", Name: "logs", OID: 3}}}
	if l := cat.Lookup("logs"); l == nil || l.Schema != "app" {
		t.Fatalf("Lookup must fall back to non-public schema, got %+v", l)
	}
}

func TestCatalogLookupQualified(t *testing.T) {
	cat := &Catalog{Tables: []Table{
		{Schema: "public", Name: "orgs", OID: 1},
		{Schema: "aux", Name: "orgs", OID: 2},
	}}
	if l := cat.LookupQualified("aux", "orgs"); l == nil || l.OID != 2 {
		t.Fatalf("qualified lookup = %+v, want aux.orgs", l)
	}
	if l := cat.LookupQualified("", "orgs"); l == nil || l.OID != 1 {
		t.Fatalf("unqualified lookup = %+v, want public preference", l)
	}
	// An explicit qualifier never falls back to a same-named table of
	// another schema.
	if l := cat.LookupQualified("missing", "orgs"); l != nil {
		t.Fatalf("qualified lookup of absent schema = %+v, want nil", l)
	}
}
