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
| `goga/config` | Needs configuration | `config.Load[T](ctx, opts...)` — declare your own config struct, tag its fields `koanf:`, and pass the sources as options; `Load` applies them in the house order whatever order you list them in. `Config.Value` is the typed struct, `Config.K` the raw `*koanf.Koanf`, and `Cut(path)` a subtree handle for an adapter that decodes its own settings | epos, then skill-test/go-service, then mcp-anything |
| `goga/migrate` | Needs schema migrations | `migrate.New(db)`, then `Up` — hand it the migrations as an embedded `fs.FS` with `WithFS` (there is no default source; `New` fails without one), so the binary carries the schema it was built against; `Up` takes the advisory lock itself, so two replicas booting together take turns rather than race. `Ready` has `serve.WithReadinessCheck`'s signature, so a service whose schema is behind fails `/readyz` instead of erroring once per request | gopgql |
| `goga/mcp` | Needs an MCP server, or an MCP client to call one | `mcp.New(opts...)`, then `mcp.AddTool(s, name, desc, fn)` — the handler is a plain `func(ctx, In) (Out, error)`, so the tool's schema comes from your own Go types and `AddTool` is what attaches the span, the per-tool timeout and the panic recovery; `AddResource` and `AddPrompt` are the same shape. Serve it with `Run(ctx)` for stdio, or mount `Handler()` on `goga/serve` for HTTP — it is an `http.Handler`, so it goes where any other handler goes. `mcp.Connect` is the client half, and `SDK()` on either is the escape hatch for the SDK surface goga does not wrap | gopgql, mcp-anything |

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
| Configuration sources are applied in a **fixed** order — defaults → file → env → flags — and the order is a property of `Load`, not of the order the options were passed | **API shape plus a table test — not a linter, and this row says so rather than implying a rule that does not exist.** `Load` sorts the sources it was given into the house order internally before loading any of them, so `Load[T](ctx, WithFlags(fs), WithEnv("GOGA"), WithFile(p))` and the same three options in any other order produce the same result; there is no option that reorders them and no exported hook that could. What no analyzer can see syntactically — that the order really is the documented one — is pinned by two tests: `TestPrecedenceIsFixed`, a table that sets the same key in every combination of sources and asserts which one won, and `TestPrecedenceIgnoresOptionOrder`, which loads the same sources listed backwards and asserts the same winner. This is the convention a reader is most likely to assume is option-ordered, which is exactly why it is a property of the function rather than of the call | compile (API shape) + test | M3 |
| The environment is read through `goga/config`, never with `os.Getenv` in a library package | `goga/lint`'s `gogaconfig` analyzer, which reports `os.Getenv`, `os.LookupEnv` and `os.Environ` in project code outside `package main`. A value read directly from the environment cannot be overridden by the config file or by a flag, because both were applied by `Load` to a struct the read does not consult — so the two highest-precedence sources silently stop working for that one key. `package main` is exempt: a command has to read something before there is any configuration to read it from. `goga/config` and everything under it are exempt, since the environment source is theirs to be. `_test.go` files are exempt too, in deliberate disagreement with `gogaserve`, which does not exempt them: a test binary has no file and no flags, so there is no precedence for a read to bypass | lint | M3 |
| Configuration is loaded through `goga/config`, never through Viper | `depguard` denies the `github.com/spf13/viper` import path, in test files as well as in the package. The entry itself has been in the shipped `.golangci.yml` since M0, when the file landed with the three house bans; what M3 adds is the module its `desc` sends the reader to, which is what turns the oldest written-down house rule (`.claude/rules/go-cli-koanf.md`) from a paragraph into a mechanism with somewhere to go | lint | M3 |
| A project does not build its own koanf loader beside goga's | `depguard`'s `config-owns-koanf` rule allows `github.com/knadh/koanf/...` only under `goga/config` — the same shape as the OTel SDK rule at M1, and the prefix match covers `koanf/v2`, its providers and its parsers, which is every way a second loader gets built. The restriction costs a project nothing, because the one pattern that genuinely needs koanf's own API — an adapter decoding its own settings subtree — is served by `Config.K` and `Cut(path)`. Merging sources in a second koanf is how precedence gets lost silently: koanf has none of its own, so whichever loader ran last wins | lint | M3 |
| Migrations ship inside the binary, so a deployment cannot drift from the schema it was built against | **The absent default plus tests — not a linter, and this row says so rather than implying a rule that does not exist.** There is no default source of migrations: `New` fails unless it was given one, so it can never quietly read a `./migrations` that happens to sit beside the binary, which is the deployment accident the convention exists to remove. `WithFS` handed an `embed.FS` is the house shape — a binary that carries its own schema cannot be deployed without it — and `WithDir` is the stated exception for a development loop or a tool handed a path; the two write one field, so the later option wins outright and there is no precedence rule to remember. No linter could close the gap without also firing on the development loop, so what holds the shape up is the missing default plus `goga/migrate`'s tests, and this row says which | compile (API shape) + test | M5 |
| Two replicas booting together do not both migrate | **A lock plus tests — not a linter, and this row says so rather than implying a rule that does not exist.** The lock is taken *inside* the run rather than left to the caller, which is the difference between a guarantee and a documented precaution: `Up`, `UpTo` and `Down` all take a session-level Postgres advisory lock before anything is applied and hold it for the whole run, and the pending set is read inside it — reading it outside would compute the work from a state another replica is still changing, so the loser of the race would try to apply what the winner had already committed. The wait is bounded (`WithLockTimeout`, 30s by default) so a stuck lock surfaces as an error instead of hanging the boot, and the release is detached from the caller's cancellation, since a cancelled run is exactly when a release that inherited the cancellation would strand the lock. The mechanism is run-time, so what keeps it there is `goga/migrate`'s concurrency test: two migrators against one container-backed database, asserting each migration was applied exactly once | test | M5 |
| A project does not drive goose beside goga's migrator | `depguard`'s `migrate-owns-goose` rule allows `github.com/pressly/goose/v3/...` only under `goga/migrate` — the same shape as the OTel SDK rule at M1 and the koanf rule at M3, and the prefix match covers `goose/v3/database`, `goose/v3/lock` and the rest of the subpackages, which is every way a second migrator gets built. What going direct costs is invisible at the call site: goose driven by hand takes no advisory lock, emits no span per migration, and answers to none of this module's guarantees — and it still succeeds, most of the time, which is the worst available failure shape for a schema change. The one sanctioned way past it is `Migrator.Provider()`, which hands back the goose provider for the operations this package does not wrap — and its doc comment states what that gives up, namely the lock and the spans. A caller reaching through it is making that trade in the open, which is the difference between an escape hatch and a hole | lint | M5 |
| The migration engine is goose — `golang-migrate` and `rubenv/sql-migrate` are not used | `depguard` denies both import paths outright, in test files as well as in the package. No file scoping, deliberately: this is not a confinement like the goose rule above but a ban. The M2 note reads *a wrapper may not ban what it does not wrap*, and what spared gin and chi there was that a project's handler imports them legitimately — the test is legitimate use, not wrapping. Nothing here imports a second migration engine legitimately, because `goga/migrate` is the schema surface, so there is no correct code for this ban to fire on. It is the row that turns "goose is the house engine" from a sentence in this file into a build failure. `golang-migrate` has been in the shipped `.golangci.yml` since M0, when the file landed with the three house bans; what M5 adds is `rubenv/sql-migrate` beside it, and the module both `desc`s send the reader to | lint | M5 |
| `AddTool` is the only path to the wrapped MCP server, so no tool runs without the span, the per-tool timeout and the panic recovery | **Two mechanisms, and it takes both — neither closes this alone.** The structural half: the SDK server is an unexported field of `mcp.Server`, `New` is the only exported constructor, and nothing else in the package returns one, so a project cannot obtain a server the wrapper did not build. The lint half: that leaves exactly one hole, the `SDK()` accessor, which is a deliberate escape hatch for the SDK surface goga does not wrap — legitimate to reach for, never legitimate to REGISTER through. `goga/lint`'s `gogamcp` analyzer reports precisely that, in both spellings (`s.SDK().AddTool(…)` and `sdkmcp.AddTool(s.SDK(), …)`) and through a local binding. The structural half cannot see past `SDK()`, since by then the server is the SDK's own type with its own callable `Add*` methods; the analyzer cannot make the field reachable in the first place. What a tool registered underneath the wrapper loses is invisible — it answers `tools/call` correctly and simply never emits a span, never stops at the timeout and never survives a panic | compile (API shape) + lint | M6 |
| A project does not build its own MCP server beside goga's, and does not pin its own SDK version | `depguard`'s `mcp-owns-the-protocol-sdk` rule allows `github.com/modelcontextprotocol/go-sdk/...` only under `goga/mcp` — the same shape as the OTel SDK rule at M1, the koanf rule at M3 and the goose rule at M5, and the prefix match covers `go-sdk/auth`, `go-sdk/jsonrpc` and `go-sdk/oauthex` beside `go-sdk/mcp`. The second half of the row is what makes this one different from its three predecessors: it ends a version drift **structurally rather than by asking two repositories to agree**. The two adopting projects were pinned to two different SDK versions when M6 was written — gopgql to v1.6.1, mcp-anything to v1.4.1 — and with the import confined to one package the version becomes a property of goga's `go.mod`, upgraded by upgrading goga. **The known edge, stated because this matrix has no "not enforced" column:** goga/mcp wraps the SDK's server, not its whole vocabulary, and five of its exported signatures still name SDK types — `PromptFunc`'s `[]*sdkmcp.PromptMessage` return, `WithToolAnnotations`, `WithPromptArguments`, `WithClientTransport`, and the `Transport` interface's `Serve`. A project reaching one of those has to import the denied path and takes a `//nolint:depguard` with a reason until goga/mcp re-exports them. Nothing in goga outside `mcp/` imports the SDK, so the rule is green in this repository today | lint | M6 |
| A trace crosses the MCP boundary through `traceparent` in the request's `_meta` | **A house convention plus tests — not a linter, and this row says so rather than implying a rule that does not exist.** MCP defines no header for trace context, so there is nothing standard to conform to: goga puts the W3C `traceparent` into the request's `_meta` map, and it only works because BOTH ends agree to look there. That is why the client half (`mcp.Connect`, `CallTool`) ships in the same milestone as the server: a convention with one end implemented is a convention that silently does nothing, and an adopter pairing goga's server with somebody else's client gets a broken trace rather than an error. What holds it up is `goga/mcp`'s own propagation: the client injects into `params.Meta` on `CallTool`, `ReadResource` and `GetPrompt`, and every server-side handler extracts from `req.Params.Meta` before the span starts. The propagator is **pinned to W3C `TraceContext` rather than read from OTel's global**, which is the detail that makes the convention hold: the global is exactly the thing a program is free to change, and a service configured for B3 would write b3 keys into `_meta` while its peer looked for `traceparent`, breaking every trace at the MCP hop with no error anywhere. A client outside a trace sends no `_meta` at all, and a server that receives none starts a root span, so a goga server talking to a non-goga client degrades rather than fails | test | M6 |

**CI actions.** D18's fifth part is a composite action wherever a milestone
introduces a tool that has to run in CI. A milestone that introduces no such
tool records that here rather than leaving the part unmentioned.

- **M1** — none new. `setup-go`, `go-lint` and `go-test` shipped at M0 and
  cover this milestone.
- **M2** — none new. The same three actions cover it; the milestone introduces
  no tool that has to run in CI.
- **M3** — none new. The same three actions cover it; the milestone introduces
  no tool that has to run in CI. Its linting is a `depguard` rule and a
  `goga/lint` analyzer, both of which run inside the `go-lint` action that
  already exists.
- **M5** — none new. Its linting is two `depguard` rules, which run inside the
  `go-lint` action that already exists, and its container-backed migration
  tests run inside `go-test-integration`, which M4 added. The milestone
  introduces no tool of its own that has to run in CI.
- **M6** — none new. Its linting is one `depguard` rule and one `goga/lint`
  analyzer, both of which run inside the `go-lint` action that already exists,
  and its tests are ordinary `go test`. The milestone introduces no tool of its
  own that has to run in CI.

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
