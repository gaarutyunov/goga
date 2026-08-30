# goga

Shared Go framework foundations for the projects in this workspace — gopgql,
epos, codiq, mcp-anything, sysgo and the rest.

goga is not an application framework and not a framework object. It is a set of
independent packages, each one wrapping a tool the projects were otherwise
going to wrap five times: telemetry, HTTP serving, configuration, Postgres,
migrations, MCP, CLI, test infrastructure. A project adopts one package at a
time and keeps its own layout. What goga owns is the *interface* — stable
enough to outlive the tool behind it — plus the telemetry, and the enforcement
that keeps a project from reaching past the interface by accident.

**Status: M0.** The repository is scaffolding. See "What exists today" below
before assuming a module is available.

## Layout

**Flat. No `pkg/`, no `internal/` for goga's own code.** Each module is a
top-level package (`goga/telemetry`, `goga/serve`), and each adapter is a
sub-package of its module (`goga/database/pgxdb`, `goga/mcp/httptransport`).

goga is a **single Go module**. Adapters are sub-packages rather than separate
modules because Go's module graph pruning already gives the property that
matters: a project that does not import an adapter does not compile it and does
not carry its dependency in its build list. The multi-module alternative buys
the same isolation and charges a release tax for it.

`tools/` is the one exception, and it is not an adapter: it is a nested module
with **no Go source and no importable package**, holding nothing but the `tool`
directives for the code generators (`buf`, `wire`, `oapi-codegen`, `goose`,
`sqlc`, `mockgen`). They cannot live in the root `go.mod`, because a `tool`
directive is a module *requirement* and requirements propagate — with them in
the root, a project importing only `goga/config` resolved buf's and sqlc's
dependency trees and had cel-go and `speakeasy-api/jsonpath` forced into its
build list, breaking real consumers. Its own `go.mod` looked clean the whole
time. Run the generators with `make generate` (which builds them into `./bin`
first); do not tidy them back into the root. The full account is
[`docs/CONVENTIONS.md` §3.5](docs/CONVENTIONS.md).

## Go version

`go.mod` says **`go 1.27`**, with no `toolchain` directive.

What sets the floor is `goga/registry`'s generic methods, and nothing else.
The consequence is worth stating plainly, because it is inherited: Go's module
rule is that a module cannot require a lower Go version than a module it
depends on, so **`go >= 1.27` propagates into every project that adopts any
goga package** — including projects that never touch the registry. With the
default `GOTOOLCHAIN=auto` an older toolchain resolves and downloads 1.27
silently; with `GOTOOLCHAIN=local` (hermetic CI, distribution packaging, an
air-gapped builder) it is a hard failure until Go 1.27 is installed.

## How goga is delivered

One package per milestone, one **named** adopter per milestone, and **adoption
is the gate**: a milestone is finished when the adopting project's migration
pull request is merged, not when the package compiles.

| | package | first adopter |
|---|---|---|
| M0 | *(repository scaffolding — not a package)* | goga itself |
| M1 | `goga/telemetry` | gopgql |
| M2 | `goga/serve` | epos |
| M3 | `goga/config` | epos |
| M4 | `goga/database` | gopgql |
| … | see the spec's milestone table | |

Every milestone from M1 lands six things together — implementation, tests, its
row in `docs/SKILL.md`, a `goga/lint` rule, a CI action where it introduces a
tool, and the merged adoption pull request. Splitting functionality from
enforcement is not allowed; M0 is the one exception, because it *is* the
mechanism for the skill, the linter and the actions, and it has no package for
a project to adopt.

## What exists today

- **`goga`** (root package) — `Option[S]` and `Apply`, the two declarations
  every other module needs. A deliberate leaf: it imports nothing but the
  standard library, because every module imports it.
- **`goga/registry`** — name-keyed adapter registration and resolution, as
  generic methods on an injected `*Registry`. Used by every adapter-bearing
  module from M1 on; it has no adopter of its own.
- The `goga/lint` plugin scaffold, the composite actions goga's own CI uses,
  and the `.golangci.yml` / `Makefile` / `.goreleaser.yaml` that double as the
  templates goga ships.

No other module is available yet. `docs/SKILL.md` carries a row per module as
it lands, and an empty routing table means exactly what it says.

## Documentation

- [`docs/CONVENTIONS.md`](docs/CONVENTIONS.md) — the cross-cutting Go
  conventions, the house settings shape, package boundaries, and the Go 1.27
  floor. Read it before writing a goga module or an adapter.
- [`docs/SKILL.md`](docs/SKILL.md) — the routing table and enforcement matrix
  an agent reads to pick a module. A skeleton until M1.
- [`CONTRIBUTING.md`](CONTRIBUTING.md) — the loop, the definition of done, and
  the branch and pull-request conventions.

The merged specification lives in `gaarutyunov/workspace` at
[`openspec/changes/goga-issue-1-framework-foundations/`](https://github.com/gaarutyunov/workspace/tree/main/openspec/changes/goga-issue-1-framework-foundations):
`design.md` carries the decisions (D-numbers), `tasks.md` is milestone-ordered
and authoritative for scope, and `specs/*/spec.md` carry the capability deltas.
