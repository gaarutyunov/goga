package lint

import (
	"go/constant"
	"go/types"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/attribute"
	"golang.org/x/tools/go/analysis/analysistest"
	"golang.org/x/tools/go/packages"
)

// TestGogaSemconv runs the analyzer over the fixture tree in testdata/src.
//
// Half the packages listed carry no `want` comment at all, and that half is the
// point: analysistest fails on an unexpected diagnostic as well as on a missing
// one, so a rule that had quietly become "report every string literal near an
// attribute" would fail here rather than pass. The clean packages cover each
// way this rule could be wrong — a key semconv does not declare, a value that
// reads like a key, a lookalike package, the registry itself, and another
// module entirely.
func TestGogaSemconv(t *testing.T) {
	t.Parallel()

	results := analysistest.Run(t, analysistest.TestData(), NewSemconvAnalyzer(""),
		// Violating: goga's own code writing covered keys as literals, through
		// the plain import and through an alias.
		"github.com/gaarutyunov/goga/emitter",
		"github.com/gaarutyunov/goga/aliased",
		// Clean: correct usage and every near miss.
		"github.com/gaarutyunov/goga/clean",
		// Clean: the registry itself, which must write the literals.
		"github.com/gaarutyunov/goga/semconv",
		// Clean: a dependency, whose attribute keys are not goga's to police.
		"example.com/dep/attrs",
	)
	require.Len(t, results, 5, "every fixture package should have been loaded and analyzed")
}

// TestSemconvConstantsCoverPackage is the guard that keeps [semconvConstants]
// honest as goga/semconv grows.
//
// A hand-maintained table of what another package declares rots silently: a key
// added to semconv and not added here produces a rule that still passes every
// test, still reports its old keys, and simply stops covering the new one — the
// same failure mode as golangci-lint's unknown-key trap, a check that looks
// healthy and guards less than it claims. So the expected table is DERIVED here
// from the type-checked package rather than written down a second time, and any
// disagreement in either direction fails.
//
// It type-checks the real goga/semconv (not the testdata stub): the stub is a
// fixture for the analyzer, and asserting the table against a fixture would be
// asserting it against a copy of itself.
func TestSemconvConstantsCoverPackage(t *testing.T) {
	t.Parallel()

	scope := loadSemconvScope(t)

	// Every exported function returning a single attribute.KeyValue is a helper
	// a caller can write at the point of use, which is what the diagnostic
	// should name whenever one exists for the key.
	helpers := map[string]bool{}
	for _, name := range scope.Names() {
		object := scope.Lookup(name)
		if !object.Exported() {
			continue
		}
		function, ok := object.(*types.Func)
		if !ok {
			continue
		}
		signature, ok := function.Type().(*types.Signature)
		if !ok || signature.Results().Len() != 1 {
			continue
		}
		if typeName(signature.Results().At(0).Type()) == "go.opentelemetry.io/otel/attribute.KeyValue" {
			helpers[name] = true
		}
	}

	want := map[string]semconvConstant{}
	for _, name := range scope.Names() {
		object := scope.Lookup(name)
		if !object.Exported() {
			continue
		}
		declared, ok := object.(*types.Const)
		if !ok || typeName(declared.Type()) != "go.opentelemetry.io/otel/attribute.Key" {
			continue
		}

		// The helper's name is the constant's without the Key suffix:
		// ServiceNameKey is written as semconv.ServiceName(…). Where no helper
		// exists the constant itself is the thing to write.
		replacement := "semconv." + name
		if base := strings.TrimSuffix(name, "Key"); base != name && helpers[base] {
			replacement = "semconv." + base + "(…)"
		}
		want[name] = semconvConstant{
			key:         attribute.Key(constant.StringVal(declared.Val())),
			replacement: replacement,
		}
	}

	require.NotEmpty(t, want, "goga/semconv must declare at least one attribute key, or this guard proves nothing")
	assert.Equal(t, want, semconvConstants,
		"semconvConstants and goga/semconv have drifted: add the missing keys to the table, or drop the ones semconv no longer declares")
}

// TestSemconvKeysAreDistinct pins the assumption the inverted lookup rests on.
// Two constants carrying the same key would silently make one of their
// suggestions unreachable.
func TestSemconvKeysAreDistinct(t *testing.T) {
	t.Parallel()

	assert.Len(t, semconvKeys, len(semconvConstants),
		"two semconv constants share an attribute key, so one replacement can never be suggested")
}

