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
		e.Params = append(e.Params, cache.EntryType{OID: p.OID, Name: p.Name})
	}
	for _, c := range d.Columns {
		e.Columns = append(e.Columns, cache.EntryColumn{
			Name: c.Name, OID: c.Type.OID, TypeName: c.Type.Name,
			SrcRel: c.SrcRel, SrcAtt: c.SrcAtt,
		})
	}
	return e
}

// DescFromEntry is EntryFromDesc's inverse, used on cache hits.
func DescFromEntry(e *cache.OracleEntry) Desc {
	var d Desc
	for _, p := range e.Params {
		d.Params = append(d.Params, TypeRef{OID: p.OID, Name: p.Name})
	}
	for _, c := range e.Columns {
		d.Columns = append(d.Columns, ColumnDesc{
			Name: c.Name, Type: TypeRef{OID: c.OID, Name: c.TypeName},
			SrcRel: c.SrcRel, SrcAtt: c.SrcAtt,
		})
	}
	return d
}
