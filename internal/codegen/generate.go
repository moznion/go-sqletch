package codegen

import (
	"fmt"
	"go/format"
	"sort"
	"strings"
	"unicode"

	"github.com/moznion/go-sqletch/internal/diagnostics"
	"github.com/moznion/go-sqletch/internal/dialect"
	"github.com/moznion/go-sqletch/internal/template"
	"github.com/moznion/go-sqletch/runtime"
)

const runtimeImport = "github.com/moznion/go-sqletch/runtime"

// optionalImport is the Option[T] package generated code represents
// absence with: None instead of a nil pointer, for guarded parameters,
// nullable result columns, and :maybe-one results alike.
const optionalImport = "github.com/moznion/go-optional"

const pgxImport = "github.com/jackc/pgx/v5"

// QueryInput is everything codegen needs for one query, produced by
// the earlier pipeline phases.
type QueryInput struct {
	Q          *template.QueryTemplate
	Frags      []runtime.Frag
	ParamTypes map[string]dialect.TypeRef // pinned types (premise P1)
	Columns    []dialect.ColumnDesc       // maximal Describe result
	Nullable   []bool                     // per column (P5)
	// ExpandedShapes, when non-nil, switches the query to strict static
	// expansion: keys are canonical shape-key strings, precomputed by
	// the pipeline via runtime.Compose. The function dispatches by
	// lookup instead of composing.
	ExpandedShapes map[string]runtime.Expanded
}

type Options struct {
	Package string
	// TreeCaps bounds @filter-tree values; zero fields fall back to
	// runtime.DefaultTreeCaps.
	TreeCaps runtime.TreeCaps
	// Style is the dialect's placeholder style; it selects the driver
	// flavor of the generated code (StyleDollar → pgx, StyleQuestion →
	// database/sql) and the composition entry points.
	Style runtime.Style
}

// Generate emits the full package: db.gen.go, querier.gen.go, and one
// <query>.sql.gen.go per query — every emitted name ends in ".gen.go"
// so generated files are recognizable by name, not only by the
// "Code generated" header. Returns file contents keyed by base name;
// any diagnostics mean the emission is incomplete and must not be
// written.
func Generate(opts Options, tm dialect.TypeMap, queries []QueryInput) (map[string][]byte, []diagnostics.Diagnostic) {
	files := map[string][]byte{}
	var diags []diagnostics.Diagnostic

	sorted := append([]QueryInput(nil), queries...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Q.Name < sorted[j].Q.Name })

	caps := opts.TreeCaps
	if caps.MaxNodes == 0 {
		caps.MaxNodes = runtime.DefaultTreeCaps.MaxNodes
	}
	if caps.MaxDepth == 0 {
		caps.MaxDepth = runtime.DefaultTreeCaps.MaxDepth
	}

	typeNames := map[string]string{}        // generated type name -> query
	fileStems := map[string]string{}        // generated file stem -> query
	policyTypes := map[string]*policyType{} // named type -> policy parameter
	var querier []string
	sigImports := map[string]bool{}

	for _, in := range sorted {
		g := &queryGen{in: in, tm: tm, pkg: opts.Package, caps: caps, style: opts.Style,
			sigImports: map[string]bool{}}
		src, sig, ds := g.emit(typeNames, policyTypes)
		for imp := range g.sigImports {
			sigImports[imp] = true
		}
		diags = append(diags, ds...)
		if len(ds) > 0 {
			continue
		}
		// Distinct query names can share a file stem (UserID / UserId);
		// without this the later query would silently overwrite the
		// earlier one's file.
		stem := pascalToSnake(in.Q.Name)
		if prev, ok := fileStems[stem]; ok {
			diags = append(diags, diagnostics.Errorf(diagnostics.CodeNameCollision, in.Q.HeaderSpan,
				"query %q and %q both generate %s.sql.gen.go; rename one so the file names differ",
				prev, in.Q.Name, stem))
			continue
		}
		fileStems[stem] = in.Q.Name
		files[stem+".sql.gen.go"] = src
		querier = append(querier, sig)
	}

	// A policy type is declared once for the package, so a per-query
	// claimType cannot see it; the cross-check runs here, after every
	// query has claimed its own names (sorted for determinism).
	for _, tn := range sortedKeys(policyTypes) {
		if q, ok := typeNames[tn]; ok {
			pt := policyTypes[tn]
			diags = append(diags, diagnostics.Errorf(diagnostics.CodeNameCollision, pt.span,
				"generated type %q collides between query %s and policy parameter %q; rename one",
				tn, q, pt.param))
		}
	}

	if !diagnostics.HasErrors(diags) {
		// hookTree is only emitted for packages that actually have a
		// @filter-tree query; emitting it always would leave an unused
		// unexported method for every other package to trip a linter on.
		treeHook := ""
		if anyFilterTree(sorted) {
			treeHook = treeHookFunc
		}
		if opts.Style == runtime.StyleQuestion {
			files["db.gen.go"] = []byte(dbFileQuestion(opts.Package, treeHook))
		} else {
			files["db.gen.go"] = []byte(dbFile(opts.Package, treeHook))
		}
		files["querier.gen.go"] = []byte(querierFile(opts.Package, querier, sigImports))
		if len(policyTypes) > 0 {
			files["policy.gen.go"] = []byte(policyFile(opts.Package, policyTypes))
		}
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
	// sigImports are the imports the query's *signature* needs, as
	// opposed to its body: required arguments put types into the
	// signature, and querier.go repeats those signatures.
	sigImports map[string]bool
	in         QueryInput
	tm         dialect.TypeMap
	pkg        string
	caps       runtime.TreeCaps
	style      runtime.Style
	imports    map[string]bool
	b          strings.Builder
}

