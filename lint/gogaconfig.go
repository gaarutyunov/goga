package lint

import (
	"go/ast"
	"strings"

	"golang.org/x/tools/go/analysis"
)

// osImportPath is the package whose environment readers this rule is about.
// The analyzer matches on the import path rather than on the identifier "os",
// so a project package named os, or an alias bound to something else, is never
// mistaken for it.
const osImportPath = "os"

// configOwnerPackage is the one package inside the module that is ALLOWED to
// read the environment: goga/config, and everything beneath it. Reading the
// environment is precisely what that package does — WithEnv is a source, and a
// rule that fired there would be telling the loader not to load.
//
// Like [serveOwnerPackage] it is matched against the path relative to the
// module prefix, not as a path segment anywhere. The exemption is "the package
// that owns the environment source", which is a position in the tree; a helper
// named configutil, or a nested api/config somewhere else in the module, is
// ordinary project code and gets checked like any other.
const configOwnerPackage = "config"

// mainPackageName is the package whose environment reads are legitimate. See
// [configDoc] for the argument; the short form is that a command is where the
// process meets its environment, and it has to read something before there is
// any configuration to read it from.
const mainPackageName = "main"

// configEnvReaders are the os functions that read the process environment.
// All three are the same defect: a value obtained from any of them arrives
// after Load has already applied its precedence, so nothing in a file and no
// flag can override it.
//
// LookupEnv and Environ are in the set deliberately. A rule that named only
// Getenv would be evaded by a one-character edit — `os.LookupEnv("PORT")`
// discards nothing but the zero-value ambiguity — and Environ is the same
// bypass wholesale. Naming one of three and calling the convention enforced is
// the failure mode D5 is written against.
//
// os.ExpandEnv is deliberately NOT in the set, and the reason is that its
// argument usually comes FROM the configuration rather than instead of it:
// expanding "$HOME/data" out of a loaded path value is not a bypass, and it is
// the common use. A rule that reported it would fire on correct code more often
// than on the defect, which is how a rule gets switched off wholesale. os.Setenv
// and os.Unsetenv are absent for a different reason: they write, and a write is
// not a source Load could have taken precedence over.
var configEnvReaders = map[string]bool{
	"Getenv":    true,
	"LookupEnv": true,
	"Environ":   true,
}

const configDoc = `check that the environment is read through goga/config, not with os.Getenv

goga/config exists to make one property true of every value a program is
configured with: it comes from defaults, then a file, then the environment,
then flags, in that fixed order, and the order is a property of config.Load
rather than of the order options were passed to it (task 3.1). This analyzer
reports the way that guarantee gets bypassed in practice — a package reading
the environment itself with os.Getenv, os.LookupEnv or os.Environ.

Why, rather than what. A direct environment read is not obviously wrong: it
compiles, it returns the right value in the developer's shell, and it is one
line shorter than threading the value through. What it removes is invisible.
A value read here cannot be overridden by the config file or by a flag,
because both of those were applied by Load before this code ran, to a struct
this read does not consult. So the two highest-precedence sources silently stop
working for that one key, and the failure surfaces as "the flag does nothing"
long after the read shipped. Three of the projects this module was written from
had already explained their precedence in prose because koanf has none of its
own; this is the rule that keeps the explanation true.

Scope, and what it deliberately does NOT report. Only packages belonging to
the module named by -module-prefix are checked, so an adopting project points
the same rule at its own module and never sees a dependency's environment
reads.

package main is exempt, and the exemption is the rule rather than a hole in
it. A command is where the process meets its environment: it has to find the
config file before there is any configuration to find it in, and something has
to read the variable that says where to look. Every OTHER package runs after
Load has finished, which is exactly when a direct read starts overriding
things that already resolved. The exemption is keyed on the package NAME, not
on a cmd/ directory, because where a command's directory lives is a layout
choice and this rule is about position in the program rather than in the tree.

goga/config and everything under it are exempt: reading the environment is the
job of the package that owns the environment source, and a rule firing there
would be telling it not to be the thing it is.

_test.go files are exempt, BY DECISION and in deliberate disagreement with
gogaserve, which does not exempt them. The two rules differ in what a test file
can even do wrong. A real listener in a test is a real listener, and httptest
already covers every legitimate case, so what remains there is the defect. A
test binary, by contrast, has no config file and no flags: there is no
precedence for an environment read to take precedence OVER, so the defect this
rule names cannot occur in one. What a test does read the environment for —
skipping unless an integration endpoint is set, finding a container the harness
started — is the correct way to write that test, and goga offers nothing else
to suggest. A rule that fired there would be reported against correct code in
every adopting project, and a rule that cries wolf gets disabled, taking the
cases that mattered with it.

A reference to the function without calling it, and any other os function, are
not reported: only a call to one of the three readers is.`

