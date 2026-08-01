package codegen

import (
	"fmt"
	"go/format"
	"sort"
	"strings"

	"github.com/moznion/sqletch/internal/diagnostics"
	"github.com/moznion/sqletch/internal/dialect"
	"github.com/moznion/sqletch/internal/template"
	"github.com/moznion/sqletch/runtime"
)

const runtimeImport = "github.com/moznion/sqletch/runtime"

// QueryInput is everything codegen needs for one query, produced by
// the earlier pipeline phases.
type QueryInput struct {
	Q          *template.QueryTemplate
	Frags      []runtime.Frag
	ParamTypes map[string]dialect.TypeRef // pinned types (premise P1)
	Columns    []dialect.ColumnDesc       // maximal Describe result
	Nullable   []bool                     // per column (P5)
}

type Options struct {
	Package string
}

// Generate emits the full package: db.go, querier.go, and one file per
// query. Returns file contents keyed by base name; any diagnostics
// mean the emission is incomplete and must not be written.
func Generate(opts Options, tm dialect.TypeMap, queries []QueryInput) (map[string][]byte, []diagnostics.Diagnostic) {
	files := map[string][]byte{}
	var diags []diagnostics.Diagnostic

	sorted := append([]QueryInput(nil), queries...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Q.Name < sorted[j].Q.Name })

	typeNames := map[string]string{} // generated type name -> query
	var querier []string

	for _, in := range sorted {
		g := &queryGen{in: in, tm: tm, pkg: opts.Package}
		src, sig, ds := g.emit(typeNames)
		diags = append(diags, ds...)
		if len(ds) > 0 {
			continue
		}
		files[pascalToSnake(in.Q.Name)+".sql.go"] = src
		querier = append(querier, sig)
	}

	if !diagnostics.HasErrors(diags) {
		files["db.go"] = []byte(dbFile(opts.Package))
		files["querier.go"] = []byte(querierFile(opts.Package, querier))
	}

	for name, src := range files {
		formatted, err := format.Source(src)
		if err != nil {
			diags = append(diags, diagnostics.Errorf(diagnostics.CodeNameCollision,
				diagnostics.Span{}, "internal: generated %s does not gofmt: %v", name, err))
			continue
		}
		files[name] = formatted
	}
	return files, diags
}

type queryGen struct {
	in      QueryInput
	tm      dialect.TypeMap
	pkg     string
	imports map[string]bool
	b       strings.Builder
}

type chooseMeta struct {
	c        *template.Choose
	enumName string
	numNamed int
}

type field struct{ name, typ, comment string }

