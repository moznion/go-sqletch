# Diagnostics reference

Every sqletch diagnostic carries a stable code. **Codes are part of
the v1 compatibility surface — their meanings do not change**; message
wording may improve between releases
([compatibility](11-compatibility.md)). `--json` emits machine-readable
diagnostics with the same codes; the LSP server reports them as you
type.

Severity is `error` unless noted. Errors stop the pipeline before
code generation; the CLI exits 1 (see the CLI chapter for exit
codes).

## SQLETCH0xx — template scanning

| Code | Meaning |
| --- | --- |
| SQLETCH001 | Malformed or unterminated construct (`@if-present` without `@endif`, bad argument list, `@in` without `(:param)`, …). |
| SQLETCH002 | Guard, case, key, predicate, and parameter names must be `snake_case` identifiers. |
| SQLETCH003 | A statement without a `-- name: Name :annotation` header. Every query needs one. |
| SQLETCH004 | Two queries share a name. Names are global across all template files. |
| SQLETCH005 | A query contains more than one SQL statement. One statement per query. |
| SQLETCH006 | A construct inside parentheses or a subquery. Constructs live at the top level of the statement (rule R1); write the dynamic part as a top-level slot instead. |
| SQLETCH007 | A construct at a clause that is not one of its slots (e.g. `@if-present` in a projection, `@in` inside a guarded body). The message names the slot rules. |
| SQLETCH008 | An optional WHERE/HAVING conjunct whose body does not start with `AND`. |
| SQLETCH009 | Malformed `@choose`/`@order-by`/`@filter-tree` structure (a `@case` outside `@choose`, empty case list, duplicate `@default`, …). |
| SQLETCH010 | Too many guard atoms for one query (the shape bitmask is 64 bits wide). |
| SQLETCH011 | A positional placeholder (`$1`, `?`) in a template. Templates use `:name` parameters; the compiler owns placeholder emission. |
| SQLETCH012 | A guarded construct inside another guarded body (rule R5). Flatten: multi-param `@if-present(a, b)` expresses conjunction. |
| SQLETCH020 | A `.go` file listed in `queries:` does not parse. Templates are read syntactically, so the file must at least be valid Go syntax (it need not type-check). |
| SQLETCH021 | `//sqletch:query` on something other than a `const` declaration. A const is what makes the verified SQL the SQL that runs. |
| SQLETCH022 | A `//sqletch:query` const whose value is not a single raw (backquoted) string literal. Interpreted strings process escapes and concatenations have no contiguous source range, so template spans could not point back at the file. |
| SQLETCH023 | A `//sqletch:query` const that declares no value, or more names than values. |

## SQLETCH1xx — structural rules

