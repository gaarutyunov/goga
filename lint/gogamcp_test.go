package lint

import (
	"go/types"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/tools/go/analysis/analysistest"
	"golang.org/x/tools/go/packages"
)

// TestGogaMCP runs the analyzer over the fixture tree in testdata/src.
//
// Most of what the fixtures contain carries no `want` comment, and that
// majority is the point: analysistest fails on an unexpected diagnostic as well
// as on a missing one, so a rule that had quietly become "report every call on
// SDK()" would fail here rather than pass. The clean packages cover each way
// this rule could be wrong — the escape hatch used for what it is for, the
// wrapper's own registration, the Add… neighbours that are not registrations, a
// lookalike SDK() on an unrelated type, the package that owns the wrapped
// server, and another module entirely.
//
// The two lookalikes are the pair that decides how the owner exemption is
// written, exactly as in gogaserve's fixtures. mcputil must be reported because
// its name merely starts with the owner's; goga/rpc/mcp must be reported
// because its last segment is "mcp" but its position is not. A segment match
// would silence both.
func TestGogaMCP(t *testing.T) {
	t.Parallel()

	analysistest.Run(t, analysistest.TestData(), NewMCPAnalyzer(""),
		// Violating: goga's own code registering underneath the wrapper, in
		// every shape of the trigger set, through an alias, through a local
		// binding, and in a test file.
		"github.com/gaarutyunov/goga/mcpbypass",
		// Violating: the two packages the exemption must not reach.
		"github.com/gaarutyunov/goga/mcputil",
		"github.com/gaarutyunov/goga/rpc/mcp",
		// Clean: the package that owns the wrapped server.
		"github.com/gaarutyunov/goga/mcp",
		// Clean: correct usage and every near miss.
		"github.com/gaarutyunov/goga/mcpclean",
		// Clean: a dependency, whose registrations are not goga's to police.
		"example.com/dep/mcpdep",
	)
}

// TestMCPTriggersExistInSDK is the guard that keeps the rule's trigger set
// honest.
//
// Every trigger is a string matched against a selector, so a typo — or an
// upstream rename — produces a rule that still compiles, still passes every
// fixture it was given, and simply stops covering the case it names. That is
// the same failure mode as golangci-lint's unknown-key trap: a check that looks
// healthy and guards less than it claims. It matters more here than for the
// other rules in this package, because the trigger set is an upstream API
// rather than the standard library: the SDK is pre-1.0 in spirit, two adopting
// projects were on two different versions when this rule was written, and a
// renamed registration method is exactly the kind of change it would ship.
//
// The fixture stub under testdata cannot stand in for this. It is a hand-written
// file that declares whatever the fixtures need, so it agrees with the trigger
// set by construction; only the real package can disagree with it.
func TestMCPTriggersExistInSDK(t *testing.T) {
	t.Parallel()

	scope := loadPackageScope(t, mcpSDKImportPath)

	object := scope.Lookup("Server")
	require.NotNil(t, object, "the SDK no longer declares Server, which is the type SDK() hands back")
	named, ok := object.Type().(*types.Named)
	require.True(t, ok, "the SDK's Server is no longer a named type")

	methods := make(map[string]bool, named.NumMethods())
	for i := range named.NumMethods() {
		methods[named.Method(i).Name()] = true
	}

	for name := range mcpRegistrationMethods {
		assert.True(t, methods[name],
			"the SDK's Server no longer declares %s; the rule silently stopped covering it", name)
	}

	for name, position := range mcpRegistrationFuncs {
		object := scope.Lookup(name)
		require.NotNil(t, object, "the SDK no longer declares %s; the rule silently stopped covering it", name)
		function, ok := object.(*types.Func)
		require.True(t, ok, "the SDK's %s is no longer a function", name)
		signature, ok := function.Type().(*types.Signature)
		require.True(t, ok)

		require.Greater(t, signature.Params().Len(), position,
			"the SDK's %s no longer takes an argument at position %d", name, position)
		pointer, ok := signature.Params().At(position).Type().(*types.Pointer)
		require.True(t, ok,
			"the SDK's %s no longer takes the server at position %d, so the rule is checking the wrong argument", name, position)
		server, ok := pointer.Elem().(*types.Named)
		require.True(t, ok)
		assert.Equal(t, "Server", server.Obj().Name(),
			"the SDK's %s no longer takes a *Server at position %d", name, position)
	}
}

