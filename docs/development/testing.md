# Testing policy

Status: normative for every change to this repository.
Companion to `CLAUDE.md` §"Working conventions" (which this document
expands) and `docs/spec.md` (whose soundness argument the tests exist to
defend). Where a rule here and the spec disagree, the spec wins and
this document is fixed.

sqletch's product is a guarantee: every reachable query shape is
statically verified, generated code never narrows a column the engine
can return NULL in, a policy-designated table is never read unscoped.
A guarantee is only as strong as the evidence that it holds, so the
test suite is not a quality aid here — it is the specification of the
guarantee in executable form. The rules below are written to keep it
that way. "MUST" is mandatory; "SHOULD" requires a written reason in
the PR to deviate.

## 1. Principles

1. **Tests are the specification.** Behavior without a test is
   undefined behavior; a reviewer may treat it as a bug.
2. **Test first, and see it fail.** A new test MUST be observed failing
   against the code it is meant to guard before the change that makes
   it pass — for a bug fix, against the unfixed code. A test that has
   never been red has not been shown to test anything.
3. **Soundness is asymmetric.** sqletch may under-report (stay nullable,
   refuse a query, warn less) but must never over-claim (narrow, accept,
   scope less). Every rule that *grants* something — narrowing,
   acceptance, crediting a conjunct — MUST have a must-stay test that
   pins the refusal side, and must-stay tests are permanent: they are
   never deleted or relaxed to make an implementation easier (review
   counterexamples F1a/F1b are the model).
4. **Oracles must be independent of the code under test.** A check that
   re-reads the output with the same scanner, walker or rule that
   produced it inherits that code's blind spots. Prefer, in order: a
   real engine; a second, structurally different implementation
   (protobuf reflection, brute force); a hand-written expected value.
5. **Determinism.** Identical inputs produce byte-identical outputs, and
   tests assert it where output is pinned (renderings, cache JSON,
   generated Go, composed SQL). Tests themselves are deterministic: no
   unseeded randomness, no wall-clock dependence, no map iteration into
   an assertion.

## 2. What every change must carry

| Change | Required tests |
|---|---|
| New or changed diagnostic / rejection | The rejected input asserted down to its `SQLETCHnnn` (or `SQLETCHLnnn`) code **and** its span; the accepted neighbor that must not fire. Assert codes and spans, not message prose. |
| Rule granting narrowing / acceptance / credit | Positive cases, plus must-stay cases for each way the grant could be wrong (ambiguity, null-extension, guarded fragments, set operations, provenance through derived tables/CTEs/views). One per dialect whose frontend differs. |
| Claim about engine behavior (nullability through attribution, planner index use, driver error shape) | Evidence against the real engine in the devdb suite (`nullability_soundness_devdb_test.go`, `perf_rewrite_devdb_test.go`, `TestQueryTimeout*`, …). A claim that only a unit test supports is an assumption. |
| Hand-written walker or lexical scanner over a third-party AST/token stream | A position-coverage table: one case per node kind / syntactic position the walker must reach, each planting a sentinel and asserting it comes back (with offset and scope marking). When the upstream parser is upgraded, diff its node kinds against the walker's `switch`. |
| Emission change (renderer, fragments, runtime composer) | Both sides of the Compose conformance invariant (`TestComposeConformance`, `FuzzComposeConformance`) plus regenerated examples. Never change one side alone. |
| Cache / layout / oracle bytes | Byte-identity tests and, for oracle backends, a corpus case (`internal/corpus`) re-derived against the real server. |
| Runtime behavior visible to generated code | Unit tests in `runtime/`, alloc tests where a hot path's allocation count is pinned, and coverage in the generated-module devdb run. |
| Bug fix | A regression test that reproduces the bug (red before the fix), with a comment naming the audit/issue and the failure it prevents. |
| Fuzz crasher | The crashing input committed under the package's `testdata/fuzz/<Target>/`. That file is the regression test; do not convert it into something weaker. |

## 3. Testing the tests

A soundness test that passes for the wrong reason is worse than none:
it certifies the hole. For every test guarding a soundness-relevant
branch, the author MUST check that it actually guards it:

- **Mutation check.** Break the guarded line (delete the call, invert
  the condition, widen the predicate) and confirm the test fails;
  restore. State in the PR which mutations were checked. A survivor
  means the test is mis-aimed — fix the test, not the mutation list.
- **Watch for coincidental passes.** Iteration order, a fixture that
  happens to make both outcomes agree, or a catalog where the wrong
  candidate is also non-null all produce tests that survive the very
  bug they name. Example: an ambiguity test whose *last* candidate is
  the null-extended one passes even under a "take the last match"
  bug; order the fixture so the bug would choose the wrong answer.
- **Equivalent mutants are documented, not ignored.** If a mutation
  cannot be killed on some dialect because the input space cannot reach
  it (SQLite has no RIGHT/FULL join), say so and name the suite that
  kills it on the dialects that can.

## 4. Test design rules

- **Seed data is adversarial**: NULL in every nullable column, empty
  results, multibyte text, symmetric data across tenants (so a leaked
  predicate is observable rather than vacuously empty).
- **Per-dialect where the frontend differs.** PostgreSQL (pg_query),
  MySQL (TiDB parser + lexical relation recovery) and SQLite (rqlite +
  hand walkers) disagree in exactly the places bugs live; a behavior
  claimed for all three is tested on all three.