// configChecker holds the analyzer's one setting; per-instance rather than
// package-level for the same reason as [layoutChecker], [semconvChecker] and
// [serveChecker], so that two analyzers configured with different prefixes
// cannot interfere.
type configChecker struct {
	// modulePrefix is the import-path prefix of the module under analysis.
	modulePrefix string
}

// NewConfigAnalyzer builds the gogaconfig analyzer. An empty modulePrefix means
// [DefaultModulePrefix].
//
// It mirrors the other three constructors exactly, including the
// constructor-argument and -module-prefix-flag pair bound to the same field:
// the argument is how the golangci-lint plugin configures the rule, the flag is
// how singlechecker, go vet and analysistest do, and a test asserts the two
// cannot come apart.
func NewConfigAnalyzer(modulePrefix string) *analysis.Analyzer {
	analyzer, _ := newConfigAnalyzer(modulePrefix)
	return analyzer
}

// newConfigAnalyzer also returns the checker the analyzer and its flag share, so
// a test can assert that configuring the rule through either path reaches the
// same field.
func newConfigAnalyzer(modulePrefix string) (*analysis.Analyzer, *configChecker) {
	if modulePrefix == "" {
		modulePrefix = DefaultModulePrefix
	}
	checker := &configChecker{modulePrefix: modulePrefix}

	analyzer := &analysis.Analyzer{
		Name: "gogaconfig",
		Doc:  configDoc,
		URL:  "https://pkg.go.dev/github.com/gaarutyunov/goga/lint",
		Run:  checker.run,
	}
	analyzer.Flags.StringVar(&checker.modulePrefix, "module-prefix", modulePrefix,
		"import-path prefix of the module to check; packages outside it are ignored")

	return analyzer, checker
}

func (c *configChecker) run(pass *analysis.Pass) (any, error) {
	if pass.Pkg == nil || c.skipPackage(pass.Pkg.Path()) || pass.Pkg.Name() == mainPackageName {
		return nil, nil
	}

	for _, file := range pass.Files {
		if isTestFile(pass, file) {
			continue
		}

		names := importedPackageNames(file, osImportPath, "os")
		if len(names) == 0 {
			// The file cannot name os.Getenv, so nothing in it can be a
			// violation. Skipping here rather than inside the walk keeps this
			// analyzer free on every file that never touches os, which is most
			// of them.
			continue
		}

		ast.Inspect(file, func(node ast.Node) bool {
			if call, ok := node.(*ast.CallExpr); ok {
				c.checkCall(pass, names, call)
			}
			return true
		})
	}

	return nil, nil
}

// checkCall reports call if it invokes one of [configEnvReaders].
func (c *configChecker) checkCall(pass *analysis.Pass, names map[string]bool, call *ast.CallExpr) {
	name, ok := packageSelector(call.Fun, names)
	if !ok || !configEnvReaders[name] {
		return
	}

	pass.Reportf(call.Fun.Pos(),
		"reading the environment with os.%s bypasses config.Load's fixed defaults, file, env, flags precedence: a value read here cannot be overridden by the config file or by a flag; take it from the struct config.Load filled instead",
		name)
}

// skipPackage reports whether pkgPath is outside this rule's remit: another
// module entirely, one of the subtrees in [skippedSegments], or the
// [configOwnerPackage] subtree that owns the environment source.
//
// It deliberately does NOT decide the main-package and _test.go exemptions:
// neither is a property of the import path. A command's package path says
// nothing about whether it is a command, and a test file shares its package's
// path with the non-test files beside it.
func (c *configChecker) skipPackage(pkgPath string) bool {
	if pkgPath == c.modulePrefix {
		// The module root. It is inside the module and gets checked; there are
		// simply no segments below the prefix to inspect.
		return false
	}

	rel := strings.TrimPrefix(pkgPath, c.modulePrefix+"/")
	if rel == pkgPath {
		return true
	}

	if rel == configOwnerPackage || strings.HasPrefix(rel, configOwnerPackage+"/") {
		return true
	}

	for _, segment := range strings.Split(rel, "/") {
		if skippedSegments[segment] {
			return true
		}
	}

	return false
}

// isTestFile reports whether file is a _test.go file.
//
// The name is taken from the FileSet rather than from anything in the AST
// because the AST does not record it: go/ast has no notion of a test file, and
// the build constraint that would express one applies to build tags rather than
// to the _test suffix the go tool keys on.
func isTestFile(pass *analysis.Pass, file *ast.File) bool {
	return strings.HasSuffix(pass.Fset.Position(file.Pos()).Filename, "_test.go")
}