type chooseMeta struct {
	c        *template.Choose
	enumName string
	numNamed int
}

type orderMeta struct {
	o        *template.OrderBy
	typeName string
}

// field is one generated struct field. optElem, when non-empty, marks
// a nullable result column: typ is optional.Option[optElem] and the
// scan path goes through a *optElem temporary (see writeFunc).
type field struct{ name, typ, comment, optElem string }

func (g *queryGen) emit(typeNames map[string]string, policyTypes map[string]*policyType) ([]byte, string, []diagnostics.Diagnostic) {
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

	// ---- choose / order-by / filter-tree / @in metadata ------------------
	var chooses []chooseMeta
	var orders []orderMeta
	var filter *template.FilterTree
	var ins []*template.InExpr
	inParams := map[string]bool{}
	for _, it := range q.Items {
		switch c := it.(type) {
		case *template.Choose:
			cm := chooseMeta{c: c, enumName: q.Name + GoName(c.Param), numNamed: len(c.Cases)}
			claimType(cm.enumName)
			chooses = append(chooses, cm)
		case *template.OrderBy:
			om := orderMeta{o: c, typeName: q.Name + GoName(c.Param) + "Key"}
			claimType(om.typeName)
			orders = append(orders, om)
		case *template.InExpr:
			ins = append(ins, c)
			inParams[c.Param] = true
		case *template.FilterTree:
			filter = c
			claimType(q.Name + "Unscoped")
			for _, pr := range c.Predicates {
				claimType(q.Name + GoName(pr.Name))
			}
		}
	}

	// ---- params struct ---------------------------------------------------
	paramsName := q.Name + "Params"
	claimType(paramsName)
	var paramFields []field
	// Values the caller must not be able to omit. They leave the params
	// struct and become arguments; see requiredArg.
	var requiredArgs []requiredArg
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
		if filter != nil && name == filter.Param {
			continue // the tree field is appended below
		}
		if filterOnlyParam(p) {
			continue // predicate constructor argument, not a field
		}
		var typ, comment, typImport string
		tr, ok := g.in.ParamTypes[name]
		switch {
		case ok:
			goType, tok := g.tm.GoType(tr.OID)
			if !tok {
				fail(diagnostics.CodeUnsupportedType, paramSpanOf(q, name),
					"no Go mapping for type %s (oid %d) of parameter %q; add an explicit cast to a supported type", tr.Name, tr.OID, name)
				continue
			}
			g.addImport(goType.Import)
			typImport = goType.Import
			typ = goType.Name
			if g.style == runtime.StyleQuestion && inParams[name] {
				// The annotation gives the ELEMENT type on expanding
				// dialects; the parameter is a slice of it.
				typ = "[]" + typ
				comment = " // @in list; empty matches nothing"
			}
			if p.Optional {
				typ = "optional.Option[" + typ + "]"
				comment = " // None omits the guarded fragment(s)"
				g.addImport(optionalImport)
			}
		default:
			// A parameter the oracle never saw: legal only as a pure
			// @when control parameter, typed by its literal (R9).
			litType, isControl := valueAtomGoType(q, name)
			if !isControl {
				fail(diagnostics.CodeUnsupportedType, paramSpanOf(q, name),
					"parameter %q has no resolved type (oracle did not see it)", name)
				continue
			}
			typ = litType
			comment = " // @when control parameter"
		}
		if p.Policy != "" {
			// A policy wove this parameter in; the query author never
			// wrote it. Making it a required argument is what keeps the
			// scoping guarantee from evaporating at the Go boundary: a
			// params-struct field omitted from a keyed literal compiles
			// and sends the zero value, so the woven predicate silently
			// matches nothing instead of scoping the read.
			//
			// The argument's type is a distinct named type declared once
			// for the package (policy.gen.go), shared by every query the
			// policy touches: two same-typed policy arguments cannot be
			// swapped at a call site, because the wrong order is a type
			// mismatch. The named type never reaches the driver — the
			// bind site converts back to the underlying type.
			tn := GoName(name)
			switch tn {
			case "Queries", "Querier", "DBTX", "New":
				// db.gen.go / querier.gen.go declare these; the collision
				// would otherwise surface as generated code that does not
				// compile in the user's module.
				fail(diagnostics.CodeNameCollision, paramSpanOf(q, name),
					"policy parameter %q would generate type %q, which the generated package already declares; rename the parameter", name, tn)
				continue
			}
			if pt, ok := policyTypes[tn]; ok {
				if pt.underlying != typ || pt.param != name {
					fail(diagnostics.CodeNameCollision, paramSpanOf(q, name),
						"policy type %q is %s for parameter %q of query %s but %s for parameter %q of query %s; align the policies' declared types or rename a parameter",
						tn, pt.underlying, pt.param, pt.query, typ, name, q.Name)
					continue
				}
				pt.policies[p.Policy] = true
			} else {
				policyTypes[tn] = &policyType{
					underlying: typ,
					imp:        typImport,
					param:      name,
					policies:   map[string]bool{p.Policy: true},
					query:      q.Name,
					span:       paramSpanOf(q, name),
				}
			}
			requiredArgs = append(requiredArgs, requiredArg{
				name:  argIdent(name),
				typ:   tn,
				expr:  convertExpr(typ, argIdent(name)),
				doc:   fmt.Sprintf("policy %s", p.Policy),
				param: name,
			})
			continue
		}
		goName := GoName(name)
		if claimField(goName, name) {
			paramFields = append(paramFields, field{name: goName, typ: typ, comment: comment})
		}
	}
	for _, cm := range chooses {
		goName := GoName(cm.c.Param)
		if claimField(goName, "@choose("+cm.c.Param+")") {
			comment := " // required: zero value is an error"
			if cm.c.Default != nil {
				comment = " // zero value selects @default"
			}
			paramFields = append(paramFields, field{name: goName, typ: cm.enumName, comment: comment})
		}
	}
	for _, om := range orders {
		goName := GoName(om.o.Param)
		if claimField(goName, "@order-by("+om.o.Param+")") {
			comment := " // key sequence; empty = @default"
			if om.o.Default == nil {
				comment = " // key sequence; empty omits ORDER BY"
			}
			paramFields = append(paramFields, field{name: goName, typ: "[]" + om.typeName, comment: comment})
		}
	}
	if filter != nil {
		if filter.Required {
			// `!` says the caller must decide: a scope or an explicit
			// Unscoped(). As an argument, "decide" is not something the
			// compiler lets them skip.
			g.sigImports[runtimeImport] = true
			requiredArgs = append(requiredArgs, requiredArg{
				name:  argIdent(filter.Param),
				typ:   "runtime.Tree",
				expr:  argIdent(filter.Param),
				doc:   "@filter-tree!; " + q.Name + "Unscoped() opts out",
				param: filter.Param,
			})
		} else {
			goName := GoName(filter.Param)
			if claimField(goName, "@filter-tree("+filter.Param+")") {
				paramFields = append(paramFields, field{name: goName, typ: "runtime.Tree", comment: " // zero Tree renders TRUE"})
			}
		}
	}

	// ---- row struct ------------------------------------------------------
	rowName := q.Name + "Row"
	hasRows := q.Annotation == template.AnnotationOne || q.Annotation == template.AnnotationMaybeOne ||
		q.Annotation == template.AnnotationMany
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
			typ, optElem := goType.Name, ""
			if i < len(g.in.Nullable) && g.in.Nullable[i] {
				typ, optElem = "optional.Option["+goType.Name+"]", goType.Name
				g.addImport(optionalImport)
			}
			goName := GoName(col.Name)
			if prev, dup := colNames[goName]; dup {
				fail(diagnostics.CodeNameCollision, q.HeaderSpan,
					"row field %q generated for both columns %q and %q; alias one differently", goName, prev, col.Name)
				continue
			}
			colNames[goName] = col.Name
			rowFields = append(rowFields, field{name: goName, typ: typ, optElem: optElem})
		}
	}

	if diagnostics.HasErrors(diags) {
		return nil, "", diags
	}

	// ---- render ----------------------------------------------------------
	// Imports must be complete before the header is written: the
	// choose/order error paths use fmt, and :maybe-one's no-row branch
	// needs the Option package plus the driver's no-rows sentinel.
	if len(chooses) > 0 || len(orders) > 0 {
		g.imports["fmt"] = true
	}
	if q.Annotation == template.AnnotationMaybeOne {
		g.addImport(optionalImport)
		g.sigImports[optionalImport] = true
		g.imports["errors"] = true
		if g.style == runtime.StyleQuestion {
			g.imports["database/sql"] = true
		} else {
			g.addImport(pgxImport)
		}
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
	for _, om := range orders {
		prefix := q.Name + GoName(om.o.Param)
		fmt.Fprintf(w, "type %s int\n\nconst (\n", om.typeName)
		for i, k := range om.o.Keys {
			fmt.Fprintf(w, "\t%s%sAsc %s = %d\n", prefix, GoName(k.Name), om.typeName, i*2)
			fmt.Fprintf(w, "\t%s%sDesc %s = %d\n", prefix, GoName(k.Name), om.typeName, i*2+1)
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

	if g.in.ExpandedShapes != nil {
		g.writeShapesVar(w)
	} else {
		g.writeFragsVar(w)
	}
	if filter != nil {
		g.writeFilterConstructors(w, filter, &diags)
		if diagnostics.HasErrors(diags) {
			return nil, "", diags
		}
	}
	sig := g.writeFunc(w, paramsName, rowName, chooses, orders, filter, ins, rowFields, requiredArgs)

	return []byte(g.b.String()), sig, diags
}

// requiredArg is a value the caller cannot be allowed to omit: a
// policy-woven parameter or the tree of a `@filter-tree!`. Both become
// arguments of the generated method rather than params-struct fields,
// because Go has no way to make a struct field mandatory — a keyed
// composite literal that leaves one out compiles and yields the zero
// value. For a scoping value that is the difference between "the query
// refused to run" and "the query ran unscoped", so the omission has to
// be a compile error instead.
//
// Note the residual: an argument can still be given an explicit zero
// (`nil`, `""`). Passing one is a deliberate act rather than an
// oversight, and the tree keeps its ErrFilterRequired check for it.
type requiredArg struct {
	name  string // Go identifier in the signature
	typ   string // Go type
	expr  string // how the body refers to it
	doc   string // why it is required, for the signature comment
	param string // template parameter name
}

// policyType is the distinct named Go type generated for one
// policy-woven parameter. Policies guarantee the predicate's presence,
// never its argument's correctness; the named type closes the residual
// a plain `int64` leaves open — two same-typed policy arguments swapped
// at a call site. One type per parameter name, shared by every query
// (and every policy) that binds it, so the value stays passable across
// call sites; parameters agreeing on the name but not the Go type are
// a name collision, never a silent second type.
type policyType struct {
	underlying string          // Go type the name wraps
	imp        string          // import the underlying type needs ("" = none)
	param      string          // template parameter name
	policies   map[string]bool // policies that wove it, for the doc comment
	query      string          // first registering query, for diagnostics
	span       diagnostics.Span
}

// convertExpr spells the conversion of ident back to the underlying
// type at the bind site, so the driver never sees the named type.
func convertExpr(typ, ident string) string {
	if strings.HasPrefix(typ, "*") {
		// *T(x) would parse as *(T(x)).
		return "(" + typ + ")(" + ident + ")"
	}
	return typ + "(" + ident + ")"
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// policyFile emits policy.gen.go: the named types of every policy-woven
// parameter, in sorted order.
func policyFile(pkg string, types map[string]*policyType) string {
	var b strings.Builder
	fmt.Fprintf(&b, "// Code generated by sqletch. DO NOT EDIT.\n\npackage %s\n\n", pkg)
	imports := map[string]bool{}
	for _, pt := range types {
		if pt.imp != "" {
			imports[pt.imp] = true
		}
	}
	if len(imports) > 0 {
		fmt.Fprint(&b, "import (\n")
		for _, imp := range sortedKeys(imports) {
			fmt.Fprintf(&b, "\t%q\n", imp)
		}
		fmt.Fprint(&b, ")\n\n")
	}
	for _, tn := range sortedKeys(types) {
		pt := types[tn]
		pols := sortedKeys(pt.policies)
		label := "policy " + pols[0]
		if len(pols) > 1 {
			label = "policies " + strings.Join(pols, ", ")
		}
		fmt.Fprintf(&b, "// %s is the %q argument woven by %s.\n", tn, pt.param, label)
		fmt.Fprint(&b, "// A distinct type keeps same-typed policy arguments from being\n")
		fmt.Fprintf(&b, "// swapped at a call site; construct it explicitly: %s(v).\n", tn)
		fmt.Fprintf(&b, "type %s %s\n\n", tn, pt.underlying)
	}
	return b.String()
}

// argIdent names a required argument. Reserved identifiers get a
// suffix so a parameter called `ctx` or `arg` cannot shadow the
// generated locals.
func argIdent(param string) string {
	name := lowerCamel(GoName(param))
	switch name {
	case "ctx", "arg", "q", "err", "key", "zero", "args", "binds", "sqlText", "argIdx", "items", "rows", "i":
		return name + "Arg"
	}
	return name
}

// filterOnlyParam reports a parameter bound exclusively inside
// @predicate bodies — a constructor argument, not a struct field.
func filterOnlyParam(p *template.Param) bool {
	if len(p.Occurrences) == 0 {
		return false
	}
	for _, occ := range p.Occurrences {
		if !occ.InFilterTree {
			return false
		}
	}
	return true
}

// writeFilterConstructors emits the typed predicate constructors and
// the explicit Unscoped opt-out.
func (g *queryGen) writeFilterConstructors(w *strings.Builder, filter *template.FilterTree, diags *[]diagnostics.Diagnostic) {
	q := g.in.Q
	for i, pr := range filter.Predicates {
		var params []string
		ok := true
		for _, name := range pr.Params {
			tr, seen := g.in.ParamTypes[name]
			if !seen {
				*diags = append(*diags, diagnostics.Errorf(diagnostics.CodeUnsupportedType, pr.Span,
					"predicate parameter %q has no resolved type", name))
				ok = false
				continue
			}
			goType, tok := g.tm.GoType(tr.OID)
			if !tok {
				*diags = append(*diags, diagnostics.Errorf(diagnostics.CodeUnsupportedType, pr.Span,
					"no Go mapping for type %s (oid %d) of predicate parameter %q", tr.Name, tr.OID, name))
				ok = false
				continue
			}
			g.addImport(goType.Import)
			params = append(params, GoName(name)+" "+goType.Name)
		}
		if !ok {
			continue
		}
		var args []string
		for _, name := range pr.Params {
			args = append(args, GoName(name))
		}
		fmt.Fprintf(w, "// %s%s builds the %q predicate of @filter-tree(%s).\n",
			q.Name, GoName(pr.Name), pr.Name, filter.Param)
		fmt.Fprintf(w, "func %s%s(%s) runtime.Tree {\n\treturn runtime.NewLeaf(%d",
			q.Name, GoName(pr.Name), strings.Join(lowerFirst(params), ", "), i)
		for _, a := range lowerFirst(args) {
			fmt.Fprintf(w, ", %s", a)
		}
		fmt.Fprint(w, ")\n}\n\n")
	}
	fmt.Fprintf(w, "// %sUnscoped is the explicit, greppable opt-out: it renders TRUE.\n", q.Name)
	fmt.Fprintf(w, "func %sUnscoped() runtime.Tree { return runtime.Unscoped() }\n\n", q.Name)
}

// lowerFirst lowercases the leading identifier of each "Name type" (or
// bare name) so constructor arguments are unexported locals.
func lowerFirst(items []string) []string {
	out := make([]string, len(items))
	for i, s := range items {
		out[i] = lowerCamel(s)
	}
	return out
}

// writeShapesVar emits the precomposed shape table (sorted keys for
// deterministic output).
func (g *queryGen) writeShapesVar(w *strings.Builder) {
	fmt.Fprintf(w, "var %sShapes = map[string]runtime.Expanded{\n", lowerCamel(g.in.Q.Name))
	keys := make([]string, 0, len(g.in.ExpandedShapes))
	for k := range g.in.ExpandedShapes {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		e := g.in.ExpandedShapes[k]
		fmt.Fprintf(w, "\t%q: {SQL: %q, ArgIdx: []int16{", k, e.SQL)
		for i, a := range e.ArgIdx {
			if i > 0 {
				fmt.Fprint(w, ", ")
			}
			fmt.Fprintf(w, "%d", a)
		}
		fmt.Fprint(w, "}},\n")
	}
	fmt.Fprint(w, "}\n\n")
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
			switch f.Sep {
			case runtime.SepAnd:
				sep = "runtime.SepAnd"
			case runtime.SepComma:
				sep = "runtime.SepComma"
			}
			fmt.Fprintf(w, "\t{Kind: runtime.Guarded, GuardMask: %#x, Sep: %s, Text: %q%s},\n",
				f.GuardMask, sep, f.Text, spanLits(f.ParamSpans, f.ParamIdx))
		case runtime.Choose:
			fmt.Fprint(w, "\t{Kind: runtime.Choose, Cases: []runtime.Case{\n")
			for _, c := range f.Cases {
				fmt.Fprintf(w, "\t\t{Text: %q%s},\n", c.Text, spanLits(c.ParamSpans, c.ParamIdx))
			}
			fmt.Fprint(w, "\t}},\n")
		case runtime.InAny:
			fmt.Fprintf(w, "\t{Kind: runtime.InAny, ParamIdx: []int16{%d}},\n", f.ParamIdx[0])
		case runtime.InList:
			fmt.Fprintf(w, "\t{Kind: runtime.InList, Text: %q, ParamIdx: []int16{%d}},\n", f.Text, f.ParamIdx[0])
		case runtime.FilterTree:
			fmt.Fprint(w, "\t{Kind: runtime.FilterTree, Cases: []runtime.Case{\n")
			for _, c := range f.Cases {
				fmt.Fprintf(w, "\t\t{Text: %q%s},\n", c.Text, spanLits(c.ParamSpans, c.ParamIdx))
			}
			fmt.Fprint(w, "\t}},\n")
		case runtime.OrderBy:
			fmt.Fprint(w, "\t{Kind: runtime.OrderBy, Cases: []runtime.Case{\n")
			for _, c := range f.Cases {
				fmt.Fprintf(w, "\t\t{Text: %q%s},\n", c.Text, spanLits(c.ParamSpans, c.ParamIdx))
			}
			fmt.Fprint(w, "\t}")
			if f.Default != nil {
				fmt.Fprintf(w, ", Default: &runtime.Case{Text: %q%s}",
					f.Default.Text, spanLits(f.Default.ParamSpans, f.Default.ParamIdx))
			}
			fmt.Fprint(w, "},\n")
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
	chooses []chooseMeta, orders []orderMeta, filter *template.FilterTree,
	ins []*template.InExpr, rowFields []field, requiredArgs []requiredArg) string {

	q := g.in.Q
	fragsVar := lowerCamel(q.Name) + "Frags"

	var ret, zero string
	switch q.Annotation {
	case template.AnnotationMany:
		ret, zero = "([]"+rowName+", error)", "nil"
	case template.AnnotationOne:
		ret, zero = "("+rowName+", error)", "zero"
	case template.AnnotationMaybeOne:
		ret, zero = "(optional.Option["+rowName+"], error)", "zero"
	case template.AnnotationExecRows:
		ret, zero = "(int64, error)", "0"
	default:
		ret, zero = "error", ""
	}
	// Required values come before the params struct, in template order
	// with the tree last, so the signature reads "what you must decide,
	// then what you may vary".
	var params []string
	params = append(params, "ctx context.Context")
	for _, ra := range requiredArgs {
		params = append(params, fmt.Sprintf("%s %s", ra.name, ra.typ))
	}
	params = append(params, "arg "+paramsName)
	sig := fmt.Sprintf("%s(%s) %s", q.Name, strings.Join(params, ", "), ret)
	for _, ra := range requiredArgs {
		fmt.Fprintf(w, "// %s is required (%s).\n", ra.name, ra.doc)
	}
	fmt.Fprintf(w, "func (q *Queries) %s {\n", sig)
	switch q.Annotation {
	case template.AnnotationOne:
		fmt.Fprintf(w, "\tvar zero %s\n", rowName)
	case template.AnnotationMaybeOne:
		// Option's zero value is None, which doubles as the no-row result.
		fmt.Fprintf(w, "\tvar zero optional.Option[%s]\n", rowName)
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
		if atom.IsValue() {
			op := "=="
			if atom.Op == "!=" {
				op = "!="
			}
			fmt.Fprintf(w, "\tif arg.%s %s %s {\n\t\tkey.Guards |= 1 << %d\n\t}\n",
				GoName(atom.Param), op, goLiteral(atom), i)
			continue
		}
		fmt.Fprintf(w, "\tif arg.%s.IsSome() {\n\t\tkey.Guards |= 1 << %d\n\t}\n", GoName(atom.Param), i)
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
	if len(orders) > 0 {
		g.imports["fmt"] = true
		var seqs []string
		for i, om := range orders {
			seq := fmt.Sprintf("oseq%d", i)
			seqs = append(seqs, seq)
			fmt.Fprintf(w, "\t%s, err := runtime.OrderSeq(arg.%s, %d)\n",
				seq, GoName(om.o.Param), len(om.o.Keys))
			fmt.Fprintf(w, "\tif err != nil {\n\t\t%s\n\t}\n", errRet(fmt.Sprintf("fmt.Errorf(%q, err)", q.Name+": %w")))
		}
		fmt.Fprintf(w, "\tkey.Orders = [][]uint8{%s}\n", strings.Join(seqs, ", "))
	}
	if g.style == runtime.StyleQuestion && len(ins) > 0 {
		var arities []string
		for _, in := range ins {
			arities = append(arities, fmt.Sprintf("int32(len(arg.%s))", GoName(in.Param)))
		}
		fmt.Fprintf(w, "\tkey.Arities = []int32{%s}\n", strings.Join(arities, ", "))
	}

	// A required value is an argument, not a field, so the body must
	// name it directly.
	argExpr := map[string]string{}
	for _, ra := range requiredArgs {
		argExpr[ra.param] = ra.expr
	}
	var vals []string
	for _, name := range q.ParamOrder {
		switch {
		case filter != nil && name == filter.Param:
			vals = append(vals, "nil /* tree control */")
		case filterOnlyParam(q.Params[name]):
			vals = append(vals, "nil /* predicate arg */")
		case argExpr[name] != "":
			vals = append(vals, argExpr[name])
		case q.Params[name].Optional:
			// The composed plan selects this slot only in shapes whose
			// guard is on (Some), and UnwrapAsPtr keeps the value the
			// driver sees identical to the pre-Option pointer bind.
			vals = append(vals, "arg."+GoName(name)+".UnwrapAsPtr()")
		default:
			vals = append(vals, "arg."+GoName(name))
		}
	}
	styleArg := ""
	if g.style == runtime.StyleQuestion {
		styleArg = "runtime.StyleQuestion, "
	}
	switch {
	case filter != nil:
		treeField := "arg." + GoName(filter.Param)
		if e := argExpr[filter.Param]; e != "" {
			treeField = e
		}
		if filter.Required {
			// The argument's type keeps `nil` from compiling; this
			// refuses the one zero that still can, `runtime.Tree{}`.
			fmt.Fprintf(w, "\tif %s.IsZero() {\n\t\t%s\n\t}\n", treeField, errRet("runtime.ErrFilterRequired"))
		}
		// The tree's `;t=` key segment is NOT derived here: the cache
		// derives it authoritatively, and deriving it a second time for
		// the hook encoded every tree twice per call. hookTree spells it
		// out only when a hook is installed.
		method := "GetTree"
		if g.style == runtime.StyleQuestion {
			method = "GetTreeStyle"
		}
		fmt.Fprintf(w, "\tsqlText, binds, err := q.cache.%s(%s%q, %s, key, %s, runtime.TreeCaps{MaxNodes: %d, MaxDepth: %d})\n",
			method, styleArg, q.Name, fragsVar, treeField, g.caps.MaxNodes, g.caps.MaxDepth)
		fmt.Fprintf(w, "\tif err != nil {\n\t\t%s\n\t}\n", errRet("err"))
		fmt.Fprintf(w, "\targs := runtime.ResolveArgs(binds, []any{%s}, runtime.TreeArgs(%s))\n",
			strings.Join(vals, ", "), treeField)
	case g.in.ExpandedShapes != nil:
		fmt.Fprintf(w, "\tsqlText, argIdx, err := runtime.Lookup(%sShapes, key)\n", lowerCamel(q.Name))
		fmt.Fprintf(w, "\tif err != nil {\n\t\t%s\n\t}\n", errRet("err"))
		fmt.Fprintf(w, "\targs := runtime.BuildArgs(argIdx, []any{%s})\n", strings.Join(vals, ", "))
	case g.style == runtime.StyleQuestion:
		// The binds path covers slice-element expansion (@in).
		fmt.Fprintf(w, "\tsqlText, binds, err := q.cache.GetBindsStyle(runtime.StyleQuestion, %q, %s, key)\n", q.Name, fragsVar)
		fmt.Fprintf(w, "\tif err != nil {\n\t\t%s\n\t}\n", errRet("err"))
		fmt.Fprintf(w, "\targs := runtime.ResolveArgs(binds, []any{%s}, nil)\n", strings.Join(vals, ", "))
	default:
		fmt.Fprintf(w, "\tsqlText, argIdx := q.cache.Get(%q, %s, key)\n", q.Name, fragsVar)
		fmt.Fprintf(w, "\targs := runtime.BuildArgs(argIdx, []any{%s})\n", strings.Join(vals, ", "))
	}
	// The key is passed unencoded: hook only spells it out when a hook
	// is actually installed, so the common no-observability case does
	// not pay for the canonical encoding on every call. A filter tree
	// goes through hookTree, which folds in the `;t=` segment there for
	// the same reason.
	if filter != nil {
		treeField := "arg." + GoName(filter.Param)
		if e := argExpr[filter.Param]; e != "" {
			treeField = e
		}
		fmt.Fprintf(w, "\tq.hookTree(key, %s, sqlText)\n", treeField)
	} else {
		fmt.Fprint(w, "\tq.hook(key, sqlText)\n")
	}

	// Nullable columns scan through a *T temporary and convert with
	// optional.FromNillable afterwards: the driver keeps seeing exactly
	// the pointer destinations it saw before Option adoption, so no
	// scan path depends on Option implementing sql.Scanner (pgx would
	// route that through a lossy driver.Value detour).
	declTemps := func(indent string) string {
		var b strings.Builder
		n := 0
		for _, f := range rowFields {
			if f.optElem != "" {
				fmt.Fprintf(&b, "%svar nul%d *%s\n", indent, n, f.optElem)
				n++
			}
		}
		return b.String()
	}
	assignTemps := func(indent string) string {
		var b strings.Builder
		n := 0
		for _, f := range rowFields {
			if f.optElem != "" {
				fmt.Fprintf(&b, "%si.%s = optional.FromNillable(nul%d)\n", indent, f.name, n)
				n++
			}
		}
		return b.String()
	}
	scanList := func() string {
		var refs []string
		n := 0
		for _, f := range rowFields {
			if f.optElem != "" {
				refs = append(refs, fmt.Sprintf("&nul%d", n))
				n++
				continue
			}
			refs = append(refs, "&i."+f.name)
		}
		return strings.Join(refs, ", ")
	}

	query, queryRow, exec := "Query", "QueryRow", "Exec"
	if g.style == runtime.StyleQuestion {
		query, queryRow, exec = "QueryContext", "QueryRowContext", "ExecContext"
	}
	switch q.Annotation {
	case template.AnnotationMany:
		fmt.Fprintf(w, "\trows, err := q.db.%s(ctx, sqlText, args...)\n", query)
		fmt.Fprintf(w, "\tif err != nil {\n\t\t%s\n\t}\n", errRet("err"))
		fmt.Fprint(w, "\tdefer rows.Close()\n")
		fmt.Fprintf(w, "\tvar items []%s\n", rowName)
		fmt.Fprint(w, "\tfor rows.Next() {\n")
		fmt.Fprintf(w, "\t\tvar i %s\n", rowName)
		fmt.Fprint(w, declTemps("\t\t"))
		fmt.Fprintf(w, "\t\tif err := rows.Scan(%s); err != nil {\n\t\t\treturn nil, err\n\t\t}\n", scanList())
		fmt.Fprint(w, assignTemps("\t\t"))
		fmt.Fprint(w, "\t\titems = append(items, i)\n\t}\n")
		fmt.Fprint(w, "\treturn items, rows.Err()\n")
	case template.AnnotationOne:
		fmt.Fprintf(w, "\trow := q.db.%s(ctx, sqlText, args...)\n", queryRow)
		fmt.Fprint(w, "\tvar i "+rowName+"\n")
		fmt.Fprint(w, declTemps("\t"))
		fmt.Fprintf(w, "\tif err := row.Scan(%s); err != nil {\n\t\treturn zero, err\n\t}\n", scanList())
		fmt.Fprint(w, assignTemps("\t"))
		fmt.Fprint(w, "\treturn i, nil\n")
	case template.AnnotationMaybeOne:
		noRows := "pgx.ErrNoRows"
		if g.style == runtime.StyleQuestion {
			noRows = "sql.ErrNoRows"
		}
		fmt.Fprintf(w, "\trow := q.db.%s(ctx, sqlText, args...)\n", queryRow)
		fmt.Fprint(w, "\tvar i "+rowName+"\n")
		fmt.Fprint(w, declTemps("\t"))
		fmt.Fprintf(w, "\tif err := row.Scan(%s); err != nil {\n", scanList())
		fmt.Fprintf(w, "\t\tif errors.Is(err, %s) {\n\t\t\treturn zero, nil\n\t\t}\n", noRows)
		fmt.Fprint(w, "\t\treturn zero, err\n\t}\n")
		fmt.Fprint(w, assignTemps("\t"))
		fmt.Fprint(w, "\treturn optional.Some(i), nil\n")
	case template.AnnotationExecRows:
		if g.style == runtime.StyleQuestion {
			fmt.Fprint(w, "\tres, err := q.db.ExecContext(ctx, sqlText, args...)\n")
			fmt.Fprint(w, "\tif err != nil {\n\t\treturn 0, err\n\t}\n")
			fmt.Fprint(w, "\treturn res.RowsAffected()\n")
		} else {
			fmt.Fprint(w, "\ttag, err := q.db.Exec(ctx, sqlText, args...)\n")
			fmt.Fprint(w, "\tif err != nil {\n\t\treturn 0, err\n\t}\n")
			fmt.Fprint(w, "\treturn tag.RowsAffected(), nil\n")
		}
	default: // exec
		fmt.Fprintf(w, "\t_, err := q.db.%s(ctx, sqlText, args...)\n", exec)
		fmt.Fprint(w, "\treturn err\n")
	}
	fmt.Fprint(w, "}\n")
	return sig
}

// treeHookFunc is spliced into db.gen.go only when the package has a
// @filter-tree query. See hookTree's own comment for why the tree's key
// segment is derived here rather than at the call site.
const treeHookFunc = `
// hookTree is hook for a @filter-tree query. The tree's key segment is
// derived here rather than at the call site so that a query without an
// installed hook never encodes its tree twice.
func (q *Queries) hookTree(key runtime.ShapeKey, t runtime.Tree, sql string) {
	if q.onQuery != nil {
		key.Trees = []string{t.Encode()}
		q.onQuery(key.String(), sql)
	}
}
`

func anyFilterTree(queries []QueryInput) bool {
	for _, in := range queries {
		for _, it := range in.Q.Items {
			if _, ok := it.(*template.FilterTree); ok {
				return true
			}
		}
	}
	return false
}

func dbFile(pkg string, treeHook string) string {
	return fmt.Sprintf(`// Code generated by sqletch. DO NOT EDIT.

package %s

import (
	"context"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/moznion/go-sqletch/runtime"
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

func (q *Queries) hook(key runtime.ShapeKey, sql string) {
	if q.onQuery != nil {
		q.onQuery(key.String(), sql)
	}
}
%s
// And / Or combine @filter-tree predicates built with the generated
// per-query constructors.
var (
	And = runtime.And
	Or  = runtime.Or
)
`, pkg, treeHook)
}

func dbFileQuestion(pkg string, treeHook string) string {
	return fmt.Sprintf(`// Code generated by sqletch. DO NOT EDIT.

package %s

import (
	"context"
	"database/sql"

	"github.com/moznion/go-sqletch/runtime"
)

// DBTX matches sqlc's database/sql flavor: a *sql.DB or *sql.Tx
// satisfies it, so sqlc- and sqletch-generated code share transactions.
type DBTX interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

type Queries struct {
	db      DBTX
	cache   *runtime.ComposedCache
	onQuery func(shapeKey, sql string)
}

func New(db DBTX) *Queries {
	return &Queries{db: db, cache: runtime.NewComposedCache(256)}
}

func (q *Queries) WithTx(tx *sql.Tx) *Queries {
	return &Queries{db: tx, cache: q.cache, onQuery: q.onQuery}
}

// OnQuery installs an observability hook receiving the shape key and
// the composed SQL of every call.
func (q *Queries) OnQuery(fn func(shapeKey, sql string)) { q.onQuery = fn }

func (q *Queries) hook(key runtime.ShapeKey, sql string) {
	if q.onQuery != nil {
		q.onQuery(key.String(), sql)
	}
}
%s
// And / Or combine @filter-tree predicates built with the generated
// per-query constructors.
var (
	And = runtime.And
	Or  = runtime.Or
)
`, pkg, treeHook)
}

func querierFile(pkg string, sigs []string, extraImports map[string]bool) string {
	var b strings.Builder
	// Group standard library and external imports with a blank line
	// between them, the same way writeImports does: gofmt leaves an
	// ungrouped block alone but goimports rewrites it, and CI runs both.
	std := []string{"context"}
	var ext []string
	for imp := range extraImports {
		switch {
		case imp == "" || imp == "context":
		case strings.Contains(imp, "."):
			ext = append(ext, imp)
		default:
			std = append(std, imp)
		}
	}
	sort.Strings(std)
	sort.Strings(ext)
	fmt.Fprintf(&b, "// Code generated by sqletch. DO NOT EDIT.\n\npackage %s\n\nimport (\n", pkg)
	for _, imp := range std {
		fmt.Fprintf(&b, "\t%q\n", imp)
	}
	if len(ext) > 0 {
		fmt.Fprint(&b, "\n")
		for _, imp := range ext {
			fmt.Fprintf(&b, "\t%q\n", imp)
		}
	}
	fmt.Fprint(&b, ")\n\n")
	fmt.Fprint(&b, "// Querier lets user code mock the generated queries.\ntype Querier interface {\n")
	for _, s := range sigs {
		fmt.Fprintf(&b, "\t%s\n", s)
	}
	fmt.Fprint(&b, "}\n\nvar _ Querier = (*Queries)(nil)\n")
	return b.String()
}

// valueAtomGoType returns the Go type of a pure @when control
// parameter (typed by its literal).
func valueAtomGoType(q *template.QueryTemplate, name string) (string, bool) {
	for _, g := range q.GuardAtoms {
		if g.Param != name || !g.IsValue() {
			continue
		}
		switch g.Kind {
		case template.ValueString:
			return "string", true
		case template.ValueInt:
			return "int64", true
		case template.ValueBool:
			return "bool", true
		}
	}
	return "", false
}

// goLiteral renders a @when literal as a Go expression.
func goLiteral(g template.GuardAtom) string {
	if g.Kind == template.ValueString {
		return fmt.Sprintf("%q", g.Value)
	}
	return g.Value // int / bool literals are identical in Go
}

func paramSpanOf(q *template.QueryTemplate, name string) diagnostics.Span {
	if p := q.Params[name]; p != nil && len(p.Occurrences) > 0 {
		return p.Occurrences[0].Span
	}
	return q.HeaderSpan
}

// pascalToSnake maps a query name to its generated file stem. A run of
// capitals is ONE word — FindUserByUserID -> find_user_by_user_id,
// ParseHTTPRequest -> parse_http_request — so the initialisms GoName
// produces (doc: naming.go) survive the round trip; an underscore
// already present in the name is never doubled.
func pascalToSnake(name string) string {
	rs := []rune(name)
	var b strings.Builder
	for i, r := range rs {
		if i > 0 && unicode.IsUpper(r) && rs[i-1] != '_' {
			// A word starts here when the previous rune is not itself a
			// capital (userID -> user_id), or when this capital is the
			// last of a run and a lowercase follows (HTTPRequest ->
			// http_request).
			if !unicode.IsUpper(rs[i-1]) || (i+1 < len(rs) && unicode.IsLower(rs[i+1])) {
				b.WriteRune('_')
			}
		}
		b.WriteRune(unicode.ToLower(r))
	}
	return b.String()
}