func (g *queryGen) emit(typeNames map[string]string) ([]byte, string, []diagnostics.Diagnostic) {
	var diags []diagnostics.Diagnostic
	q := g.in.Q
	g.imports = map[string]bool{"context": true, runtimeImport: true}

	fail := func(code diagnostics.Code, span diagnostics.Span, format string, args ...any) {
		diags = append(diags, diagnostics.Errorf(code, span, format, args...))
	}
	claimType := func(name string) {
		if prev, ok := typeNames[name]; ok {
			fail(diagnostics.CodeNameCollision, q.HeaderSpan,
				"generated type %q collides between queries %s and %s", name, prev, q.Name)
			return
		}
		typeNames[name] = q.Name
	}

	// ---- choose metadata -------------------------------------------------
	var chooses []chooseMeta
	for _, it := range q.Items {
		if c, ok := it.(*template.Choose); ok {
			cm := chooseMeta{c: c, enumName: q.Name + GoName(c.Param), numNamed: len(c.Cases)}
			claimType(cm.enumName)
			chooses = append(chooses, cm)
		}
	}

	// ---- params struct ---------------------------------------------------
	paramsName := q.Name + "Params"
	claimType(paramsName)
	var paramFields []field
	fieldNames := map[string]string{}
	claimField := func(goName, src string) bool {
		if prev, ok := fieldNames[goName]; ok {
			fail(diagnostics.CodeNameCollision, q.HeaderSpan,
				"params field %q generated for both %q and %q; rename one", goName, prev, src)
			return false
		}
		fieldNames[goName] = src
		return true
	}
	for _, name := range q.ParamOrder {
		p := q.Params[name]
		tr, ok := g.in.ParamTypes[name]
		if !ok {
			fail(diagnostics.CodeUnsupportedType, paramSpanOf(q, name),
				"parameter %q has no resolved type (oracle did not see it)", name)
			continue
		}
		goType, tok := g.tm.GoType(tr.OID)
		if !tok {
			fail(diagnostics.CodeUnsupportedType, paramSpanOf(q, name),
				"no Go mapping for type %s (oid %d) of parameter %q; add an explicit cast to a supported type", tr.Name, tr.OID, name)
			continue
		}
		g.addImport(goType.Import)
		typ := goType.Name
		comment := ""
		if p.Optional {
			typ = "*" + typ
			comment = " // nil omits the guarded fragment(s)"
		}
		goName := GoName(name)
		if claimField(goName, name) {
			paramFields = append(paramFields, field{goName, typ, comment})
		}
	}
	for _, cm := range chooses {
		goName := GoName(cm.c.Param)
		if claimField(goName, "@choose("+cm.c.Param+")") {
			comment := " // required: zero value is an error"
			if cm.c.Default != nil {
				comment = " // zero value selects @default"
			}
			paramFields = append(paramFields, field{goName, cm.enumName, comment})
		}
	}

	// ---- row struct ------------------------------------------------------
	rowName := q.Name + "Row"
	hasRows := q.Annotation == template.AnnotationOne || q.Annotation == template.AnnotationMany
	var rowFields []field
	if hasRows {
		claimType(rowName)
		colNames := map[string]string{}
		for i, col := range g.in.Columns {
			goType, ok := g.tm.GoType(col.Type.OID)
			if !ok {
				fail(diagnostics.CodeUnsupportedType, q.HeaderSpan,
					"no Go mapping for type %s (oid %d) of result column %q; add an explicit cast to a supported type",
					col.Type.Name, col.Type.OID, col.Name)
				continue
			}
			g.addImport(goType.Import)
			typ := goType.Name
			if i < len(g.in.Nullable) && g.in.Nullable[i] {
				typ = "*" + typ
			}
			goName := GoName(col.Name)
			if prev, dup := colNames[goName]; dup {
				fail(diagnostics.CodeNameCollision, q.HeaderSpan,
					"row field %q generated for both columns %q and %q; alias one differently", goName, prev, col.Name)
				continue
			}
			colNames[goName] = col.Name
			rowFields = append(rowFields, field{goName, typ, ""})
		}
	}

	if diagnostics.HasErrors(diags) {
		return nil, "", diags
	}

	// ---- render ----------------------------------------------------------
	// Imports must be complete before the header is written: the choose
	// error path uses fmt.
	if len(chooses) > 0 {
		g.imports["fmt"] = true
	}
	w := &g.b
	fmt.Fprintf(w, "// Code generated by sqletch. DO NOT EDIT.\n\npackage %s\n\n", g.pkg)
	g.writeImports(w)

	for _, cm := range chooses {
		fmt.Fprintf(w, "type %s int\n\nconst (\n", cm.enumName)
		if cm.c.Default != nil {
			fmt.Fprintf(w, "\t%sDefault %s = iota\n", cm.enumName, cm.enumName)
		} else {
			fmt.Fprintf(w, "\t_ %s = iota // zero value is invalid: no @default declared\n", cm.enumName)
		}
		for _, cs := range cm.c.Cases {
			fmt.Fprintf(w, "\t%s%s\n", cm.enumName, GoName(cs.Name))
		}
		fmt.Fprint(w, ")\n\n")
	}

	fmt.Fprintf(w, "type %s struct {\n", paramsName)
	for _, f := range paramFields {
		fmt.Fprintf(w, "\t%s %s%s\n", f.name, f.typ, f.comment)
	}
	fmt.Fprint(w, "}\n\n")

	if hasRows {
		fmt.Fprintf(w, "type %s struct {\n", rowName)
		for _, f := range rowFields {
			fmt.Fprintf(w, "\t%s %s\n", f.name, f.typ)
		}
		fmt.Fprint(w, "}\n\n")
	}

	g.writeFragsVar(w)
	sig := g.writeFunc(w, paramsName, rowName, chooses, rowFields)

	return []byte(g.b.String()), sig, diags
}

func (g *queryGen) addImport(imp string) {
	if imp != "" {
		g.imports[imp] = true
	}
}

func (g *queryGen) writeImports(w *strings.Builder) {
	var std, ext []string
	for imp := range g.imports {
		if strings.Contains(imp, ".") {
			ext = append(ext, imp)
		} else {
			std = append(std, imp)
		}
	}
	sort.Strings(std)
	sort.Strings(ext)
	fmt.Fprint(w, "import (\n")
	for _, imp := range std {
		fmt.Fprintf(w, "\t%q\n", imp)
	}
	if len(ext) > 0 {
		fmt.Fprint(w, "\n")
		for _, imp := range ext {
			fmt.Fprintf(w, "\t%q\n", imp)
		}
	}
	fmt.Fprint(w, ")\n\n")
}

