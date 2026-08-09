# sqletch Design — 17: go-optional Adoption (Option[T] in Generated Code)

Status: implemented (2026-08). Delta over designs 05/06; spec sections
"Query annotations", "@if-present", R9, and "Generated API
Conventions" updated in the same change.

## 1. Decision

Generated code represents **absence** uniformly with
`github.com/moznion/go-optional`'s `Option[T]`, replacing bare
pointers. There is deliberately **no configuration knob and no
pointer fallback** — the owner's call (2026-08-09): one representation
keeps the generated surface, docs, and tests single-track.

Three surfaces:

1. **Optional (`@if-present`) parameters** — params-struct fields are
   `optional.Option[T]`; the zero value `None` omits the guarded
   fragment(s). Presence check compiles to `arg.X.IsSome()`.
2. **Nullable result columns** (P5 output) — row-struct fields are
   `optional.Option[T]`.
3. **`:maybe-one`** (new annotation) — `:one` with "no row" as a
   normal outcome: returns `(optional.Option[Row], error)`; the
   driver's no-rows sentinel (`pgx.ErrNoRows` / `sql.ErrNoRows`) maps
   to `(None, nil)`; a hit returns `optional.Some(row)`.

The generated `Ptr[T]` helper is gone (callers write
`optional.Some(v)`); "Ptr" left the reserved-name list with it.

## 2. The driver boundary is unchanged — by construction

go-optional v0.13.0 implements `sql.Scanner`/`driver.Valuer`, but the
generated code deliberately does **not** rely on either:

- **Binding**: an optional parameter binds as
  `arg.X.UnwrapAsPtr()` — the driver sees exactly the `*T` it saw
  before this change (composition selects the slot only in shapes
  whose guard is on, so the pointer is non-nil whenever it is bound;
  `runtime.BuildArgs`/`ResolveArgs` are untouched).
- **Scanning**: nullable columns scan into a per-column `*T`
  temporary (`var nul0 *string; … rows.Scan(&i.ID, &nul0)`) and
  convert afterwards with `i.X = optional.FromNillable(nul0)`.

Rationale: pgx v5 routes unknown `sql.Scanner` destinations through a
`driver.Value` detour (codec → text-ish value → `convertAssign`),
which is lossy/slow for types like `numeric`, and `Option[T]`'s
underlying `[]T` shape invites ambiguity in pgx's reflection-based
plan selection. Keeping the wire-facing types identical to the
pre-Option emission means **no scan or bind behavior rides on
go-optional's driver integration** on any dialect. A future
pgx-native codec module (`go-optional` side) could enable direct
`&i.Field` scanning; that is an optimization, not a correctness need.

## 3. :maybe-one

- Scanner: header vocabulary gains `maybe-one`
  (`AnnotationMaybeOne`); everything between header parse and codegen
  treats it exactly like `:one` (same renderings, same oracle
  entries, same nullability) — codegen is the only consumer that
  branches.
- Emission (per placeholder style):
  - dollar/pgx: `errors.Is(err, pgx.ErrNoRows)` → `return zero, nil`
    (the file imports pgx; the consumer module already requires it
    for `DBTX`).
  - question/database-sql: `errors.Is(err, sql.ErrNoRows)`.
  - `var zero optional.Option[Row]` doubles as the None result
    (Option's zero value is None).

## 4. Consequences

- **Generated code now imports one external package** beyond
  `runtime`: `github.com/moznion/go-optional` (consumer modules pick
  it up via `go mod tidy`). The corpus byte-identity gates and
  determinism are unaffected — the emission is byte-deterministic as
  before, and nothing here touches renderings, cache JSON, or shape
  keys.
- **JSON ergonomics**: `None` is a nil slice underneath, so
  `omitempty` works on row-struct fields and `None` marshals as
  `null` (go-optional implements json.Marshaler/Unmarshaler).
- The e2e generated-module scaffolds pin go-optional's version from
  the parent go.mod, like every other dependency they inject.

## 5. Not done, deliberately

- `internal/config`'s tri-state `Override.Nullable *bool` stays a
  pointer: yaml.v3 consults only `yaml.Unmarshaler` on decode, so
  `Option[bool]` would need go-optional to take a hard dependency on
  the (archived) yaml.v3 — not worth one field. Revisit only if
  go-optional grows `UnmarshalYAML` upstream.
- Internal comma-ok APIs (`cache.Load*`, `TypeMap.GoType`, …) and
  nil-pointer defaults (`template.Choose.Default`,
  `runtime.Frag.Default`) keep Go idioms; converting them adds a
  dependency to `runtime/`'s public API without any soundness gain.
- `internal/lsp` is excluded on principle (stdlib-only invariant,
  byte-pinned outbound frames).
