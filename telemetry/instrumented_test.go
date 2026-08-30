package telemetry

import (
	"go/ast"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/tools/go/packages"
)

const (
	// modulePath is goga's module path; every package under it is in scope for
	// the instrumentation check.
	modulePath = "github.com/gaarutyunov/goga"
	// telemetryPkgPath is this package, where the call is unqualified.
	telemetryPkgPath = modulePath + "/telemetry"
	// forFuncName is the function an instrumented module must call.
	forFuncName = "For"
	// minJustification is the shortest string accepted as a reason for an
	// exemption. A length floor is crude, but it is what stops the exempt set
	// from growing entries justified by "n/a": a sentence has to be written,
	// and a sentence is something review can disagree with.
	minJustification = 40
)

// exemptFromInstrumentation lists the goga packages that legitimately never
// call [For], each with the reason why.
//
// The justification is not decoration. The failure mode this whole test exists
// to prevent is a later milestone quietly adding an eighth name here, and a
// name with no reason beside it is exactly how that happens. An entry with no
// justification fails the test as surely as an uninstrumented module does.
//
// Entries naming packages that do not exist yet are the modules later
// milestones will add; they are recorded now because the reasoning is settled,
// and they are inert until the package appears.
var exemptFromInstrumentation = map[string]string{
	modulePath: "the root package declares only Option and Apply. It performs no runtime work, and it must import nothing but the standard library, so it cannot import telemetry at all.",

	modulePath + "/semconv": "a registry of attribute keys and metric names. It is the vocabulary telemetry emits; instrumenting the vocabulary in terms of itself is circular and there is no operation to time.",

	modulePath + "/registry": "telemetry resolves its exporters through the registry, so registry importing telemetry would close an import cycle. The registry's package doc records the same constraint.",

	modulePath + "/lint": "static-analysis passes that run inside golangci-lint, in a linter process rather than in a goga program. There is no request to trace and no meter provider to record into.",

	modulePath + "/di": "wire provider sets: declarations consumed by a code generator, whose generated wiring calls the constructors that are themselves instrumented (M6, not present yet).",

	modulePath + "/app": "the composition root. It starts the surfaces and shuts them down, and each of those is instrumented by its own module; a span around the whole process lifetime would duplicate them (M8, not present yet).",

	modulePath + "/gogatest": "test doubles and the conformance suites adapters run against. Instrumenting them would emit telemetry from the assertions rather than from the code under test (not present yet).",
}

// TestEveryModuleIsInstrumented is checklist item 1.11: every goga module that
// does runtime work calls [For], and every module that does not is exempt for a
// written reason.
//
// The module list is computed from the packages that are actually in the
// repository, never hand-maintained. A hand-written list is a list that stops
// matching the repository the first time somebody adds a package and forgets,
// which is precisely the case the test is supposed to catch.
func TestEveryModuleIsInstrumented(t *testing.T) {
	callsFor := loadModuleInstrumentation(t)

	require.Contains(t, callsFor, telemetryPkgPath,
		"the package walk found goga's own packages")
	assert.True(t, callsFor[telemetryPkgPath],
		"goga/telemetry counts as instrumented: Telemetry.Shutdown opens a span, "+
			"which is the one operation in this package that runs after the providers are installed")

	violations := instrumentationViolations(callsFor, exemptFromInstrumentation)

	assert.Empty(t, violations, "every goga module is instrumented or exempt with a reason")
}