| Code | Meaning |
| --- | --- |
| SQLETCH100 | A verified rendering does not parse under the dialect grammar. The span maps back to the template text at fault. |
| SQLETCH101 | An optional join is neither `INNER` nor `LEFT` (rule R2): `RIGHT`/`FULL`/`CROSS` would change other rows' presence or nullability per shape. |
| SQLETCH102 | A fragment is not exactly one complete AST node for its slot (rule R1) — e.g. two conjuncts in one `@if-present`, a sort key that parses as two. |
| SQLETCH103 | The query is not exactly one `SELECT`/`UPDATE`/`INSERT`/`DELETE`. |
| SQLETCH110 | A `@if-present` guard names a parameter that is required anyway, or a `@when` compares against a value the parameter always has (rule R9): the guard is vacuous. |
| SQLETCH111 | A guard parameter that never binds inside its own guarded fragments (rule R9): presence would have no observable meaning. |
| SQLETCH112 | A control parameter (`@choose`/`@order-by`/`@filter-tree`) also used as a `:name` bind (rule R9). Control values select shapes; they never travel to the database. |
| SQLETCH113 | Every conjunct of a WHERE/HAVING is optional (rule R6). Add the explicit `TRUE` anchor (`sqletch fmt` inserts it). |
| SQLETCH114 | An unqualified column reference matches more than one relation in some shape. Qualify it. |
| SQLETCH115 | A reference into an optional join's columns outside fragments sharing that guard (rule R3): the column vanishes in shapes where the join is absent. |
| SQLETCH116 | A planner-sensitive combination (e.g. a locking clause with an optional `LEFT JOIN`) whose semantics silently differ per shape. |
| SQLETCH117 | `SELECT *` (or `t.*`) would include an optional join's columns (rule R2): the result shape must be constant across shapes. |
| SQLETCH118 | Every `SET` item (or `INSERT` list item) is optional (rule R6): the minimal shape would be syntactically invalid. Keep one unconditional item. |
| SQLETCH119 | `INSERT` guarded column/value pairing is broken (rule R7): for each guard set, column items and `VALUES` items must be equinumerous, positionally aligned, and at the tail of their clauses. |
| SQLETCH122 | `@order-by` under `DISTINCT ON` (PostgreSQL): validity there depends on the sort prefix, which reordering breaks. Use `@choose` with whole verified cases instead. |
| SQLETCH123 | The skeleton uses `FETCH FIRST … WITH TIES`, so `ORDER BY` may never vanish: the `@order-by` needs a `@default`. |
| SQLETCH124 | A query touches a policy-designated table without the policy's scoping conjunct present in every reachable shape, and carries no opt-out. A hand-written copy inside `@if-present` does not count — it vanishes in guard-off shapes. |
| SQLETCH125 | A policy applies to this query but cannot be woven: the designated table sits in a position sqletch cannot scope (inside a subquery/CTE, on the null-extended side of an outer join, introduced by a guarded join), its bound name is not a bare identifier, or the query declares a conflicting type for the policy parameter. Opt out explicitly (`-- @policy-optout`) or restructure. |
| SQLETCH126 | A `-- @policy-optout` names a policy that does not exist, or one that does not apply to this query. Renaming a policy can never silently disarm its opt-outs. |

## SQLETCH2xx — type oracle

| Code | Meaning |
| --- | --- |
| SQLETCH200 | The connected server's version does not match the pinned `server_version`. Fix the pin or the DSN. |
| SQLETCH201 | The database cannot determine a parameter's type. Add an explicit cast (`:param::text`) — the hint shows where. |
| SQLETCH202 | Prepare/describe failed: the database rejected a verified rendering (unknown column/table, type mismatch, …). The span maps the database's error position back into the template. |
| SQLETCH210 | Two renderings disagree on the result columns (name, order, or type). Usually a `@choose` case changing the projection's types. |
| SQLETCH211 | Renderings disagree on one parameter's type (the same `:param` used in incompatible positions). |
| SQLETCH212 | *(warning)* An optional `INSERT` column is `NOT NULL` without a default: omitting it fails at runtime. |
| SQLETCH213 | A `-- @param` annotation disagrees with the type the oracle inferred (Tier 1). The oracle types the rendering, so a wrong annotation would otherwise be invisible until execution; the message spells the inferred type's writable name. |

## SQLETCH3xx — configuration and code generation

| Code | Meaning |
| --- | --- |
| SQLETCH300 | `sqletch.yaml` is unreadable or contains unknown keys (strict decoding). |
| SQLETCH301 | `sqletch.yaml` field validation (unsupported dialect, missing `server_version`, …). |
| SQLETCH302 | Static expansion would exceed `static_expansion.max_shapes`, or the query's shape space is unbounded (`@filter-tree`, `@in` on expanding dialects). |
| SQLETCH303 | A policy declaration in `sqletch.yaml` is malformed (bad name, empty or non-identifier `tables`, unknown `applies_to` kind, …), or its predicate does not parse as one complete boolean expression in the dialect. |
| SQLETCH310 | Generated Go identifiers collide (two queries or columns mapping to the same name). Rename or alias one. |
| SQLETCH311 | No Go mapping for a database type. Cast to a supported type, or on Tier 2 dialects add/fix the `-- @param` / `-- @column` annotation. |
