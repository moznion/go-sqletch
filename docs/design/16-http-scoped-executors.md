# sqletch Design — 16: Scoped query executors for net/http consumers

**Status: PROPOSED — decisions D1–D7 open, to be settled with the
user before implementation.** This document designs a generated
HTTP-facing layer over `@filter-tree!` queries: per-scope executor
bundles whose scoping predicate is partially applied from an
`http.Request`, injected via middleware, and extracted from the
request `context.Context` — such that handler code *cannot* obtain an
unscoped executor. The feature must be consumable by any project that
uses `net/http` (directly or through any router built on
`http.Handler`), not just this repository's examples.

## 1. What this buys — the benefit case

`@filter-tree!` (spec §"Use Case 5") already makes scoping *required*:
`Params.Scope == nil` is a runtime error and the opt-out
(`XxxUnscoped()`) is explicit and greppable. What it does not do is
say anything about *who* builds the tree. In an HTTP service the
scoping value (a tenant id, an organization id) is an attribute of the
request, and today every handler must (a) extract it, (b) call the
right predicate constructor, and (c) remember to do both. (c) is the
weak point: a handler that passes a caller-influenced tree, or the
wrong tenant's value, compiles fine.

This feature inverts that, the same inversion `@filter-tree!` performs
for "did you pass a tree at all" and doc 14 performs for "does the SQL
contain the conjunct":

1. **The scoping leaf becomes impossible to omit or replace.** The
   only executor a handler can reach is one whose scope leaf is
   already bound. There is no code path from handler-visible API
   surface to an unscoped query.
2. **Extraction is written once.** The request → value function is
   supplied once at wiring time (it is *injected*, so sqletch makes no
   assumption about JWTs, headers, or auth frameworks), instead of
   being re-spelled per handler.
3. **Fail-closed at the HTTP boundary.** If the value cannot be
   extracted, the middleware never invokes the next handler; the
   request dies before any query layer is reachable. A route that
   forgot the middleware fails loudly on its first request (the
   context lookup fails), never silently unscoped.
4. **Capability-shaped API.** The scoped executor is a value with
   unexported fields, constructible only inside the generated
   package, carried in the context under an unexported key. Go's
   visibility rules — not reviewer vigilance — are what stand between
   a handler and raw query access.

Honest limits, stated up front:

- **In-process bypass cannot be made absolute.** Any code holding the
  underlying `DBTX` can construct `gen.New(db)` itself. The guarantee
  is capability discipline — handlers are *handed* only scope-bound
  capabilities — hardened by the §7 static enforcement so that
  reaching for raw access is loud in CI, not merely discouraged in
  review.
- **It guarantees the leaf is present, not that its value is
  correct.** A buggy extractor scopes every query to the wrong
  tenant. Same caveat as doc 14 §1.
- **It covers `@filter-tree` queries** (and, later, doc 14
  policy-woven parameters — §9). A query with neither is outside any
  scope set and untouched.

## 2. Position in the architecture — strictly above the verified boundary

This is a **codegen-only** addition. Nothing in the pipeline changes:

```
scan → weave(14) → render → rules → oracle → codegen ─→ gen/        (unchanged)
                                                    └─→ gen/scoped/  (NEW, this doc)
```

- The scoped executor calls the ordinary generated method with an
  ordinary `Params` value; every query still flows through
  `cache.GetTree` → `runtime.ComposeTree` with the same caps, the
  same shape keys, the same bind plans. `TestComposeConformance` is
  untouched and must need no modification — that is the evidence this
  layer adds no soundness surface.