func (g *queryGen) writeFragsVar(w *strings.Builder) {
	fmt.Fprintf(w, "var %sFrags = []runtime.Frag{\n", lowerCamel(g.in.Q.Name))
	for _, f := range g.in.Frags {
		switch f.Kind {
		case runtime.Skel:
			fmt.Fprintf(w, "\t{Kind: runtime.Skel, Text: %q%s},\n", f.Text, spanLits(f.ParamSpans, f.ParamIdx))
		case runtime.Guarded:
			sep := "runtime.SepNone"
			if f.Sep == runtime.SepAnd {
				sep = "runtime.SepAnd"
			}
			fmt.Fprintf(w, "\t{Kind: runtime.Guarded, GuardMask: %#x, Sep: %s, Text: %q%s},\n",
				f.GuardMask, sep, f.Text, spanLits(f.ParamSpans, f.ParamIdx))
		case runtime.Choose:
			fmt.Fprint(w, "\t{Kind: runtime.Choose, Cases: []runtime.Case{\n")
			for _, c := range f.Cases {
				fmt.Fprintf(w, "\t\t{Text: %q%s},\n", c.Text, spanLits(c.ParamSpans, c.ParamIdx))
			}
			fmt.Fprint(w, "\t}},\n")
		}
	}
	fmt.Fprint(w, "}\n\n")
}

func spanLits(spans []runtime.Span, idx []int16) string {
	if len(spans) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString(", ParamSpans: []runtime.Span{")
	for i, s := range spans {
		if i > 0 {
			b.WriteString(", ")
		}
		fmt.Fprintf(&b, "{Start: %d, End: %d}", s.Start, s.End)
	}
	b.WriteString("}, ParamIdx: []int16{")
	for i, p := range idx {
		if i > 0 {
			b.WriteString(", ")
		}
		fmt.Fprintf(&b, "%d", p)
	}
	b.WriteString("}")
	return b.String()
}

func (g *queryGen) writeFunc(w *strings.Builder, paramsName, rowName string,
	chooses []chooseMeta, rowFields []field) string {

	q := g.in.Q
	fragsVar := lowerCamel(q.Name) + "Frags"

	var ret, zero string
	switch q.Annotation {
	case template.AnnotationMany:
		ret, zero = "([]"+rowName+", error)", "nil"
	case template.AnnotationOne:
		ret, zero = "("+rowName+", error)", "zero"
	case template.AnnotationExecRows:
		ret, zero = "(int64, error)", "0"
	default:
		ret, zero = "error", ""
	}
	sig := fmt.Sprintf("%s(ctx context.Context, arg %s) %s", q.Name, paramsName, ret)
	fmt.Fprintf(w, "func (q *Queries) %s {\n", sig)
	if q.Annotation == template.AnnotationOne {
		fmt.Fprintf(w, "\tvar zero %s\n", rowName)
	}

	errRet := func(errExpr string) string {
		switch q.Annotation {
		case template.AnnotationExec:
			return "return " + errExpr
		default:
			return "return " + zero + ", " + errExpr
		}
	}

	fmt.Fprint(w, "\tvar key runtime.ShapeKey\n")
	for i, atom := range q.GuardAtoms {
		fmt.Fprintf(w, "\tif arg.%s != nil {\n\t\tkey.Guards |= 1 << %d\n\t}\n", GoName(atom.Param), i)
	}
	if len(chooses) > 0 {
		g.imports["fmt"] = true
		var ords []string
		for i, cm := range chooses {
			ord := fmt.Sprintf("ord%d", i)
			ords = append(ords, ord)
			fmt.Fprintf(w, "\t%s, err := runtime.ChooseOrdinal(int(arg.%s), %d, %v)\n",
				ord, GoName(cm.c.Param), cm.numNamed, cm.c.Default != nil)
			fmt.Fprintf(w, "\tif err != nil {\n\t\t%s\n\t}\n", errRet(fmt.Sprintf("fmt.Errorf(%q, err)", q.Name+": %w")))
		}
		fmt.Fprintf(w, "\tkey.Choices = []uint8{%s}\n", strings.Join(ords, ", "))
	}

	fmt.Fprintf(w, "\tsqlText, argIdx := q.cache.Get(%q, %s, key)\n", q.Name, fragsVar)
	var vals []string
	for _, name := range q.ParamOrder {
		vals = append(vals, "arg."+GoName(name))
	}
	fmt.Fprintf(w, "\targs := runtime.BuildArgs(argIdx, []any{%s})\n", strings.Join(vals, ", "))
	fmt.Fprint(w, "\tq.hook(key.String(), sqlText)\n")

	scanList := func() string {
		var refs []string
		for _, f := range rowFields {
			refs = append(refs, "&i."+f.name)
		}
		return strings.Join(refs, ", ")
	}

	switch q.Annotation {
	case template.AnnotationMany:
		fmt.Fprint(w, "\trows, err := q.db.Query(ctx, sqlText, args...)\n")
		fmt.Fprintf(w, "\tif err != nil {\n\t\t%s\n\t}\n", errRet("err"))
		fmt.Fprint(w, "\tdefer rows.Close()\n")
		fmt.Fprintf(w, "\tvar items []%s\n", rowName)
		fmt.Fprint(w, "\tfor rows.Next() {\n")
		fmt.Fprintf(w, "\t\tvar i %s\n", rowName)
		fmt.Fprintf(w, "\t\tif err := rows.Scan(%s); err != nil {\n\t\t\treturn nil, err\n\t\t}\n", scanList())
		fmt.Fprint(w, "\t\titems = append(items, i)\n\t}\n")
		fmt.Fprint(w, "\treturn items, rows.Err()\n")
	case template.AnnotationOne:
		fmt.Fprint(w, "\trow := q.db.QueryRow(ctx, sqlText, args...)\n")
		fmt.Fprint(w, "\tvar i "+rowName+"\n")
		fmt.Fprintf(w, "\tif err := row.Scan(%s); err != nil {\n\t\treturn zero, err\n\t}\n", scanList())
		fmt.Fprint(w, "\treturn i, nil\n")
	case template.AnnotationExecRows:
		fmt.Fprint(w, "\ttag, err := q.db.Exec(ctx, sqlText, args...)\n")
		fmt.Fprint(w, "\tif err != nil {\n\t\treturn 0, err\n\t}\n")
		fmt.Fprint(w, "\treturn tag.RowsAffected(), nil\n")
	default: // exec
		fmt.Fprint(w, "\t_, err := q.db.Exec(ctx, sqlText, args...)\n")
		fmt.Fprint(w, "\treturn err\n")
	}
	fmt.Fprint(w, "}\n")
	return sig
}

