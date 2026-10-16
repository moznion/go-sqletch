package dialect

import "github.com/moznion/go-sqletch/internal/cache"

// EntryFromDesc converts a Describe answer into its committed-cache
// form. This is the single Desc→entry conversion: the pipeline and
// the oracle corpus harness must serialize identically, or cache
// byte-identity across oracle backends (design 15 §3) stops being
// checkable.
func EntryFromDesc(fp, renderedSQL string, d Desc) *cache.OracleEntry {
	e := &cache.OracleEntry{SchemaFP: fp, RenderedSQL: renderedSQL}
	for _, p := range d.Params {
		et := cache.EntryType{OID: p.OID, Name: p.Name}
		if p.Ext != nil {
			et.Schema, et.Kind, et.Labels = p.Ext.Schema, p.Ext.Kind, p.Ext.Labels
		}
		e.Params = append(e.Params, et)
	}
	for _, c := range d.Columns {
		ec := cache.EntryColumn{
			Name: c.Name, OID: c.Type.OID, TypeName: c.Type.Name,
			SrcRel: c.SrcRel, SrcAtt: c.SrcAtt,
		}
		if x := c.Type.Ext; x != nil {
			ec.TypeSchema, ec.TypeKind, ec.TypeLabels = x.Schema, x.Kind, x.Labels
		}
		e.Columns = append(e.Columns, ec)
	}
	return e
}

// DescFromEntry is EntryFromDesc's inverse, used on cache hits.
func DescFromEntry(e *cache.OracleEntry) Desc {
	var d Desc
	for _, p := range e.Params {
		d.Params = append(d.Params, TypeRef{OID: p.OID, Name: p.Name, Ext: typeExt(p.Schema, p.Kind, p.Labels)})
	}
	for _, c := range e.Columns {
		d.Columns = append(d.Columns, ColumnDesc{
			Name:   c.Name,
			Type:   TypeRef{OID: c.OID, Name: c.TypeName, Ext: typeExt(c.TypeSchema, c.TypeKind, c.TypeLabels)},
			SrcRel: c.SrcRel, SrcAtt: c.SrcAtt,
		})
	}
	return d
}

// typeExt rebuilds a TypeRef.Ext from its entry fields; an entry with no
// kind recorded has none (Kind is set for every described user-defined
// type, so it alone decides presence).
func typeExt(schema, kind string, labels []string) *TypeExt {
	if kind == "" {
		return nil
	}
	return &TypeExt{Schema: schema, Kind: kind, Labels: labels}
}