- The forced composition is `runtime.And(scopeLeaf, extra)`. Any tree
  a scoped executor can build is a tree the handler could already have
  passed by hand, so the reachable-shape space is a *subset* of what
  is already verified. (`runtime.And` drops `nil` children and
  collapses a single child, `runtime/tree.go:34`, so "no extra
  filter" composes to exactly the bare leaf.)
- The generator consumes information sqletch already owns — each
  query's predicate vocabulary, predicate argument Go types, and
  params struct layout come from the same `codegen` model that emits
  the constructors. **The generator must not be designed as a
  second-stage tool that parses generated Go**; it runs inside
  `sqletch generate` with first-hand access to the model.
- Cache impact: none. The scoped package is generated output, derived
  from the same inputs; it does not enter the oracle fingerprint.
  Determinism applies as everywhere: byte-identical output for
  identical inputs, scope sets emitted in declaration order, member
  queries in sorted-name order.

## 3. Configuration surface

Following `policies:` (doc 14 §3) in style; strict decoding applies
(`SQLETCH300` for unknown keys), and the key is additive to the frozen
v1.0 `Config` surface, so it must be designed once.

```yaml
http_scopes:
  - name: tenant                # → generated identifiers TenantScope, TenantFrom, …
    predicate: tenant           # member = every @filter-tree query declaring
                                #   a predicate with this name (D2)
    package: scoped             # subpackage under `out:`, default "scoped" (D3)
```

Validation at generate time (codes in §8): a declared predicate name
that no query declares, a member predicate whose argument tuple
disagrees with the others in the set, a predicate with more than one
argument (v1 limit, D7), or a query matched by two scope sets (v1
limit, D7) — each is a diagnostic, never a silent skip.

## 4. Generated surface

For scope `tenant` over queries `ListOrders`, `ListInvoices` (member
predicates typed `int64` by the oracle), sqletch emits one package —
sketched here for the pgx flavor; the database/sql flavor differs only
in the `DBTX`/`WithTx` types, exactly as `gen/` itself does:

```go
// Code generated by sqletch. DO NOT EDIT.
package scoped

// TenantExtractor derives the scope value from a request. It is
// injected at wiring time; sqletch assumes nothing about where the
// value lives (header, JWT claim, path segment, mTLS identity, …).
type TenantExtractor func(*http.Request) (int64, error)

// TenantScope is a capability: it executes member queries with the
// tenant predicate unconditionally bound. Fields are unexported and
// no constructor is exported — values exist only inside a request
// served by TenantMiddleware.
type TenantScope struct {
	q  *gen.Queries
	id int64
}

type tenantKey struct{} // unexported: no other package can forge the entry

// TenantMiddleware extracts the scope value and injects a TenantScope
// into the request context. Extraction failure is fail-closed: onDeny
// (default: 403, no body) runs and next is never invoked.
func TenantMiddleware(q *gen.Queries, extract TenantExtractor,
	onDeny func(http.ResponseWriter, *http.Request, error)) func(http.Handler) http.Handler {
	if onDeny == nil {
		onDeny = func(w http.ResponseWriter, _ *http.Request, _ error) {
			http.Error(w, http.StatusText(http.StatusForbidden), http.StatusForbidden)
		}
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			v, err := extract(r)
			if err != nil {
				onDeny(w, r, err)
				return
			}
			ctx := context.WithValue(r.Context(), tenantKey{}, TenantScope{q: q, id: v})
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

// TenantFrom extracts the scoped executor. ok is false when
// TenantMiddleware is not installed on the route — a wiring bug; the
// handler should fail the request, and MustTenantFrom does so by
// panicking (a misrouted handler breaks loudly on its first request,
// never runs unscoped).
func TenantFrom(ctx context.Context) (TenantScope, bool)
func MustTenantFrom(ctx context.Context) TenantScope

// Per member query: the Params struct minus Scope, plus an optional
// extra tree composed UNDER the forced leaf (D4).
type ListOrdersArgs struct {
	Limit  int64
	Filter *runtime.Tree // optional; nil means no extra filtering
}

func (s TenantScope) ListOrders(ctx context.Context, arg ListOrdersArgs) ([]gen.ListOrdersRow, error) {
	return s.q.ListOrders(ctx, gen.ListOrdersParams{
		Limit: arg.Limit,
		Scope: runtime.And(gen.ListOrdersTenant(s.id), arg.Filter),
	})
}

// WithTx preserves the capability across transactions: the result is
// still scope-bound. There is no path from a TenantScope back to the
// raw *gen.Queries.
func (s TenantScope) WithTx(tx pgx.Tx) TenantScope {
	return TenantScope{q: s.q.WithTx(tx), id: s.id}
}
```

Handler code in a consumer project:

```go
mux.Handle("GET /orders", tenantMW(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
	exec := scoped.MustTenantFrom(r.Context())
	rows, err := exec.ListOrders(r.Context(), scoped.ListOrdersArgs{Limit: 50})
	…
})))
```

Notes:

- **Dependencies of the generated package**: stdlib (`net/http`,
  `context`), `runtime/`, the sibling `gen` package, and the driver
  types `gen` already imports (pgx for the `WithTx` signature). No new
  public sqletch package, no framework import — any router that
  ultimately serves `http.Handler` (net/http, chi, gorilla; echo/gin
  via their stdlib adapters) can install the middleware. This is the
  concrete meaning of "consumable by other net/http projects".
- **The scoped package does not export an unscoped anything.** The
  deliberate escape hatch remains `gen.XxxUnscoped()` on the raw
  `Queries` — outside the handler-visible surface and gated by §7.
- Identifier derivation follows the existing codegen naming rules;
  collisions are the existing `SQLETCH310`.
- `Filter` composes *under* the forced leaf and can never displace it;
  a caller-supplied tree that pushes past the query's `filter_tree_caps`
  fails with the ordinary `runtime.ErrTreeTooLarge` (the forced
  `And` + leaf cost 2 nodes of the budget — worth one manual sentence,
  not a new mechanism).

## 5. The bypass-impossibility argument

Layer by layer, each enforced by the compiler or the runtime rather
than by convention:

1. **Forging the capability** — impossible outside the generated
   package: `TenantScope` has unexported fields and no exported
   constructor; the only expression that creates a non-zero value is
   inside `TenantMiddleware`. (The zero value has `q == nil` and
   fails the nil-receiver check every method performs — a defensive
   diagnostic, not a security boundary, since a zero value scopes
   nothing anyway: every method would panic on the nil `Queries`.)
2. **Forging the context entry** — impossible: `tenantKey` is an
   unexported type; no other package can construct the key.
3. **Reaching the executor without the middleware** — fails closed:
   `TenantFrom` returns `ok == false`, `MustTenantFrom` panics. The
   failure mode of a forgotten middleware is a 500 on first exercise,
   never a silently unscoped query.
4. **Reaching a query without the leaf** — impossible through this
   surface: every generated method sets `Scope:` itself; `Args`
   structs have no `Scope` field; `runtime.And` semantics guarantee
   the extra `Filter` narrows and never replaces.
5. **Reaching the raw `Queries`** — possible in principle for any
   code that holds the `DBTX` (§1 limits), which is exactly the
   residual §7 closes: raw construction is confined to declared
   wiring packages and CI-checked.

## 6. Decisions to settle

### D1 — Input surface: config-declared scope sets, not handler annotations

The conversation that produced this doc started from a per-handler
comment annotation (`//@Scope(ListOrdersParams, tenantID)`). With the
middleware + context design the annotation no longer earns its keep:

- (a) **`http_scopes:` in `sqletch.yaml` only.** The scope set is the
  unit of generation; handlers need no annotation because extraction
  is `TenantFrom(ctx)` — one line, type-safe, unforgeable.
- (b) Handler annotations generating per-endpoint adapters. Requires
  scanning the consumer's handler sources (doc 13 machinery), and the
  property it suggests — "this route has the middleware installed" —
  is not statically checkable against dynamic routing anyway.

**Recommendation: (a).** (b) adds surface without adding a checkable
guarantee. Revisit only if a per-endpoint property becomes checkable
(e.g. a framework with static route tables).

### D2 — Scope-set membership: predicate-name matching

- (a) Explicit query list per scope in config.
- (b) **Matching by predicate name**: every `@filter-tree` query that
  declares a predicate named `tenant` is a member.

**Recommendation: (b).** It is the doc-14 §1.1 inversion at this
layer: a query added next month with a `tenant` predicate is in the
bundle the moment it compiles, and *forgetting* becomes impossible.
The residual risk — a new query that misspells or omits the predicate
name is silently a non-member — is exactly the gap doc 14's
enforcement pass covers table-wise (§9), and `explain` reports
membership per scope (§8) so the set is auditable. (a) re-creates the
per-call-site maintenance this feature exists to remove.

### D3 — Output placement: a sibling subpackage

- (a) Same package as `gen`.
- (b) **Subpackage** (default `<out>/scoped`, name configurable).

**Recommendation: (b).** The package boundary is what makes §7
cheap and precise ("import of `gen` outside the allowlist" is a
package-granular question) and keeps the handler-visible surface
free of `gen.New`/`gen.XxxUnscoped`. It also lets a consumer's
review policy say "handlers import `scoped`, never `gen`" — a
one-line rule.

### D4 — Extra caller filters: allowed, composed under the leaf

- (a) Scoped methods take no tree; the scope is the whole filter.
- (b) **Optional `Filter *runtime.Tree` ANDed under the forced
  leaf.**

**Recommendation: (b).** `@filter-tree!` exists because callers
legitimately compose filters; (a) would push consumers back to the
raw surface for every faceted-search endpoint, eroding the §5
discipline. (b) costs nothing: `runtime.And` already has the right
nil/collapse semantics, and the leaf is not displaceable.

### D5 — Extraction failure handling

- (a) Fixed behavior: 403, empty body.
- (b) **Injectable `onDeny` callback, defaulting to (a).**

**Recommendation: (b).** Real services want their own error envelope
and audit logging; a callback with a safe default keeps fail-closed
the path of least resistance. The callback deliberately receives the
extractor's error; the default writes none of it to the response.

### D6 — Static enforcement of the raw-access residual

- (a) Documentation only.
- (b) **A `go/analysis` Analyzer, published as a public package, plus
  a `sqletch scopecheck` convenience command** (§7).

**Recommendation: (b), phased last.** Without it, §5 step 5 rests on
review discipline, and the user's requirement is that bypass be
*reliably* excluded. `golang.org/x/tools` becomes a dependency of the
analyzer package only.

### D7 — v1 limits: single-argument predicates, disjoint scope sets

Predicates with two or more arguments would need a generated value
struct and a tuple-typed extractor; a query matched by two scope sets
would need combined-capability semantics (which middleware's executor
does the handler use? is one leaf enough?). Both have designs, neither
has a driving use case.

**Recommendation:** v1 rejects both at generate time (`SQLETCH305`)
— loud and incomplete beats speculative API. Multi-argument scopes
are the more likely follow-up (composite tenant keys) and the config
shape in §3 does not preclude them.

## 7. Static enforcement: `sqletch scopecheck`

The residual from §5: raw `gen.Queries` construction must be confined
to wiring code. Two deliverables, both consumer-facing:

1. **A public analyzer package** (working name
   `github.com/moznion/go-sqletch/analysis/scopecheck`), a standard
   `go/analysis.Analyzer` so consumers can add it to their existing
   `go vet -vettool` / multichecker / golangci-lint custom-plugin
   setups. It reports, for each generated package that has scope
   sets: calls to `gen.New` / `gen.WithTx`-on-raw / references to
   `gen.XxxUnscoped`, and imports of the `gen` package itself, from
   any package not allowlisted.
2. **`sqletch scopecheck ./...`**: a convenience wrapper that loads
   the module via `go/packages` and runs the analyzer with the
   allowlist from config:

```yaml
http_scopes_enforcement:
  allow_raw: [yourapp/internal/wiring, yourapp/internal/batch]
```

Notes: the analyzer is type-based (`go/types` object identity), not
lexical — renamed imports and dot-imports do not evade it. It is
soundness-*supporting*, not soundness-*bearing*: §5 steps 1–4 hold
without it; it exists to make step 5's discipline mechanical. The
`x/tools` dependency lives in the analyzer package and `internal/cli`
only. Doc 13's "go/parser only" rule governs the *template input*
path and is untouched.

## 8. Diagnostics and `explain`

New codes in the `3xx` config/codegen band (300–302, 310, 311 in use;
303 reserved by doc 14). Every code added to
`docs/manual/08-diagnostics.md` — `diagnostics.manual_test.go` fails
otherwise.

| Code | Meaning |
| --- | --- |
| `SQLETCH304` | An `http_scopes` declaration is malformed: duplicate scope name, bad identifier, or a predicate name declared by no `@filter-tree` query in the project. |
| `SQLETCH305` | A scope set is inconsistent with its members: member predicates disagree on argument type; a member predicate takes ≠ 1 argument; or a query is matched by more than one scope set (v1 limits, D7). |

Attribution: `SQLETCH304` to `config.Config.Path` (the declaration is
wrong — the doc-14 D4 discipline); `SQLETCH305` to the member query's
`HeaderSpan`, naming the scope and config path in the message. The
scoped layer is offline-derivable (predicate types come from the
committed cache), so both surface in the LSP like any config/codegen
diagnostic.

`sqletch explain` gains a per-scope section: member queries, the
predicate and its Go type, and — once doc 14 lands — which members are
additionally policy-woven. `--format json` includes it for CI
assertions, mirroring doc 14 §6.3.

## 9. Relationship to other mechanisms

- **`@filter-tree!`** — this layer is pure consumer glue over it; the
  per-query construct, its verification story, and its generated API
  are unchanged. Handlers that do not go through HTTP (batch jobs,
  workers) keep using the raw surface, deliberately, from allowlisted
  packages.
- **Doc 14 policy weaving** — complementary, and this doc is the
  natural fulfillment of doc 14's deferred D3(b): a policy-woven
  parameter (a required *argument* of the generated method, per D3(a)
  as amended) can be bound by a scope set exactly like a predicate
  argument — the scoped method passes `s.id` in the tenant position
  alongside the tree. The ambient value
  then lives in the *scoped executor*, not in `Queries` — the hidden
  state that made D3(b) unattractive never enters the generated core
  API. This is deferred until doc 14 (currently on its unmerged
  branch) lands; the config shape reserves nothing and needs nothing.
- **Doc 14 enforcement vs. D2's residual** — a query that touches the
  tenant table but declares no `tenant` predicate is invisible to this
  feature (not a member) but is exactly what `SQLETCH124` catches
  table-wise. The two features close each other's gaps: weaving
  guarantees the SQL is scoped; this doc guarantees the *value* comes
  from the request via one audited path.
- **RLS** — unchanged from doc 14 §9: defense in depth, not replaced.

## 10. Testing plan

Per `CLAUDE.md` working conventions — test-first, every layer,
rejected inputs asserted to their code:

- **`internal/config`**: `http_scopes` decoding, strict unknown keys,
  `SQLETCH304` cases (dup name, unknown predicate, bad identifier).
- **Codegen golden tests**: the scoped package for a fixture project —
  pgx and database/sql flavors, multi-query set, single-query set,
  `SQLETCH305` fixtures (arg-type disagreement, 2-arg predicate,
  overlapping sets). Byte-identical determinism across runs; scope
  sets in declaration order, members sorted.
- **Generated-code behavior via `net/http/httptest`** (compiled
  fixture, like the existing generated-module tests): middleware
  happy path; extractor error → 403 and next *not* invoked; custom
  `onDeny`; `TenantFrom` without middleware → `ok == false`;
  `MustTenantFrom` panic; `WithTx` preserves the bound value.
- **Composition semantics**: `Filter == nil` composes to exactly the
  bare leaf (byte-identical SQL to a hand-built
  `gen.ListOrdersTenant(v)` call); non-nil `Filter` produces
  `And(leaf, filter)` with the leaf's binds first; a `Filter`
  exceeding caps fails with `runtime.ErrTreeTooLarge`.
  `TestComposeConformance` passes unmodified — asserted, because §2
  claims it.
- **Capability surface (compile-time)**: a `go build`-based negative
  fixture asserting `scoped.TenantScope{...}` composite literals and
  `tenantKey` references do not compile outside the package.
- **Analyzer** (`analysis/scopecheck`): `analysistest` cases —
  flagged `gen.New` in a non-allowlisted package, allowlisted package
  clean, renamed-import evasion caught, `Unscoped` reference caught.
- **E2E (devdb, all three dialects)**: an httptest server over a
  seeded two-tenant dataset; requests as tenant A never see tenant
  B's rows through any member query; the extractor-failure path
  reaches the DB zero times (assert via the `OnQuery` hook); an
  allowlisted batch path legitimately reads both tenants.
- **LSP**: `SQLETCH304`/`305` surface through the config-diagnostic
  path; a broken `http_scopes` block degrades, never crash-loops
  (doc 10 discipline).

## 11. Phasing

1. Config surface + codegen (§3, §4) + golden/behavioral tests. The
   feature is already useful: capability API, fail-closed middleware.
2. `explain` scope section (§8) + manual chapter ("Scoped executors
   for HTTP services", including the RLS/defense-in-depth caveat and
   the caps note from §4).
3. Analyzer + `sqletch scopecheck` (§7) — completes the user-stated
   requirement that bypass be reliably excluded.
4. Deferred: doc-14 woven-parameter binding (§9, after doc 14 lands);
   multi-argument scopes and overlapping-set semantics (D7).