func dbFile(pkg string) string {
	return fmt.Sprintf(`// Code generated by sqletch. DO NOT EDIT.

package %s

import (
	"context"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/moznion/sqletch/runtime"
)

// DBTX matches sqlc's pgx flavor: a pgx.Conn, pgxpool.Pool, or pgx.Tx
// satisfies it, so sqlc- and sqletch-generated code share transactions.
type DBTX interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

type Queries struct {
	db      DBTX
	cache   *runtime.ComposedCache
	onQuery func(shapeKey, sql string)
}

func New(db DBTX) *Queries {
	return &Queries{db: db, cache: runtime.NewComposedCache(256)}
}

func (q *Queries) WithTx(tx pgx.Tx) *Queries {
	return &Queries{db: tx, cache: q.cache, onQuery: q.onQuery}
}

// OnQuery installs an observability hook receiving the shape key and
// the composed SQL of every call.
func (q *Queries) OnQuery(fn func(shapeKey, sql string)) { q.onQuery = fn }

func (q *Queries) hook(shapeKey, sql string) {
	if q.onQuery != nil {
		q.onQuery(shapeKey, sql)
	}
}

// Ptr is a convenience for presence parameters: Ptr("x") yields *string.
func Ptr[T any](v T) *T { return &v }
`, pkg)
}

func querierFile(pkg string, sigs []string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "// Code generated by sqletch. DO NOT EDIT.\n\npackage %s\n\nimport (\n\t\"context\"\n)\n\n", pkg)
	fmt.Fprint(&b, "// Querier lets user code mock the generated queries.\ntype Querier interface {\n")
	for _, s := range sigs {
		fmt.Fprintf(&b, "\t%s\n", s)
	}
	fmt.Fprint(&b, "}\n\nvar _ Querier = (*Queries)(nil)\n")
	return b.String()
}

func paramSpanOf(q *template.QueryTemplate, name string) diagnostics.Span {
	if p := q.Params[name]; p != nil && len(p.Occurrences) > 0 {
		return p.Occurrences[0].Span
	}
	return q.HeaderSpan
}

func pascalToSnake(name string) string {
	var b strings.Builder
	for i, r := range name {
		if i > 0 && r >= 'A' && r <= 'Z' {
			b.WriteByte('_')
		}
		b.WriteRune(r)
	}
	return strings.ToLower(b.String())
}
