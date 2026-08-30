# Contributing to goga

Read [`docs/CONVENTIONS.md`](docs/CONVENTIONS.md) first. It is short, and every
rule in it is a defect somebody already shipped.

## The loop

Test first. A milestone's tests are written against the port, not the adapter,
so they are the thing that survives the tool being replaced — which is the
whole premise of the framework.

```
make test      # go test ./... with -race and atomic coverage
make lint      # gofmt gate, go vet, golangci-lint
make generate  # build the tools/ generators into ./bin, then go generate ./...
```

`make test` and `make lint` must be green before a pull request is opened, and
both run again in CI.
Integration tests use testcontainers against the real dependency; there are no
recorded-and-replayed fixtures.

## Definition of done

Every milestone from M1 lands **all six** parts in the same change. A milestone
is not done until they are all present:

1. **Implementation** — the package.
2. **Tests** — including the module's instrumentation assertions and its entry
   in `TestEveryModuleIsInstrumented`.
3. **Skill reference** — the module's row in the routing table and in the
   enforcement matrix in [`docs/SKILL.md`](docs/SKILL.md).
4. **Linter** — at least one `goga/lint` rule enforcing this module's
   conventions, written as a custom analyzer where no off-the-shelf rule
   exists, plus a `depguard` entry banning direct use of the dependency the
   module wraps.
5. **CI action** — a composite action wherever the milestone introduces a tool
   that has to run in CI. A milestone that introduces none says so explicitly.
6. **Migration** — a real project adopts the package. This is its own pull
   request in the adopting project's repository, and **the milestone does not
   merge until that pull request is merged.** A package that is perfect and
   unadopted has not demonstrated the only thing it was for.

Splitting functionality from enforcement is not allowed. M0 is the one stated
exception: it *is* the mechanism for parts 3, 4 and 5, and it has no package
for a project to adopt.

## Conventions are enforced, not reviewed

A convention is a compile error, a lint error, or a red build. If you find
yourself writing a review comment that says "goga's rule is X", the rule is
missing a mechanism — that is a goga defect, and the fix is a `goga/lint` rule
or an API shape that leaves no other option, filed and landed rather than
repeated in review.

Conversely: do not add a convention to `docs/CONVENTIONS.md` without the
mechanism that enforces it and the row in `docs/SKILL.md` that names it.

## Branches, commits, pull requests

- Branch from a fresh `origin/main`, named **`issue-<N>`** for the issue it
  closes. No suffixes — the delivery tooling looks the branch up by that exact
  name.
- One issue is one deliverable. Do not split it into sub-issues.
- Pull request body contains **`Closes #<N>`**.
- Keep generated code committed and current: **`make generate`** followed by a
  clean `git diff` is a merge gate, not a suggestion. It is `make generate`, not
  a bare `go generate ./...`: the generators are `tool` directives of the nested
  `tools/` module (they must not be in the root `go.mod` — see
  [`docs/CONVENTIONS.md` §3.5](docs/CONVENTIONS.md)), and `make generate` is what
  builds them into `./bin` and puts them on `PATH`.