// TestSemconvSkipPackage pins the rule's scope, including the edges no fixture
// tree can express cheaply: a configured prefix, and the module root.
func TestSemconvSkipPackage(t *testing.T) {
	t.Parallel()

	const otherPrefix = "example.com/adopter"

	tests := map[string]struct {
		prefix  string
		pkgPath string
		want    bool
	}{
		"a goga package is checked":                {pkgPath: "github.com/gaarutyunov/goga/telemetry"},
		"the module root is checked":               {pkgPath: "github.com/gaarutyunov/goga"},
		"a nested goga package is checked":         {pkgPath: "github.com/gaarutyunov/goga/serve/middleware"},
		"the registry itself is skipped":           {pkgPath: "github.com/gaarutyunov/goga/semconv", want: true},
		"a package under the registry is skipped":  {pkgPath: "github.com/gaarutyunov/goga/semconv/registry", want: true},
		"a fixture tree is skipped":                {pkgPath: "github.com/gaarutyunov/goga/lint/testdata/src/x", want: true},
		"a vendored dependency is skipped":         {pkgPath: "github.com/gaarutyunov/goga/vendor/example.com/d", want: true},
		"another module is skipped":                {pkgPath: "go.opentelemetry.io/otel/attribute", want: true},
		"a module sharing the prefix is skipped":   {pkgPath: "github.com/gaarutyunov/goga-extras/telemetry", want: true},
		"a configured prefix moves the rule":       {prefix: otherPrefix, pkgPath: otherPrefix + "/telemetry"},
		"a configured prefix stops it firing here": {prefix: otherPrefix, pkgPath: "github.com/gaarutyunov/goga/telemetry", want: true},
	}

	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			prefix := test.prefix
			if prefix == "" {
				prefix = DefaultModulePrefix
			}
			checker := &semconvChecker{modulePrefix: prefix}

			assert.Equal(t, test.want, checker.skipPackage(test.pkgPath))
		})
	}
}

// TestNewSemconvAnalyzerPrefixWiring covers the two ways the prefix is set: the
// constructor argument the golangci-lint plugin uses, and the -module-prefix
// flag that singlechecker, go vet and analysistest use. Both must reach the
// same field, or configuring the rule through one path silently does nothing
// through the other.
func TestNewSemconvAnalyzerPrefixWiring(t *testing.T) {
	t.Parallel()

	analyzer, checker := newSemconvAnalyzer("")
	assert.Equal(t, DefaultModulePrefix, checker.modulePrefix)

	flag := analyzer.Flags.Lookup("module-prefix")
	require.NotNil(t, flag, "the -module-prefix flag must be registered")
	assert.Equal(t, DefaultModulePrefix, flag.DefValue)

	require.NoError(t, analyzer.Flags.Set("module-prefix", "example.com/adopter"))
	assert.Equal(t, "example.com/adopter", checker.modulePrefix)

	_, explicit := newSemconvAnalyzer("example.com/other")
	assert.Equal(t, "example.com/other", explicit.modulePrefix)
}

// loadSemconvScope type-checks goga/semconv and returns its package scope.
func loadSemconvScope(t *testing.T) *types.Scope {
	t.Helper()

	loaded, err := packages.Load(&packages.Config{
		Mode: packages.NeedName | packages.NeedTypes | packages.NeedImports | packages.NeedDeps,
	}, "github.com/gaarutyunov/goga/semconv")
	require.NoError(t, err)
	require.Len(t, loaded, 1)
	require.Empty(t, loaded[0].Errors, "goga/semconv must type-check for this guard to mean anything")

	return loaded[0].Types.Scope()
}

// typeName renders a type as the fully qualified name this test matches on.
// Comparing against a string rather than a *types.Named obtained from the same
// load keeps the assertion readable and independent of how the package was
// loaded.
func typeName(t types.Type) string {
	named, ok := t.(*types.Named)
	if !ok {
		return ""
	}
	object := named.Obj()
	if object.Pkg() == nil {
		return object.Name()
	}
	return object.Pkg().Path() + "." + object.Name()
}