// TestMCPEscapeHatchExistsInGogaMCP is the other half of the same guard, and
// the half no other analyzer in this package needs.
//
// Every rule here matches an UPSTREAM name; this one also matches one of goga's
// own — the SDK() accessor. Matching it by name is what lets the rule work
// without type information, and the price is that renaming the accessor would
// leave a rule that fires on nothing while looking perfectly healthy. So the
// name is resolved against the real goga/mcp, and the method's result is
// checked to still be the SDK's server rather than merely something.
func TestMCPEscapeHatchExistsInGogaMCP(t *testing.T) {
	t.Parallel()

	scope := loadPackageScope(t, DefaultModulePrefix+"/"+mcpOwnerPackage)

	object := scope.Lookup("Server")
	require.NotNil(t, object, "goga/mcp no longer declares Server")
	named, ok := object.Type().(*types.Named)
	require.True(t, ok, "goga/mcp's Server is no longer a named type")

	var accessor *types.Func
	for i := range named.NumMethods() {
		if named.Method(i).Name() == mcpEscapeHatchMethod {
			accessor = named.Method(i)
		}
	}
	require.NotNil(t, accessor,
		"goga/mcp's Server no longer declares %s; the rule matches that name and would now fire on nothing",
		mcpEscapeHatchMethod)

	signature, ok := accessor.Type().(*types.Signature)
	require.True(t, ok)
	assert.Zero(t, signature.Params().Len(),
		"%s now takes arguments; the rule matches a zero-argument call", mcpEscapeHatchMethod)
	require.Equal(t, 1, signature.Results().Len())

	pointer, ok := signature.Results().At(0).Type().(*types.Pointer)
	require.True(t, ok, "%s no longer returns a pointer to the SDK's server", mcpEscapeHatchMethod)
	named, ok = pointer.Elem().(*types.Named)
	require.True(t, ok)
	assert.Equal(t, mcpSDKImportPath, named.Obj().Pkg().Path(),
		"%s no longer returns the SDK's own server, so the escape hatch this rule polices is somewhere else now",
		mcpEscapeHatchMethod)
}

// TestMCPSkipPackage pins the rule's scope, including the edges no fixture tree
// can express cheaply: a configured prefix, the module root, and the subtrees
// the go tool never loads.
func TestMCPSkipPackage(t *testing.T) {
	t.Parallel()

	const otherPrefix = "example.com/adopter"

	tests := map[string]struct {
		prefix  string
		pkgPath string
		want    bool
	}{
		"a goga package is checked":                 {pkgPath: "github.com/gaarutyunov/goga/registry"},
		"the module root is checked":                {pkgPath: "github.com/gaarutyunov/goga"},
		"the owner package is skipped":              {pkgPath: "github.com/gaarutyunov/goga/mcp", want: true},
		"a sub-package of the owner is skipped":     {pkgPath: "github.com/gaarutyunov/goga/mcp/mcptest", want: true},
		"a package merely named like it is checked": {pkgPath: "github.com/gaarutyunov/goga/mcputil"},
		"an mcp package elsewhere is checked":       {pkgPath: "github.com/gaarutyunov/goga/rpc/mcp"},
		"a fixture tree is skipped":                 {pkgPath: "github.com/gaarutyunov/goga/lint/testdata/src/x", want: true},
		"a vendored dependency is skipped":          {pkgPath: "github.com/gaarutyunov/goga/vendor/example.com/d", want: true},
		"another module is skipped":                 {pkgPath: mcpSDKImportPath, want: true},
		"a module sharing the prefix is skipped":    {pkgPath: "github.com/gaarutyunov/goga-extras/mcp", want: true},
		"a configured prefix moves the rule":        {prefix: otherPrefix, pkgPath: otherPrefix + "/api"},
		"a configured prefix moves the exemption":   {prefix: otherPrefix, pkgPath: otherPrefix + "/mcp", want: true},
		"a configured prefix stops it firing here":  {prefix: otherPrefix, pkgPath: "github.com/gaarutyunov/goga/api", want: true},
		"goga's own mcp is checked under a prefix":  {prefix: otherPrefix, pkgPath: "github.com/gaarutyunov/goga/mcp", want: true},
	}

	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			prefix := test.prefix
			if prefix == "" {
				prefix = DefaultModulePrefix
			}
			checker := &mcpChecker{modulePrefix: prefix}

			assert.Equal(t, test.want, checker.skipPackage(test.pkgPath))
		})
	}
}

// TestNewMCPAnalyzerPrefixWiring covers the two ways the prefix is set: the
// constructor argument the golangci-lint plugin uses, and the -module-prefix
// flag that singlechecker, go vet and analysistest use. Both must reach the
// same field, or configuring the rule through one path silently does nothing
// through the other.
func TestNewMCPAnalyzerPrefixWiring(t *testing.T) {
	t.Parallel()

	analyzer, checker := newMCPAnalyzer("")
	assert.Equal(t, DefaultModulePrefix, checker.modulePrefix)

	flag := analyzer.Flags.Lookup("module-prefix")
	require.NotNil(t, flag, "the -module-prefix flag must be registered")
	assert.Equal(t, DefaultModulePrefix, flag.DefValue)

	require.NoError(t, analyzer.Flags.Set("module-prefix", "example.com/adopter"))
	assert.Equal(t, "example.com/adopter", checker.modulePrefix)

	_, explicit := newMCPAnalyzer("example.com/other")
	assert.Equal(t, "example.com/other", explicit.modulePrefix)
}

// loadPackageScope type-checks one package and returns its scope. It is
// [loadNetHTTPScope] generalised, for the two guards above that resolve names
// against a package outside the standard library.
func loadPackageScope(t *testing.T, importPath string) *types.Scope {
	t.Helper()

	loaded, err := packages.Load(&packages.Config{
		Mode: packages.NeedName | packages.NeedTypes | packages.NeedImports | packages.NeedDeps,
	}, importPath)
	require.NoError(t, err)
	require.Len(t, loaded, 1)
	require.Empty(t, loaded[0].Errors, "%s must type-check for this guard to mean anything", importPath)

	return loaded[0].Types.Scope()
}
