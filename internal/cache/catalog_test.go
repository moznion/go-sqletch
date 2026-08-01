package cache

import "testing"

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