// TestInstrumentationCheckFailsInBothDirections proves the check above is not
// vacuous. A guard that can only fail one way lets the other way through
// forever.
func TestInstrumentationCheckFailsInBothDirections(t *testing.T) {
	const justified = "a written reason long enough to be an actual sentence about why."

	t.Run("a module that does runtime work and never calls For", func(t *testing.T) {
		violations := instrumentationViolations(
			map[string]bool{modulePath + "/serve": false},
			map[string]string{},
		)

		require.Len(t, violations, 1)
		assert.Contains(t, violations[0], modulePath+"/serve")
		assert.Contains(t, violations[0], "never calls telemetry.For")
	})

	t.Run("a module added to the exempt set without justification", func(t *testing.T) {
		violations := instrumentationViolations(
			map[string]bool{modulePath + "/serve": false},
			map[string]string{modulePath + "/serve": "  "},
		)

		require.Len(t, violations, 1)
		assert.Contains(t, violations[0], "without a justification")
	})

	t.Run("a justification too short to be a reason", func(t *testing.T) {
		violations := instrumentationViolations(
			map[string]bool{modulePath + "/serve": false},
			map[string]string{modulePath + "/serve": "n/a"},
		)

		require.Len(t, violations, 1)
		assert.Contains(t, violations[0], "without a justification")
	})

	t.Run("an exempt module that turns out to be instrumented", func(t *testing.T) {
		violations := instrumentationViolations(
			map[string]bool{modulePath + "/serve": true},
			map[string]string{modulePath + "/serve": justified},
		)

		require.Len(t, violations, 1)
		assert.Contains(t, violations[0], "remove it from the exempt set")
	})

	t.Run("an instrumented module and a justified exemption both pass", func(t *testing.T) {
		violations := instrumentationViolations(
			map[string]bool{
				modulePath + "/serve":   true,
				modulePath + "/semconv": false,
			},
			map[string]string{modulePath + "/semconv": justified},
		)

		assert.Empty(t, violations)
	})
}

// instrumentationViolations is the whole rule, separated from the package walk
// so that it can be exercised with inputs a repository cannot be put into.
func instrumentationViolations(callsFor map[string]bool, exempt map[string]string) []string {
	var violations []string

	for path, instrumented := range callsFor {
		_, isExempt := exempt[path]
		switch {
		case isExempt && instrumented:
			violations = append(violations, path+
				" is in the exempt set but calls telemetry.For; remove it from the exempt set")
		case !isExempt && !instrumented:
			violations = append(violations, path+
				" performs runtime work and never calls telemetry.For;"+
				" instrument it, or add it to the exempt set with a reason")
		}
	}

	for path, justification := range exempt {
		if len(strings.TrimSpace(justification)) < minJustification {
			violations = append(violations, path+
				" is exempt from instrumentation without a justification saying why")
		}
	}

	slices.Sort(violations)
	return violations
}

// loadModuleInstrumentation walks every package in the goga module and reports,
// per package path, whether it calls [For].
func loadModuleInstrumentation(t *testing.T) map[string]bool {
	t.Helper()

	// Syntax only, with no type checking: resolving telemetry.For by import
	// path is exact enough — revive bans dot imports, so a call is either
	// qualified by the file's own import name or unqualified inside this
	// package — and type-checking goga's whole dependency tree from source
	// would make a guard that runs on every CI cost a minute.
	cfg := &packages.Config{
		Mode:  packages.NeedName | packages.NeedFiles | packages.NeedCompiledGoFiles | packages.NeedSyntax,
		Tests: false,
	}
	pkgs, err := packages.Load(cfg, modulePath+"/...")
	require.NoError(t, err)
	require.NotEmpty(t, pkgs, "the package walk found no packages")

	callsFor := make(map[string]bool, len(pkgs))
	for _, p := range pkgs {
		require.Empty(t, p.Errors, "loading %s", p.PkgPath)
		callsFor[p.PkgPath] = packageCallsFor(p)
	}
	return callsFor
}

// packageCallsFor reports whether any non-test file of p calls telemetry.For.
func packageCallsFor(p *packages.Package) bool {
	for _, file := range p.Syntax {
		local, imported := telemetryImportName(file)

		found := false
		ast.Inspect(file, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			switch fun := call.Fun.(type) {
			case *ast.SelectorExpr:
				if !imported || fun.Sel.Name != forFuncName {
					return true
				}
				if x, ok := fun.X.(*ast.Ident); ok && x.Name == local {
					found = true
				}
			case *ast.Ident:
				// Inside goga/telemetry itself the call is unqualified.
				if p.PkgPath == telemetryPkgPath && fun.Name == forFuncName {
					found = true
				}
			}
			return true
		})
		if found {
			return true
		}
	}
	return false
}

// telemetryImportName returns the name goga/telemetry is bound to in file, and
// whether it is imported at all.
func telemetryImportName(file *ast.File) (name string, ok bool) {
	for _, imp := range file.Imports {
		if imp.Path == nil || strings.Trim(imp.Path.Value, `"`) != telemetryPkgPath {
			continue
		}
		if imp.Name != nil {
			return imp.Name.Name, true
		}
		return "telemetry", true
	}
	return "", false
}
