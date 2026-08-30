# goga skill

The routing reference an agent reads to decide *which goga module to reach for*
and *what enforces its conventions*. This file is a **skeleton**: M0 ships the
headings, and every milestone from M1 fills in its own rows as it lands.

**Nothing is written here for a module that has not landed.** A row describing
a package that does not exist yet is worse than no row: it routes work to an
import path that will not resolve, and it cannot be checked against anything.
If you are here to add a module's row, add it in the same pull request that
adds the module — not before it, and not in a later cleanup pass.

---

## Routing table

Which module to reach for, and who has already adopted it.

| Module | When to reach for it | Entry point | Adopters |
|---|---|---|---|
| `goga/telemetry` | Needs traces, metrics or logs | `telemetry.Setup` once in the binary, then `telemetry.For(module)` wherever a span, a metric or a log record is produced | gopgql, then epos |
| `goga/serve` | Needs an HTTP server | `serve.New(ctx, handler, opts...)`, then `Run(ctx)` — keep your router, it is already an `http.Handler`; a `*gin.Engine`, a `*chi.Mux`, an `*http.ServeMux` and oapi-codegen's generated server all satisfy the port unchanged. `Ops()` reaches the operational mux for a project's own `/debug` style endpoints | epos, then gopgql |

---

## Enforcement matrix

Every convention in `docs/CONVENTIONS.md` and the mechanism that enforces it.
There is no "not enforced" column, by decision: a convention that cannot be
enforced is a goga defect to fix, not a caveat to document.

The **Where** column names the point at which the mechanism fires — compile,
lint, test or merge — and a row whose mechanism is a test says so.

| Convention | Enforced by | Where | Milestone |
|---|---|---|---|
| Attributes come from generated `goga/semconv` constants, never string literals | `goga/lint`'s `gogasemconv` analyzer, which reports a string-literal attribute key where a generated constant exists. The type of `Start`'s variadic argument does not help here: `attribute.String("k", v)` type-checks with a literal key, which is why this needs an analyzer rather than a signature | lint | M1 |
| Every module resolves its instrumentation through OTel's global providers, never by snapshotting a concrete one | API shape: `telemetry.For` is the only way to obtain an `Instrumentation`, and no exported constructor takes a provider, so there is nothing to snapshot. That every module actually *holds* one rests on `TestEveryModuleIsInstrumented` — a test, not the compiler: it asserts the set of modules that called `For` is exactly the shipped package list minus the exempt set, and fails in both directions, so a later milestone can neither skip instrumenting nor quietly add an exemption | compile (API shape) + test | M1 |
| A project does not build its own OTel provider stack beside goga's | `depguard` allows `go.opentelemetry.io/otel/sdk/...` only under `goga/telemetry`. This is the general "everything goes through goga" rule made concrete for this module: the signal APIs stay importable everywhere, the SDK that configures them does not | lint | M1 |
| HTTP is served through `serve.New`, never through a hand-built `http.Server` or `http.ListenAndServe` | `goga/lint`'s `gogaserve` analyzer, which reports an `http.Server` composite literal and a call to `http.ListenAndServe` / `http.ListenAndServeTLS` in project code. Bypassing `serve.New` is what loses the bounded timeouts and the graceful drain, and it does so silently — every timeout field defaults to zero, which `net/http` documents as "no timeout". `goga/serve` and everything under it are exempt, because the stdlib listener *is* an `*http.Server`; nothing in `net/http/httptest` is reported, and that needs no exclusion, since `httptest.NewServer` returns a different type from a different package | lint | M2 |
| Probe and metrics endpoints (`/livez`, `/readyz`, `/healthz`, `/metrics`) are registered outside the `otelhttp` wrapper, and no option can move them inside | **API shape plus tests — not a linter, and this row says so rather than implying a rule that does not exist.** The paths are constants, not options, and `otelhttp.NewHandler` is applied to the application handler alone: the ops mux is a separate `*http.ServeMux`, served either on its own listener (`WithOpsAddr`) or dispatched ahead of the traced handler, so there is no option a project could set that routes a probe through the wrapper. What no analyzer can see syntactically — that the wrapping really landed on the one and not the other — is pinned by three tests over recorded spans: `TestApplicationHandlerIsTracedExactlyOnce`, `TestOperationalEndpointsAreNotTraced`, and `TestEndpointsAddedToOpsAreNotTracedEither`, which extends the guarantee to whatever a project registers through `Ops()` | compile (API shape) + test | M2 |

**CI actions.** D18's fifth part is a composite action wherever a milestone
introduces a tool that has to run in CI. A milestone that introduces no such
tool records that here rather than leaving the part unmentioned.

- **M1** — none new. `setup-go`, `go-lint` and `go-test` shipped at M0 and
  cover this milestone.
- **M2** — none new. The same three actions cover it; the milestone introduces
  no tool that has to run in CI.

**A deliberate absence, stated because the matrix has no "not enforced"
column.** M2 ships **no `depguard` entry banning `gin-gonic/gin` or
`go-chi/chi`**, and the omission is the rule rather than a gap in it. With the
port narrowed to `http.Handler`, goga no longer wraps either router, so a
project's handler imports gin or chi legitimately and a ban would fire on
correct code. The general form — **a wrapper may not ban what it does not
wrap** — leaves the house position on direct dependencies intact everywhere it
can apply: it still holds for every module that genuinely does wrap its tool.

---

## Why the rows arrive one milestone at a time

The definition of done (design D18) forbids splitting functionality from
enforcement. **Every milestone from M1 lands all six parts:**

1. **Implementation** — the package.
2. **Tests** — including the module's instrumentation assertions and its entry
   in `TestEveryModuleIsInstrumented`.
3. **Skill reference** — this module's row in the routing table above, and its
   row in the enforcement matrix.
4. **Linter** — at least one `goga/lint` rule enforcing *this* module's
   conventions, written as a custom analyzer where no off-the-shelf rule
   exists.
5. **CI action** — a composite action wherever the milestone introduces a tool
   that has to run in CI. A milestone that introduces no such tool says so
   rather than leaving the part unmentioned.
6. **Migration** — a real project adopts it, as its own pull request in that
   project's repository, and **the milestone does not merge until that pull
   request is merged.**

A milestone is not done until all six are. The previous plan collected the
linting into one late milestone and the skill into another, so eleven of
fourteen milestones would have shipped a package whose conventions nothing
checked — which is the failure the whole design is written against.

**M0 is the one stated exception**, for two reasons. It *is* the mechanism for
parts 3, 4 and 5 — this file, the `goga/lint` plugin scaffold and the composite
actions — so those parts cannot land before it. And it has no package for a
project to adopt, so part 6 does not apply.
