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

_no modules have landed yet — M0 is scaffolding_

---

## Enforcement matrix

Every convention in `docs/CONVENTIONS.md` and the mechanism that enforces it.
There is no "not enforced" column, by decision: a convention that cannot be
enforced is a goga defect to fix, not a caveat to document.

| Convention | Enforced by | Where | Milestone |
|---|---|---|---|

_no modules have landed yet — M0 is scaffolding_

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