- **White-box tests are allowed for invariants no public path can
  reach** (an encoder collision, a weaver regression that Enforce must
  catch). Fault-inject by mirroring the internal construction exactly,
  and say so in a comment, so the test breaks when the internals move.
- **Never weaken an assertion to go green**, never `t.Skip` a soundness
  test, and never delete a must-stay test. Environment-dependent tests
  use the `devdb` build tag, not skips.
- **Naming.** `Test<Unit>_<Behavior>`; must-stay tests say so
  (`…NeverNarrows`, `…NeverServed`, `…Rejected`). Comments state the
  property and why it matters, not what the code does.

## 5. Coverage

Coverage is a map for finding untested behavior, not a target. There is
no percentage gate; there is a review obligation.

Measure the real number — cross-package and with the engine suites:

```console
PKGS=$(go list ./... | grep -v '/gen$\|/examples\|/spike' | paste -sd, -)
go test -tags devdb -coverpkg=$PKGS -coverprofile=cover.out ./...
go tool cover -func=cover.out | tail -1          # 90.2% at 2026-10-03
```

Per-package `go test -cover` under-reports badly here (74% at the same
commit): rules are exercised through the dialect facades, and the
oracles only under devdb.

**Obligation:** in the soundness-critical packages —
`internal/nullability`, `internal/rules`, `internal/policy`,
`internal/dialect/*` (frontends and native oracle), `internal/ast`,
`internal/codegen`, `runtime` — a block that no test executes MUST be
either tested or justified in a comment as unreachable/defensive. A
zero-execution branch that grants or refuses something is a finding.

**Out of scope** (do not chase): error returns from I/O and connection
failures, defensive bounds checks on values the parser guarantees,
`String()`/debug helpers, and paranoid inputs outside the threat model
(`docs/spec.md` §"Threat model / trust boundary": self-authored config
and template DoS is a known limitation).

## 6. Fuzzing

### 6.1 What must have a fuzz target

1. Every scanner/parser of authored text (`FuzzScan`, `FuzzPattern`).
2. Every differential invariant between two implementations
   (`FuzzComposeConformance`: runtime composer vs reference renderer).
3. Hand-written readers over a third-party AST, against an independent
   reading of the same AST (`FuzzProvenanceFlags`: facade vs protobuf
   reflection).
4. Soundness properties an engine can decide
   (`FuzzPolicyWeaveNoLeak_*`: woven SQL executed against two tenants).
5. Robustness/determinism of offline oracles (`FuzzNativeDescribe`).

Inputs crossing the trust boundary (LSP inbound frames, committed cache
files) SHOULD have robustness targets.

### 6.2 What a target must contain

- **A property beyond "no panic"**, stated in the target's doc comment.
- **Structure-aware generation when raw bytes rarely reach the logic.**
  If a byte-mutated input almost never survives the front end (measured:
  2 % of a 30 s compose corpus nested two constructs), decode the fuzz
  bytes as production choices so every input is a plausible program
  (`leakGen`).
- **A liveness test for every generator**, failing when the share of
  inputs that reach the property drops (`TestPolicyWeaveNoLeak_*Live`).
  A generator that drifted out of what the pipeline accepts passes
  forever.
- **Seeds** for every historic bug in the target's class, in generator
  form where the target has a generator.
- **A demonstrated kill.** When a target is added, re-inject at least
  one known bug of its class and record the time to detection in the
  PR. A target that cannot find a known bug is not yet a target.

### 6.3 Where fuzzing runs

| Lane | Targets | Budget | Corpus |
|---|---|---|---|
| PR smoke (`ci.yml`) | the six plain targets | 30 s each | empty every run |
| Nightly (`fuzz-nightly.yml`) | all eight, server dialects included | 10 min each (dispatchable) | persisted in the Actions cache, compounding |
| Seeds in `go test` | every target | — | `f.Add` + `testdata/fuzz/` |

A nightly crasher fails the run and is uploaded as an artifact; it is
committed (see §2) together with the fix.

## 7. Gates before a change is "done"

All of these, with zero findings:

```console
go test ./...
go test -tags devdb ./...                  # Docker or SQLETCH_TEST_DSN / SQLETCH_TEST_MYSQL_DSN
golangci-lint run --build-tags devdb ./...
staticcheck -tags devdb -checks all $(go list ./... | grep -v '/gen$')
goimports -l .                             # prints nothing
```

plus, when the change touches a fuzzed package, its target for at least
the CI duration locally. A task is never reported complete with a
failing, skipped or missing test.

## 8. Reviewer checklist

- [ ] Each new test was shown red first (bug fixes: against the unfixed code).
- [ ] Rejections asserted to code and span; accepted neighbors present.
- [ ] Every granting rule has must-stay tests; none were relaxed.
- [ ] Mutation checks listed for soundness-relevant tests; survivors explained.
- [ ] Oracles are independent of the code under test.
- [ ] Dialects whose frontends differ are each covered.
- [ ] Zero-execution branches in soundness-critical packages are tested or justified.
- [ ] New generators have a liveness test; new fuzz targets a demonstrated kill.
- [ ] Crashers committed; design docs updated where the tests encode a decision.
